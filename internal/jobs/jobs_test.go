package jobs

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

func openTestStore(t *testing.T) (*state.Store, string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir, key
}

// clock is a clock the test moves by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeExec records the commands it is asked to run and answers through fn, which defaults to success. It plays the Runner:
// every command runs in container-1, and the result of fn becomes the outcome.
type fakeExec struct {
	mu    sync.Mutex
	calls [][]string
	ids   []string
	fn    func(ctx context.Context, id string, cmd []string) (engine.ExecResult, error)
}

func (f *fakeExec) Run(ctx context.Context, _ manifest.Manifest, _ string, job manifest.Job, _ int64) (Outcome, error) {
	f.mu.Lock()
	f.calls = append(f.calls, job.Command)
	f.ids = append(f.ids, "container-1")
	fn := f.fn
	f.mu.Unlock()
	if fn == nil {
		zero := 0
		return Outcome{Exit: &zero, Container: "container-1"}, nil
	}
	res, err := fn(ctx, "container-1", job.Command)
	if err != nil {
		return Outcome{}, err
	}
	code := res.ExitCode
	return Outcome{Exit: &code, Container: "container-1", Output: string(res.Output)}, nil
}

// runnerFunc makes a Runner of a function of the job alone.
func runnerFunc(f func(ctx context.Context, job manifest.Job) (Outcome, error)) Runner {
	return RunnerFunc(func(ctx context.Context, _ manifest.Manifest, _ string, job manifest.Job, _ int64) (Outcome, error) {
		return f(ctx, job)
	})
}

func (f *fakeExec) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

func testManifest(jobs map[string]manifest.Job) manifest.Manifest {
	return manifest.Manifest{
		Project: "demo",
		Apps: map[string]manifest.App{
			"api": {Kind: "image", Roles: map[string]manifest.Role{"web": {}}},
		},
		Jobs: jobs,
	}
}

func tickJob() manifest.Job {
	return manifest.Job{App: "api", Role: "web", Schedule: "* * * * *", Command: []string{"date"}, Timeout: "1h"}
}

type rig struct {
	sched *Scheduler
	store *state.Store
	clock *clock
	exec  *fakeExec
	jobs  map[string]manifest.Job
	zone  string // the manifest's timezone
	mu    sync.Mutex
}

func newRig(t *testing.T, jobs map[string]manifest.Job) *rig {
	t.Helper()
	store, _, _ := openTestStore(t)
	r := &rig{store: store, clock: &clock{t: time.Date(2026, 11, 16, 10, 0, 30, 0, time.UTC)}, exec: &fakeExec{}, jobs: jobs}
	r.sched = r.newScheduler(store, r.exec)
	return r
}

func (r *rig) newScheduler(store *state.Store, exec Runner) *Scheduler {
	return &Scheduler{
		Env:   "test",
		Store: store,
		Manifest: func(context.Context) (manifest.Manifest, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.jobs == nil {
				return manifest.Manifest{}, sql.ErrNoRows
			}
			m := testManifest(r.jobs)
			m.Timezone = r.zone
			return m, nil
		},
		Runner: exec,
		Now:    r.clock.Now,
		Log:    func(string) {},
		tick:   5 * time.Millisecond,
	}
}

// settle steps the scheduler at the current time and waits for the runs it started.
func (r *rig) settle(ctx context.Context) {
	r.sched.step(ctx)
	r.sched.wait()
}

func (r *rig) runs(t *testing.T, job string) []state.JobRun {
	t.Helper()
	runs, err := r.store.JobRuns(context.Background(), job, 100)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func TestAJobRunsOncePerMinute(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, map[string]manifest.Job{"tick": tickJob()})
	r.settle(ctx)
	r.settle(ctx) // the same minute again
	r.sched.step(ctx)
	r.sched.wait()
	if n := r.exec.count(); n != 1 {
		t.Fatalf("runs in the first minute: %d", n)
	}
	r.clock.Add(time.Minute)
	r.settle(ctx)
	if n := r.exec.count(); n != 2 {
		t.Fatalf("runs after two minutes: %d", n)
	}
	runs := r.runs(t, "tick")
	if len(runs) != 2 || runs[0].Status != "succeeded" || runs[0].Trigger != "schedule" || runs[0].App != "api" || runs[0].Container != "container-1" || runs[0].ExitCode == nil || *runs[0].ExitCode != 0 {
		t.Fatalf("runs: %+v", runs)
	}
	if want := time.Date(2026, 11, 16, 10, 1, 0, 0, time.UTC); !runs[0].Scheduled.Equal(want) {
		t.Fatalf("occurrence %v, want %v", runs[0].Scheduled, want)
	}
	if got := r.exec.calls[0]; len(got) != 1 || got[0] != "date" || r.exec.ids[0] != "container-1" {
		t.Fatalf("exec: %v in %s", got, r.exec.ids[0])
	}
}

