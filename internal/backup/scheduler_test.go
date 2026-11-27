package backup

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/manifest"
	"berthy/internal/objectstore"
	"berthy/internal/state"
)

// scheduleRig is a backup with a schedule, a scheduler on a clock the test moves, and the runner of the other tests.
type scheduleRig struct {
	*rig
	sched  *Scheduler
	now    time.Time
	nowMu  sync.Mutex
	logMsg []string
	logMu  sync.Mutex
}

func newScheduleRig(t *testing.T, cron string) *scheduleRig {
	t.Helper()
	g := &scheduleRig{rig: newRig(t), now: time.Date(2026, 11, 16, 10, 0, 30, 0, time.UTC)}
	b := g.m.Backups["uploads"]
	b.Schedule = cron
	g.m.Backups["uploads"] = b
	g.sched = g.newScheduler(g.runner)
	return g
}

func (g *scheduleRig) newScheduler(r *Runner) *Scheduler {
	return &Scheduler{
		Env:    "prod",
		Store:  g.st,
		Runner: r,
		Manifest: func(context.Context) (manifest.Manifest, error) {
			return g.m, nil
		},
		Now: func() time.Time { g.nowMu.Lock(); defer g.nowMu.Unlock(); return g.now },
		Log: func(s string) { g.logMu.Lock(); g.logMsg = append(g.logMsg, s); g.logMu.Unlock() },
	}
}

func (g *scheduleRig) advance(d time.Duration) {
	g.nowMu.Lock()
	g.now = g.now.Add(d)
	g.nowMu.Unlock()
}

func (g *scheduleRig) logged() string {
	g.logMu.Lock()
	defer g.logMu.Unlock()
	return strings.Join(g.logMsg, "\n")
}

// settle looks at the clock and waits for the runs that were started.
func (g *scheduleRig) settle(s *Scheduler) {
	s.Step(context.Background())
	s.Wait()
}

func (g *scheduleRig) runs() []state.BackupRun {
	g.t.Helper()
	runs, err := g.st.BackupRuns(context.Background(), "uploads", 100)
	if err != nil {
		g.t.Fatal(err)
	}
	return runs
}

func TestBackupRetriesManifestWithinTheMinute(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		log  string
	}{
		{"failure", errors.New("broken"), "backups: manifest unavailable, retrying"},
		{"no manifest", sql.ErrNoRows, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newScheduleRig(t, "* * * * *")
			g.archive("recovered")
			reads := 0
			g.sched.Manifest = func(context.Context) (manifest.Manifest, error) {
				reads++
				if reads == 1 {
					return manifest.Manifest{}, tc.err
				}
				return g.m, nil
			}
			g.settle(g.sched)
			if runs := g.runs(); len(runs) != 0 {
				t.Errorf("runs after failed read: %+v", runs)
			}
			g.settle(g.sched)
			g.settle(g.sched)
			runs := g.runs()
			if reads != 2 || len(runs) != 1 {
				t.Errorf("reads %d, runs %+v", reads, runs)
			} else if runs[0].Status != "succeeded" || !runs[0].Scheduled.Equal(time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)) {
				t.Errorf("run: %+v", runs[0])
			}
			if got := g.logged(); got != tc.log {
				t.Errorf("log: got %q, want %q", got, tc.log)
			}
		})
	}
}

func TestBackupManifestReadCrossingAMinuteDoesNotBackfill(t *testing.T) {
	g := newScheduleRig(t, "0 10 * * *")
	g.archive("old")
	g.sched.Manifest = func(context.Context) (manifest.Manifest, error) {
		g.advance(time.Minute)
		return g.m, nil
	}
	g.settle(g.sched)
	if runs := g.runs(); len(runs) != 0 {
		t.Fatalf("backfilled old minute: %+v", runs)
	}
}

func TestABackupRunsOncePerOccurrence(t *testing.T) {
	g := newScheduleRig(t, "* * * * *")
	g.archive("minute-one")
	g.settle(g.sched)
	g.settle(g.sched) // the same minute again
	runs := g.runs()
	if len(runs) != 1 || runs[0].Trigger != "schedule" || runs[0].Status != "succeeded" || !runs[0].Scheduled.Equal(time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("runs: %+v", runs)
	}
	g.advance(time.Minute)
	g.archive("minute-two")
	g.settle(g.sched)
	if runs = g.runs(); len(runs) != 2 || runs[0].Status != "succeeded" || !runs[0].Scheduled.Equal(time.Date(2026, 11, 16, 10, 1, 0, 0, time.UTC)) {
		t.Fatalf("runs after the next minute: %+v", runs)
	}
	if len(g.objs.keys()) != 4 {
		t.Fatalf("objects: %v", g.objs.keys())
	}
}

