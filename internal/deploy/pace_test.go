package deploy

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"berthy/internal/engine"
)

// eventedHealth is a health reader that also hears Docker's events, like the engine does. The test flips it healthy, and
// sends the events it wants the wait to hear.
type eventedHealth struct {
	healthy atomic.Bool
	polls   atomic.Int32
	live    atomic.Bool
	events  chan engine.Event
	mu      sync.Mutex
	match   func(engine.Event) bool
	// onPoll, when set before the wait starts, runs inside poll n, before the poll returns: what it sends is queued before
	// the wait that follows the poll can read it.
	onPoll func(n int32)
}

func newEventedHealth() *eventedHealth {
	e := &eventedHealth{events: make(chan engine.Event, 16)}
	e.live.Store(true)
	return e
}

func (e *eventedHealth) ServiceTasks(context.Context, string) ([]engine.ServiceTask, error) {
	n := e.polls.Add(1)
	if e.onPoll != nil {
		e.onPoll(n)
	}
	state := "starting"
	if e.healthy.Load() {
		state = "running"
	}
	return []engine.ServiceTask{{ID: "t", ServiceID: "s", ContainerID: "c", State: state, DesiredState: "running", Slot: 1, Labels: map[string]string{"berthy.operation": "op"}}}, nil
}

func (e *eventedHealth) InspectContainerHealth(context.Context, string) (string, error) {
	if e.healthy.Load() {
		return "healthy", nil
	}
	return "starting", nil
}

// Watch gives the wait the events the test sends that its match accepts.
func (e *eventedHealth) Watch(match func(engine.Event) bool) (<-chan engine.Event, func()) {
	e.mu.Lock()
	e.match = match
	e.mu.Unlock()
	return e.events, func() {}
}

func (e *eventedHealth) EventsLive() bool { return e.live.Load() }

// send gives the wait an event, if the match it subscribed with takes it.
func (e *eventedHealth) send(ev engine.Event) {
	e.mu.Lock()
	match := e.match
	e.mu.Unlock()
	if match == nil || match(ev) {
		e.events <- ev
	}
}

func healthStatus(service string) engine.Event {
	return engine.Event{Type: "container", Action: "health_status", Detail: "healthy", ID: "c", Attributes: map[string]string{"com.docker.swarm.service.id": service}}
}

// paceWith sets the intervals of the polls for one test.
func paceWith(t *testing.T, fast, slow, settle time.Duration) {
	t.Helper()
	f, s, st := pollFast, pollSlow, pollSettle
	pollFast, pollSlow, pollSettle = fast, slow, settle
	t.Cleanup(func() { pollFast, pollSlow, pollSettle = f, s, st })
}

// goWait runs wait on its own goroutine and returns its result channel. The goroutine is cancelled and has ended before
// the test's earlier cleanups run, so paceWith restores the intervals only once nothing reads them.
func goWait(t *testing.T, ctx context.Context, wait func(context.Context) error) chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- wait(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-exited
	})
	return done
}

func runWait(t *testing.T, ctx context.Context, e *eventedHealth) chan error {
	t.Helper()
	return goWait(t, ctx, func(ctx context.Context) error { return waitHealthy(ctx, e, "s", "op", 1, startBudget{}, 0) })
}

func within(t *testing.T, done chan error, d time.Duration, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("%s: the wait did not end within %v", what, d)
		return nil
	}
}