func TestOnlyMatchingScheduledJobsRun(t *testing.T) {
	ctx := context.Background()
	other := tickJob()
	other.Schedule = "30 2 * * *"
	onDemand := tickJob()
	onDemand.Schedule = ""
	r := newRig(t, map[string]manifest.Job{"tick": tickJob(), "daily": other, "manual-only": onDemand})
	r.settle(ctx)
	if n := r.exec.count(); n != 1 || len(r.runs(t, "daily")) != 0 || len(r.runs(t, "manual-only")) != 0 {
		t.Fatalf("exec count %d", n)
	}
	noJobs := newRig(t, nil)
	noJobs.settle(ctx)
	if noJobs.exec.count() != 0 {
		t.Fatal("ran without a manifest")
	}
}

func TestAClockJumpDoesNotBackfill(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, map[string]manifest.Job{"tick": tickJob()})
	r.settle(ctx)
	r.clock.Add(37 * time.Minute)
	r.settle(ctx)
	if runs := r.runs(t, "tick"); len(runs) != 2 {
		t.Fatalf("runs after a jump: %d", len(runs))
	}
	r.clock.Add(-10 * time.Minute)
	r.settle(ctx)
	if n := r.exec.count(); n != 2 {
		t.Fatalf("a clock moved back ran the job: %d", n)
	}
}

func TestTwoSchedulersOnOneStoreRunAnOccurrenceOnce(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, map[string]manifest.Job{"tick": tickJob()})
	secondExec := &fakeExec{}
	second := r.newScheduler(r.store, secondExec)
	r.sched.step(ctx)
	second.step(ctx)
	r.sched.wait()
	second.wait()
	if total := r.exec.count() + secondExec.count(); total != 1 {
		t.Fatalf("occurrence ran %d times", total)
	}
	if runs := r.runs(t, "tick"); len(runs) != 1 {
		t.Fatalf("runs: %+v", runs)
	}
}

func TestAStillRunningJobIsSkippedAtTheNextOccurrence(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, map[string]manifest.Job{"slow": tickJob()})
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	r.exec.fn = func(ctx context.Context, id string, cmd []string) (engine.ExecResult, error) {
		started <- struct{}{}
		<-release
		return engine.ExecResult{}, nil
	}
	r.sched.step(ctx)
	<-started
	r.clock.Add(time.Minute)
	r.sched.step(ctx)
	runs := r.runs(t, "slow")
	if len(runs) != 2 || runs[0].Status != "skipped" || runs[0].Detail != "previous run still running" || runs[0].Finished.IsZero() || runs[1].Status != "running" {
		t.Fatalf("runs while the first still runs: %+v", runs)
	}
	close(release)
	r.sched.wait()
	if n := r.exec.count(); n != 1 {
		t.Fatalf("the skipped occurrence ran: %d", n)
	}
	last, err := r.store.LastJobRuns(ctx)
	if err != nil || last["slow"].Status != "succeeded" {
		t.Fatalf("a skip changed the last run: %+v %v", last, err)
	}
	// With the run finished, the next occurrence runs again.
	r.clock.Add(time.Minute)
	r.exec.fn = nil
	r.settle(ctx)
	if n := r.exec.count(); n != 2 {
		t.Fatalf("job did not run again: %d", n)
	}
}