func TestOnlyBackupsWithAMatchingScheduleRun(t *testing.T) {
	g := newScheduleRig(t, "30 3 * * *")
	g.m.Backups["manual-only"] = manifest.Backup{App: "files", Volume: "uploads", Destination: "offsite"}
	g.settle(g.sched) // 10:00 matches nothing
	if runs := g.runs(); len(runs) != 0 {
		t.Fatalf("runs at 10:00: %+v", runs)
	}
	g.advance(17*time.Hour + 30*time.Minute) // 03:30 the next day
	g.archive("night")
	g.settle(g.sched)
	if runs := g.runs(); len(runs) != 1 || runs[0].Status != "succeeded" {
		t.Fatalf("runs at 03:30: %+v", runs)
	}
	if all, _ := g.st.BackupRuns(context.Background(), "manual-only", 10); len(all) != 0 {
		t.Fatalf("a backup without a schedule ran: %+v", all)
	}
}

func TestTwoSchedulersRunAnOccurrenceOnce(t *testing.T) {
	g := newScheduleRig(t, "* * * * *")
	// A second process: its own runner and in-memory state, the same state database.
	r := g.runner
	other := &Runner{Store: r.Store, Archiver: r.Archiver, Objects: r.Objects, Jobs: r.Jobs, Resolve: r.Resolve, Env: r.Env, Now: r.Now, Log: r.Log}
	second := g.newScheduler(other)
	g.archive("once")
	g.settle(g.sched)
	g.settle(second)
	if runs := g.runs(); len(runs) != 1 {
		t.Fatalf("runs: %+v", runs)
	}
	// And at the same moment, with the first still running.
	g.advance(time.Minute)
	pr, pw := io.Pipe()
	g.docker.stdout = pr
	g.sched.Step(context.Background())
	second.Step(context.Background())
	if runs := g.runs(); len(runs) != 2 {
		t.Fatalf("runs while the first of the minute is running: %+v", runs)
	}
	pw.Write([]byte("data"))
	pw.Close()
	g.sched.Wait()
	second.Wait()
	for _, r := range g.runs() {
		if r.Status != "succeeded" {
			t.Fatalf("run: %+v", r)
		}
	}
}

func TestARestartMakesUpNoMissedOccurrence(t *testing.T) {
	g := newScheduleRig(t, "* * * * *")
	// The agent was down from 10:00 to 10:07: a new scheduler handles the current minute and nothing before it.
	g.advance(7 * time.Minute)
	g.archive("now")
	restarted := g.newScheduler(g.runner)
	g.settle(restarted)
	runs := g.runs()
	if len(runs) != 1 || !runs[0].Scheduled.Equal(time.Date(2026, 11, 16, 10, 7, 0, 0, time.UTC)) {
		t.Fatalf("runs after the restart: %+v", runs)
	}
	// A restart within the minute that was already run does not run it again.
	again := g.newScheduler(g.runner)
	g.settle(again)
	if runs = g.runs(); len(runs) != 1 {
		t.Fatalf("the occurrence ran twice: %+v", runs)
	}
}

func TestAnOccurrenceWhileTheBackupRunsIsSkippedAndNotAFailure(t *testing.T) {
	g := newScheduleRig(t, "* * * * *")
	pr, pw := io.Pipe()
	g.docker.stdout = pr
	g.sched.Step(context.Background())
	if runs := g.runs(); len(runs) != 1 || runs[0].Status != "running" {
		t.Fatalf("first occurrence: %+v", runs)
	}
	g.advance(time.Minute)
	g.sched.Step(context.Background())
	runs := g.runs()
	if len(runs) != 1 || runs[0].Status != "running" {
		t.Fatalf("a skipped occurrence left a trace: %+v", runs)
	}
	if !strings.Contains(g.logged(), "skipped: previous backup still running") {
		t.Fatalf("log: %q", g.logged())
	}
	pw.Write([]byte("data"))
	pw.Close()
	g.sched.Wait()
	if h, _ := g.st.BackupHistory(context.Background(), "uploads"); h.Last == nil || h.Last.Status != "succeeded" {
		t.Fatalf("history after the skip: %+v", h)
	}
}