func waitForPolls(t *testing.T, e *eventedHealth, n int32) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for e.polls.Load() < n {
		select {
		case <-deadline:
			t.Fatalf("polled %d times, want %d", e.polls.Load(), n)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestHealthWaitEndsAtTheHealthStatusEventNotAtTheNextPoll(t *testing.T) {
	paceWith(t, time.Hour, time.Hour, 0)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := newEventedHealth()
	done := runWait(t, ctx, e)
	waitForPolls(t, e, 1)
	start := time.Now()
	e.healthy.Store(true)
	e.send(healthStatus("s"))
	if err := within(t, done, 2*time.Second, "health_status"); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("took %v after the event", took)
	}
}

func TestHealthWaitFallsBackOnASlowPollWhenNoEventComes(t *testing.T) {
	paceWith(t, time.Hour, 30*time.Millisecond, 0)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := newEventedHealth()
	done := runWait(t, ctx, e)
	waitForPolls(t, e, 1)
	e.healthy.Store(true)
	if err := within(t, done, 2*time.Second, "no event"); err != nil {
		t.Fatal(err)
	}
}

func TestHealthWaitPollsOnlyEveryHalfSecondWithoutAnEventFeed(t *testing.T) {
	// A feed that is down, like an engine that has none, leaves the wait on the old pace: the slow poll is for a feed that
	// can be trusted to wake it.
	paceWith(t, 20*time.Millisecond, time.Hour, 0)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := newEventedHealth()
	e.live.Store(false)
	done := runWait(t, ctx, e)
	waitForPolls(t, e, 1)
	e.healthy.Store(true)
	if err := within(t, done, 2*time.Second, "feed down"); err != nil {
		t.Fatal(err)
	}
}

func TestHealthWaitSurvivesTheFeedDroppingMidWait(t *testing.T) {
	// A wait that is sleeping on the slow poll when the feed drops ends at that poll at the latest: the outcome is the same.
	paceWith(t, 20*time.Millisecond, 100*time.Millisecond, 0)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := newEventedHealth()
	done := runWait(t, ctx, e)
	waitForPolls(t, e, 1)
	e.live.Store(false) // the stream is killed; no event will come
	time.Sleep(30 * time.Millisecond)
	e.healthy.Store(true)
	if err := within(t, done, 2*time.Second, "feed dropped"); err != nil {
		t.Fatalf("the outcome changed with the feed: %v", err)
	}
}

func TestHealthWaitPollsFastForAWhileAfterAnEvent(t *testing.T) {
	// The health event can come before Swarm marks the task running, so the poll that the event starts may not be the last.
	paceWith(t, 20*time.Millisecond, time.Hour, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := newEventedHealth()
	done := runWait(t, ctx, e)
	waitForPolls(t, e, 1)
	e.send(healthStatus("s")) // the poll finds nothing healthy yet
	waitForPolls(t, e, 2)
	e.healthy.Store(true) // no further event
	if err := within(t, done, 2*time.Second, "settle"); err != nil {
		t.Fatal(err)
	}
}

func TestHealthWaitIgnoresEventsOfOtherServices(t *testing.T) {
	paceWith(t, time.Hour, time.Hour, 0)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := newEventedHealth()
	done := runWait(t, ctx, e)
	waitForPolls(t, e, 1)
	e.send(healthStatus("another"))
	time.Sleep(50 * time.Millisecond)
	if n := e.polls.Load(); n != 1 {
		t.Fatalf("an event of another service caused a poll: %d polls", n)
	}
	cancel()
	if err := within(t, done, time.Second, "cancel"); err == nil {
		t.Fatal("a cancelled wait reported health")
	}
}

func TestHealthWaitIsWokenByTheResyncAfterAGap(t *testing.T) {
	paceWith(t, time.Hour, time.Hour, 0)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := newEventedHealth()
	done := runWait(t, ctx, e)
	waitForPolls(t, e, 1)
	e.healthy.Store(true)
	e.send(engine.Event{Type: engine.ResyncType, Action: engine.ResyncAction})
	if err := within(t, done, 2*time.Second, "resync"); err != nil {
		t.Fatal(err)
	}
}

func TestBurstOfEventsIsOnePoll(t *testing.T) {
	paceWith(t, time.Hour, time.Hour, 0)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := newEventedHealth()
	// The burst comes while the first poll reads Docker, so all ten events are queued before the wait can read one.
	e.onPoll = func(n int32) {
		if n == 1 {
			for i := 0; i < 10; i++ {
				e.send(healthStatus("s"))
			}
		}
	}
	done := runWait(t, ctx, e)
	waitForPolls(t, e, 2)
	time.Sleep(50 * time.Millisecond)
	if n := e.polls.Load(); n != 2 {
		t.Fatalf("ten events caused %d polls, want 1", n-1)
	}
	e.healthy.Store(true)
	e.send(healthStatus("s"))
	if err := within(t, done, 2*time.Second, "burst"); err != nil {
		t.Fatal(err)
	}
}

func TestDrainAndUpdateWaitsAreWokenByEventsToo(t *testing.T) {
	paceWith(t, time.Hour, time.Hour, 0)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := newEventedHealth()
	// The old task keeps "running" until the test flips healthy, which here stands for the old task having ended.
	drain := goWait(t, ctx, func(ctx context.Context) error { return waitDrain(ctx, drainView{e}, "s", "new") })
	waitForPolls(t, e, 1)
	e.healthy.Store(true)
	e.send(engine.Event{Type: "container", Action: "die", Attributes: map[string]string{"com.docker.swarm.service.id": "s"}})
	if err := within(t, drain, 2*time.Second, "drain"); err != nil {
		t.Fatal(err)
	}
}

// drainView shows the health reader's one task as a task of an old generation, running until the test flips it.
type drainView struct{ *eventedHealth }

func (d drainView) ServiceTasks(ctx context.Context, id string) ([]engine.ServiceTask, error) {
	tasks, _ := d.eventedHealth.ServiceTasks(ctx, id)
	for i := range tasks {
		tasks[i].Labels = map[string]string{"berthy.operation": "old"}
		if d.healthy.Load() {
			tasks[i].State = "shutdown"
		}
	}
	return tasks, nil
}