func TestFailuresAreRecordedWithAFixedDetail(t *testing.T) {
	ctx := context.Background()
	t.Run("nonzero exit", func(t *testing.T) {
		r := newRig(t, map[string]manifest.Job{"fail": tickJob()})
		r.exec.fn = func(context.Context, string, []string) (engine.ExecResult, error) {
			return engine.ExecResult{ExitCode: 3, Output: []byte("boom")}, nil
		}
		r.settle(ctx)
		runs := r.runs(t, "fail")
		if len(runs) != 1 || runs[0].Status != "failed" || runs[0].Detail != "exit status 3" || runs[0].ExitCode == nil || *runs[0].ExitCode != 3 {
			t.Fatalf("runs: %+v", runs)
		}
	})
	t.Run("the command did not run", func(t *testing.T) {
		r := newRig(t, map[string]manifest.Job{"fail": tickJob()})
		r.sched.Runner = runnerFunc(func(context.Context, manifest.Job) (Outcome, error) {
			return Outcome{Detail: "no running container for api/web"}, nil
		})
		r.settle(ctx)
		runs := r.runs(t, "fail")
		if len(runs) != 1 || runs[0].Status != "failed" || runs[0].Detail != "no running container for api/web" || runs[0].ExitCode != nil || r.exec.count() != 0 {
			t.Fatalf("runs: %+v", runs)
		}
	})
	t.Run("a runner error is not echoed", func(t *testing.T) {
		r := newRig(t, map[string]manifest.Job{"fail": tickJob()})
		r.sched.Runner = runnerFunc(func(context.Context, manifest.Job) (Outcome, error) {
			return Outcome{}, errors.New("Docker said secret-detail")
		})
		r.settle(ctx)
		if runs := r.runs(t, "fail"); len(runs) != 1 || runs[0].Status != "failed" || runs[0].Detail != "run failed" || runs[0].ExitCode != nil {
			t.Fatalf("runs: %+v", runs)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		job := tickJob()
		job.Timeout = "1s"
		r := newRig(t, map[string]manifest.Job{"slow": job})
		r.exec.fn = func(ctx context.Context, id string, cmd []string) (engine.ExecResult, error) {
			<-ctx.Done()
			return engine.ExecResult{}, ctx.Err()
		}
		r.settle(ctx)
		if runs := r.runs(t, "slow"); len(runs) != 1 || runs[0].Status != "failed" || runs[0].Detail != "timed out after 1s" {
			t.Fatalf("runs: %+v", runs)
		}
	})
}

func TestARunnerGivesTheJobItsTimeoutAsTheDeadline(t *testing.T) {
	job := tickJob()
	job.Timeout = "1s"
	r := newRig(t, map[string]manifest.Job{"slow": job})
	var left time.Duration
	r.sched.Runner = runnerFunc(func(ctx context.Context, _ manifest.Job) (Outcome, error) {
		d, ok := ctx.Deadline()
		if !ok {
			t.Error("no deadline")
		}
		left = time.Until(d)
		<-ctx.Done()
		return Outcome{}, ctx.Err()
	})
	start := time.Now()
	r.settle(context.Background())
	if left <= 0 || left > time.Second || time.Since(start) > 5*time.Second {
		t.Fatalf("deadline %v away, run took %v", left, time.Since(start))
	}
}

func TestAFailedOrTimedOutRunKeepsItsOutputAndASuccessfulOneKeepsNone(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		outcome Outcome
		status  string
		detail  string
		kept    string
	}{
		{"failed", Outcome{Exit: intp(3), Output: "boom [redacted]"}, "failed", "exit status 3", "boom [redacted]"},
		{"timed out", Outcome{TimedOut: true, Output: "last lines"}, "failed", "timed out after 1s", "last lines"},
		{"succeeded", Outcome{Exit: intp(0), Output: "noise"}, "succeeded", "", ""},
		{"no output to read", Outcome{Exit: intp(1)}, "failed", "exit status 1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := tickJob()
			job.Timeout = "1s"
			r := newRig(t, map[string]manifest.Job{"tick": job})
			r.sched.Runner = runnerFunc(func(context.Context, manifest.Job) (Outcome, error) { return tc.outcome, nil })
			r.settle(ctx)
			runs := r.runs(t, "tick")
			if len(runs) != 1 || runs[0].Status != tc.status || runs[0].Detail != tc.detail {
				t.Fatalf("runs: %+v", runs)
			}
			_, out, err := r.store.JobOutput(ctx, "tick", runs[0].ID)
			if err != nil || out != tc.kept {
				t.Fatalf("kept output %q, want %q (%v)", out, tc.kept, err)
			}
		})
	}
}