func TestARefusedOccurrenceIsRecordedAsAFailedRun(t *testing.T) {
	g := newScheduleRig(t, "* * * * *")
	g.runner.Objects = func(context.Context, manifest.Destination) (objectstore.Store, error) { return nil, ErrCredentials }
	g.settle(g.sched)
	runs := g.runs()
	if len(runs) != 1 || runs[0].Status != "failed" || runs[0].Detail != "backup credentials unavailable" || runs[0].Trigger != "schedule" || runs[0].Finished.IsZero() {
		t.Fatalf("runs: %+v", runs)
	}
	g.settle(g.sched)
	if len(g.runs()) != 1 {
		t.Fatal("the refused occurrence was recorded twice")
	}
	// A manual run that is refused is only an answer to the person who asked.
	if _, err := g.runner.Begin(context.Background(), g.m, "uploads", "manual"); !errors.Is(err, ErrCredentials) {
		t.Fatalf("manual: %v", err)
	}
	if len(g.runs()) != 1 {
		t.Fatalf("a refused manual run was recorded: %+v", g.runs())
	}
	// A hook that is refused is recorded: the backup's check should say why it could not run.
	if _, err := g.runner.Begin(context.Background(), g.m, "uploads", "hook"); !errors.Is(err, ErrCredentials) {
		t.Fatalf("hook: %v", err)
	}
	if runs = g.runs(); len(runs) != 2 || runs[0].Trigger != "hook" || runs[0].Status != "failed" {
		t.Fatalf("a refused hook run: %+v", runs)
	}
}

func TestScheduledRunsAreAuditedAsTheBackup(t *testing.T) {
	g := newScheduleRig(t, "* * * * *")
	g.archive("data")
	g.settle(g.sched)
	audit, _ := g.st.AuditLog(context.Background(), 10)
	var outcomes []string
	for _, a := range audit {
		if a.Action == "backup_run:uploads" && a.ActorID == "backup:uploads" && a.ActorName == "backup:uploads" {
			outcomes = append(outcomes, a.Outcome)
		}
	}
	if !slices.Equal(outcomes, []string{"succeeded", "started"}) {
		t.Fatalf("audit (newest first): %v from %+v", outcomes, audit)
	}
}

func TestStoppingTheSchedulerCutsARunShortAsAgentStopped(t *testing.T) {
	g := newScheduleRig(t, "* * * * *")
	ctx, cancel := context.WithCancel(context.Background())
	g.docker.stdout = io.MultiReader(strings.NewReader("part"), blockedReader{ctx})
	g.sched.Step(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()
	g.sched.Wait()
	runs := g.runs()
	if len(runs) != 1 || runs[0].Status != "failed" || runs[0].Detail != state.StoppedBackupDetail {
		t.Fatalf("runs: %+v", runs)
	}
}

func TestSchedulerReadsTheManifestEachMinuteAndToleratesNone(t *testing.T) {
	g := newScheduleRig(t, "* * * * *")
	calls := 0
	g.sched.Manifest = func(context.Context) (manifest.Manifest, error) {
		calls++
		return manifest.Manifest{}, sql.ErrNoRows
	}
	g.settle(g.sched)
	g.settle(g.sched)
	g.advance(time.Minute)
	g.settle(g.sched)
	if calls != 3 || len(g.runs()) != 0 {
		t.Fatalf("manifest reads %d, runs %+v", calls, g.runs())
	}
	if strings.Contains(g.logged(), "unavailable") {
		t.Fatalf("a missing manifest was logged: %q", g.logged())
	}
	g.sched.Manifest = func(context.Context) (manifest.Manifest, error) { calls++; return manifest.Manifest{}, nil }
	g.settle(g.sched)
	g.settle(g.sched)
	g.advance(time.Minute)
	g.settle(g.sched)
	g.settle(g.sched)
	if calls != 5 {
		t.Fatalf("successful manifest reads: %d", calls)
	}
	g.advance(time.Minute)
	g.sched.Manifest = func(context.Context) (manifest.Manifest, error) { return manifest.Manifest{}, errors.New("broken") }
	g.settle(g.sched)
	if !strings.Contains(g.logged(), "manifest unavailable") {
		t.Fatalf("log: %q", g.logged())
	}
}
