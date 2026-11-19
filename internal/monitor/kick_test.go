package monitor

import (
	"context"
	"testing"
	"time"
)

// counted is a check that counts how often it runs, and reports what the test last set.
type counted struct {
	scripted
	runs int
}

func (c *counted) Run(ctx context.Context) Result {
	c.mu.Lock()
	c.runs++
	c.mu.Unlock()
	return c.scripted.Run(ctx)
}

func (c *counted) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs
}

func newCounted(name string, interval time.Duration) *counted {
	c := &counted{scripted: scripted{name: name, interval: interval, failures: 1}}
	c.res = Result{State: OK, Detail: "fine"}
	return c
}

func TestKickRunsTheNamedChecksEarlyAndLeavesTheRestAlone(t *testing.T) {
	r := newRig(t, openStore(t))
	down := newCounted("down:app/web", 30*time.Second)
	probe := newCounted("probe:site", 30*time.Second)
	r.checks = []Check{down, probe}
	r.step(0)
	if down.count() != 1 || probe.count() != 1 {
		t.Fatalf("first run: %d %d", down.count(), probe.count())
	}
	down.set(Critical, "0 of 1 replicas running")
	r.step(2 * time.Second)
	if down.count() != 1 {
		t.Fatal("the check ran again before its interval without a kick")
	}
	r.m.Kick("down:")
	r.step(0)
	if down.count() != 2 {
		t.Fatalf("the kicked check ran %d times, want 2", down.count())
	}
	if probe.count() != 1 {
		t.Fatalf("a check that was not named ran early: %d", probe.count())
	}
	// The kick wakes the check; what it finds is decided as before.
	if got := r.stored("down:app/web"); got.State != Critical || got.Detail != "0 of 1 replicas running" {
		t.Fatalf("stored %+v", got)
	}
}

func TestKickReadsTheChecksAgainBeforeRunningThem(t *testing.T) {
	r := newRig(t, openStore(t))
	rebuilds := 0
	r.m.Checks = func(context.Context) []Check { rebuilds++; return r.checks }
	down := newCounted("down:app/web", 30*time.Second)
	r.checks = []Check{down}
	r.step(0)
	before := rebuilds
	r.m.Kick("down:")
	r.step(0)
	if rebuilds != before+1 {
		t.Fatalf("the checks were rebuilt %d times for one kick, want 1", rebuilds-before)
	}
	// The early rebuild stands in for the next periodic one.
	r.step(time.Second)
	if rebuilds != before+1 {
		t.Fatalf("a periodic rebuild came right after the early one: %d", rebuilds-before)
	}
}

func TestKickWithoutANameWakesEveryCheck(t *testing.T) {
	r := newRig(t, openStore(t))
	a, b := newCounted("down:a", time.Hour), newCounted("probe:b", time.Hour)
	r.checks = []Check{a, b}
	r.step(0)
	r.m.Kick()
	r.step(0)
	if a.count() != 2 || b.count() != 2 {
		t.Fatalf("%d %d", a.count(), b.count())
	}
}

func TestKicksCloserThanTheGapAreMergedAndRunOnce(t *testing.T) {
	r := newRig(t, openStore(t))
	down := newCounted("down:app/web", time.Hour)
	drift := newCounted("drift:app", time.Hour)
	r.checks = []Check{down, drift}
	r.step(0)
	r.m.Kick("down:")
	r.step(0)
	if down.count() != 2 {
		t.Fatal("the first kick did not run at once")
	}
	r.m.Kick("down:")
	r.m.Kick("drift:")
	r.step(time.Second)
	if down.count() != 2 || drift.count() != 1 {
		t.Fatalf("kicks inside the gap ran: %d %d", down.count(), drift.count())
	}
	r.step(kickGap)
	if down.count() != 3 || drift.count() != 2 {
		t.Fatalf("the merged kicks ran %d and %d times, want one more run each", down.count(), drift.count())
	}
}

func TestAKickIsRunAgainOnceAfterTheGap(t *testing.T) {
	// Swarm records a failed task a moment after the container dies, so the read that the event started can be a step ahead
	// of it. One more read after the gap closes that.
	r := newRig(t, openStore(t))
	down := newCounted("down:app/web", time.Hour)
	r.checks = []Check{down}
	r.step(0)
	r.m.Kick("down:")
	r.step(0)
	r.step(kickGap)
	if down.count() != 3 {
		t.Fatalf("the check ran %d times, want the first run, the kick and one follow-up", down.count())
	}
	r.step(kickGap)
	r.step(kickGap)
	if down.count() != 3 {
		t.Fatalf("the follow-up repeated: %d runs", down.count())
	}
}

func TestKickedCheckThatIsRunningIsNotStartedTwice(t *testing.T) {
	r := newRig(t, openStore(t))
	down := newCounted("down:app/web", time.Hour)
	r.checks = []Check{down}
	r.m.rebuild(context.Background(), r.now)
	var started []func()
	start := func(run func()) { started = append(started, run) }
	r.m.tick(context.Background(), r.now, start)
	r.m.Kick("down:")
	r.m.tick(context.Background(), r.now.Add(time.Second), start)
	if len(started) != 1 {
		t.Fatalf("%d runs started while the first one has not ended", len(started))
	}
}

func TestKickWakesTheRunLoop(t *testing.T) {
	store := openStore(t)
	r := newRig(t, store)
	down := newCounted("down:app/web", time.Hour)
	r.checks = []Check{down}
	r.m.Now = nil // the real clock, so the loop's own ticker is real
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.m.Run(ctx) }()
	defer func() { cancel(); <-done }()
	waitFor := func(want int) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for down.count() < want {
			select {
			case <-deadline:
				t.Fatalf("the check ran %d times, want %d", down.count(), want)
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	waitFor(1)
	start := time.Now()
	r.m.Kick("down:")
	waitFor(2)
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("the kick took %v to run the check, the loop ticks once a second", took)
	}
}