func TestACanceledRunKeepsNoOutput(t *testing.T) {
	r := newRig(t, map[string]manifest.Job{"slow": tickJob()})
	r.sched.Runner = runnerFunc(func(ctx context.Context, _ manifest.Job) (Outcome, error) {
		<-ctx.Done()
		return Outcome{Output: "partial"}, ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	run, err := r.sched.RunAndWait(ctx, "slow", "manual")
	if err != nil || run.Status != "failed" || run.Detail != "run canceled" {
		t.Fatalf("run: %+v %v", run, err)
	}
	if _, out, err := r.store.JobOutput(context.Background(), "slow", run.ID); err != nil || out != "" {
		t.Fatalf("output %q %v", out, err)
	}
}

func intp(n int) *int { return &n }

func TestRunsAreAuditedAsTheJob(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, map[string]manifest.Job{"tick": tickJob()})
	r.settle(ctx)
	r.exec.fn = func(context.Context, string, []string) (engine.ExecResult, error) {
		return engine.ExecResult{ExitCode: 1}, nil
	}
	r.clock.Add(time.Minute)
	r.settle(ctx)
	rows, err := r.store.AuditLog(ctx, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("audit: %+v %v", rows, err)
	}
	if a := rows[0]; a.ActorID != "job:tick" || a.ActorName != "job:tick" || a.Action != "job_run:tick" || a.Outcome != "failed" {
		t.Fatalf("newest audit row: %+v", a)
	}
	if a := rows[1]; a.ActorID != "job:tick" || a.Outcome != "succeeded" {
		t.Fatalf("first audit row: %+v", a)
	}
}

func TestStartRunsOnDemand(t *testing.T) {
	ctx := context.Background()
	onDemand := tickJob()
	onDemand.Schedule = ""
	r := newRig(t, map[string]manifest.Job{"manual": onDemand})
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	r.exec.fn = func(context.Context, string, []string) (engine.ExecResult, error) {
		started <- struct{}{}
		<-release
		return engine.ExecResult{}, nil
	}
	id, err := r.sched.Start(ctx, "manual", "manual")
	if err != nil || id == 0 {
		t.Fatalf("start: %d %v", id, err)
	}
	<-started
	if _, err = r.sched.Start(ctx, "manual", "manual"); !errors.Is(err, ErrJobRunning) {
		t.Fatalf("second start: %v", err)
	}
	if _, err = r.sched.Start(ctx, "missing", "manual"); !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("unknown job: %v", err)
	}
	if _, err = r.sched.Start(ctx, "manual", "schedule"); err == nil {
		t.Fatal("started a run with the schedule trigger")
	}
	close(release)
	r.sched.wait()
	runs := r.runs(t, "manual")
	if len(runs) != 1 || runs[0].ID != id || runs[0].Status != "succeeded" || runs[0].Trigger != "manual" || !runs[0].Scheduled.IsZero() {
		t.Fatalf("runs: %+v", runs)
	}
	// A finished job can be started again: manual runs have no occurrence.
	r.exec.fn = nil
	if _, err = r.sched.Start(ctx, "manual", "backup"); err != nil {
		t.Fatalf("start again: %v", err)
	}
	r.sched.wait()
	if runs = r.runs(t, "manual"); len(runs) != 2 || runs[0].Trigger != "backup" {
		t.Fatalf("runs: %+v", runs)
	}
}

func TestStartOfAJobWhoseScheduledRunIsInProgressIsRefused(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, map[string]manifest.Job{"tick": tickJob()})
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	r.exec.fn = func(context.Context, string, []string) (engine.ExecResult, error) {
		started <- struct{}{}
		<-release
		return engine.ExecResult{}, nil
	}
	r.sched.step(ctx)
	<-started
	if _, err := r.sched.Start(ctx, "tick", "manual"); !errors.Is(err, ErrJobRunning) {
		t.Fatalf("start during a scheduled run: %v", err)
	}
	close(release)
	r.sched.wait()
}

func TestRunAndWaitReturnsTheFinishedRun(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, map[string]manifest.Job{"tick": tickJob()})
	r.exec.fn = func(context.Context, string, []string) (engine.ExecResult, error) {
		return engine.ExecResult{ExitCode: 2}, nil
	}
	run, err := r.sched.RunAndWait(ctx, "tick", "backup")
	if err != nil {
		t.Fatal(err)
	}
	if run.ID == 0 || run.Status != "failed" || run.Detail != "exit status 2" || run.ExitCode == nil || *run.ExitCode != 2 || run.Trigger != "backup" || run.Finished.IsZero() {
		t.Fatalf("run: %+v", run)
	}
	if _, err = r.sched.RunAndWait(ctx, "missing", "backup"); !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("unknown job: %v", err)
	}
}

func TestRunAbandonsRunsOfAPreviousProcessAndKeepsScheduling(t *testing.T) {
	r := newRig(t, map[string]manifest.Job{"tick": tickJob()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The previous process was running the 10:00 occurrence when it stopped.
	stale, err := r.store.StartJobRun(ctx, state.JobRun{Job: "tick", App: "api", Trigger: "schedule", Scheduled: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC), Started: r.clock.Now(), Container: "old"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); r.sched.Run(ctx) }()
	waitFor(t, "the abandoned run", func() bool {
		runs := r.runs(t, "tick")
		return len(runs) == 1 && runs[0].ID == stale && runs[0].Status == "failed" && runs[0].Detail == "agent restarted during the run"
	})
	time.Sleep(50 * time.Millisecond)
	if n := r.exec.count(); n != 0 {
		t.Fatalf("the restarted agent ran the occurrence that already had a run: %d", n)
	}
	r.clock.Add(time.Minute)
	waitFor(t, "the next occurrence", func() bool { return r.exec.count() == 1 })
	cancel()
	<-done
	waitFor(t, "the run to finish", func() bool {
		runs := r.runs(t, "tick")
		return len(runs) == 2 && runs[0].Status == "succeeded"
	})
}

func TestRunReturnsOnlyAfterItsRunsEnd(t *testing.T) {
	r := newRig(t, map[string]manifest.Job{"slow": tickJob()})
	started := make(chan struct{}, 1)
	r.exec.fn = func(ctx context.Context, id string, cmd []string) (engine.ExecResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return engine.ExecResult{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.sched.Run(ctx) }()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	runs := r.runs(t, "slow")
	if len(runs) != 1 || runs[0].Status != "interrupted" || runs[0].Detail != "agent stopped during the run" {
		t.Fatalf("run when the agent stops: %+v", runs)
	}
	if last, err := r.store.LastJobRuns(context.Background()); err != nil || len(last) != 0 {
		t.Fatalf("an interrupted run counts as the last run: %+v %v", last, err)
	}
	if rows, err := r.store.AuditLog(context.Background(), 5); err != nil || len(rows) != 1 || rows[0].Outcome != "interrupted" {
		t.Fatalf("audit: %+v %v", rows, err)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunAndWaitCanceledByItsCaller(t *testing.T) {
	r := newRig(t, map[string]manifest.Job{"slow": tickJob()})
	r.exec.fn = func(ctx context.Context, id string, cmd []string) (engine.ExecResult, error) {
		<-ctx.Done()
		return engine.ExecResult{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	run, err := r.sched.RunAndWait(ctx, "slow", "backup")
	if err != nil || run.Status != "failed" || run.Detail != "run canceled" {
		t.Fatalf("run: %+v %v", run, err)
	}
	if runs := r.runs(t, "slow"); len(runs) != 1 || runs[0].Status != "failed" || runs[0].Detail != "run canceled" {
		t.Fatalf("stored: %+v", runs)
	}
}

func TestRunHeldWaitsForAnActiveRunThenRunsOnce(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, map[string]manifest.Job{"tick": tickJob()})
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	r.exec.fn = func(context.Context, string, []string) (engine.ExecResult, error) {
		started <- struct{}{}
		<-release
		return engine.ExecResult{}, nil
	}
	r.sched.step(ctx)
	<-started
	type held struct {
		run     state.JobRun
		release func()
		err     error
	}
	done := make(chan held, 1)
	go func() {
		run, rel, err := r.sched.RunHeld(ctx, "tick")
		done <- held{run, rel, err}
	}()
	select {
	case <-done:
		t.Fatal("RunHeld ran while the job was running")
	case <-time.After(150 * time.Millisecond):
	}
	if n := r.exec.count(); n != 1 {
		t.Fatalf("commands run while waiting: %d", n)
	}
	r.exec.mu.Lock()
	r.exec.fn = nil
	r.exec.mu.Unlock()
	close(release)
	h := <-done
	if h.err != nil || h.run.Status != "succeeded" || h.run.Trigger != "backup" || h.run.Finished.IsZero() || h.release == nil {
		t.Fatalf("held run: %+v %v", h.run, h.err)
	}
	if n := r.exec.count(); n != 2 {
		t.Fatalf("commands run: %d", n)
	}
	h.release()
}

func TestRunHeldKeepsTheJobMarkedRunningUntilReleased(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, map[string]manifest.Job{"tick": tickJob()})
	run, release, err := r.sched.RunHeld(ctx, "tick")
	if err != nil || run.Status != "succeeded" {
		t.Fatalf("held run: %+v %v", run, err)
	}
	// A scheduled occurrence while the job is held is skipped and never runs.
	r.sched.step(ctx)
	r.sched.wait()
	runs := r.runs(t, "tick")
	if len(runs) != 2 || runs[0].Status != "skipped" || runs[0].Trigger != "schedule" || runs[0].Detail != "previous run still running" {
		t.Fatalf("occurrence while held: %+v", runs)
	}
	if n := r.exec.count(); n != 1 {
		t.Fatalf("the skipped occurrence ran: %d", n)
	}
	if _, err = r.sched.Start(ctx, "tick", "manual"); !errors.Is(err, ErrJobRunning) {
		t.Fatalf("start while held: %v", err)
	}
	release()
	release() // releasing twice is harmless
	r.clock.Add(time.Minute)
	r.settle(ctx)
	if n := r.exec.count(); n != 2 {
		t.Fatalf("job did not run after the release: %d", n)
	}
}

func TestRunHeldReturnsAFailedRunAndStillHolds(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, map[string]manifest.Job{"tick": tickJob()})
	r.exec.fn = func(context.Context, string, []string) (engine.ExecResult, error) {
		return engine.ExecResult{ExitCode: 4}, nil
	}
	run, release, err := r.sched.RunHeld(ctx, "tick")
	if err != nil || run.Status != "failed" || run.Detail != "exit status 4" || release == nil {
		t.Fatalf("failed held run: %+v %v", run, err)
	}
	release()
	if _, _, err = r.sched.RunHeld(ctx, "missing"); !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("unknown job: %v", err)
	}
}

func TestRunHeldGivesUpWhenItsContextEnds(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, map[string]manifest.Job{"tick": tickJob()})
	_, release, err := r.sched.RunHeld(ctx, "tick")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	waitCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, _, err = r.sched.RunHeld(waitCtx, "tick"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting for a held job: %v", err)
	}
}

func TestVerifiedBeforeJobForAnotherAppUsesStoredJob(t *testing.T) {
	stored := tickJob()
	stored.Command = []string{"stored"}
	r := newRig(t, map[string]manifest.Job{"prepare": stored})
	incoming := testManifest(map[string]manifest.Job{"prepare": {App: "api", Role: "web", Command: []string{"incoming"}}})
	ctx := WithVerifiedRelease(context.Background(), VerifiedRelease{App: "other", Manifest: incoming})
	run, release, err := r.sched.RunHeld(ctx, "prepare")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if run.Status != "succeeded" || len(r.exec.calls) != 1 || len(r.exec.calls[0]) != 1 || r.exec.calls[0][0] != "stored" {
		t.Fatalf("run: %+v, commands: %v", run, r.exec.calls)
	}
}
