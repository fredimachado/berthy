// Package jobs runs the manifest's named commands on their cron schedules or on demand, and records each run in the agent's
// state. How a command runs is the Runner's business: a job of an image app runs in a container of its own, and a job of a
// Compose app in the service's container. Each scheduled occurrence runs at most once: the state's unique index decides, so
// a restart or a second scheduler cannot run it twice. Occurrences missed while the agent was down are not made up. The
// output of a run that failed or timed out is kept, sealed, with the secrets in it redacted by the Runner; the output of a
// run that succeeded is thrown away.
package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
	"time"

	"berthy/internal/manifest"
	"berthy/internal/schedule"
	"berthy/internal/state"
)

var (
	// ErrJobRunning means a run of the job is in progress.
	ErrJobRunning = errors.New("job is already running")
	// ErrUnknownJob means the running manifest has no job with that name.
	ErrUnknownJob = errors.New("unknown job")
)

// Outcome is how a run of a job ended.
type Outcome struct {
	// Exit is the command's exit code. It is nil when the command did not run to an end.
	Exit *int
	// TimedOut means the run was stopped at its time limit: the command is over.
	TimedOut bool
	// Detail is the reason the command did not run to an end, worded for the run record. It is empty when Exit is set.
	Detail string
	// Container is the container the command ran in, or "".
	Container string
	// Output is the last lines of a failed or timed-out run, with every managed secret value replaced. It is empty for a run
	// that succeeded and for one with no output to read.
	Output string
}

// Streams is where a command that is run for its output or its input is connected. A command with neither is run as a job is:
// its output is thrown away, and the last lines of a failed run are kept. With Stdout the command's standard output goes there
// as it is written, and the kept lines are its standard error only. With Stdin the command reads it, and its input ends when
// Stdin does. Docker gives a command no more than these, so neither is a file on the server.
type Streams struct {
	Stdin  io.Reader
	Stdout io.Writer
}

// Runner runs the command of a job.
type Runner interface {
	// Run runs the job's command and returns how it ended. The run is bounded by ctx's deadline, which is the job's timeout:
	// when it passes, the command is stopped and the outcome says TimedOut. When ctx is canceled for any other reason, the
	// command is stopped and the error is ctx's. An error that is not ctx's means the run could not be made, and its text is
	// never recorded; a reason that belongs in the run record is Outcome.Detail instead.
	Run(ctx context.Context, m manifest.Manifest, name string, job manifest.Job, runID int64) (Outcome, error)
}

// RunnerFunc is a Runner that is one function, such as a method of the component that knows how to run a command.
type RunnerFunc func(ctx context.Context, m manifest.Manifest, name string, job manifest.Job, runID int64) (Outcome, error)

// Run calls f.
func (f RunnerFunc) Run(ctx context.Context, m manifest.Manifest, name string, job manifest.Job, runID int64) (Outcome, error) {
	return f(ctx, m, name, job, runID)
}

// Scheduler runs the jobs of the running manifest. Its fields are set before Run or Start.
type Scheduler struct {
	Env   string
	Store *state.Store
	// Manifest returns the running manifest. It returns sql.ErrNoRows when nothing is deployed, which means no jobs.
	Manifest func(ctx context.Context) (manifest.Manifest, error)
	Runner   Runner
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Log receives operational messages. They never contain secrets.
	Log func(string)

	tick time.Duration // how often Run looks at the clock; zero means a second

	minutes schedule.Minutes // the minutes handled
	mu      sync.Mutex
	running map[string]*activeRun // job name -> the run in progress (one writer: guarded by mu)
	base    context.Context       // Run's context, which on-demand runs outlive their request with
	wg      sync.WaitGroup        // the runs in progress
}

const skippedDetail = "previous run still running"

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scheduler) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(fmt.Sprintf(format, args...))
	}
}

// Run starts the scheduler and returns when ctx ends and every run it started has ended. It first fails the runs a
// previous process left running. Every tick it looks at the UTC minute, and when the minute has changed it starts each
// job whose schedule matches that minute. Only the current minute counts: a sleep or a clock jump makes up nothing.
func (s *Scheduler) Run(ctx context.Context) {
	s.mu.Lock()
	s.base = ctx
	s.mu.Unlock()
	defer s.wait()
	if n, err := s.Store.AbandonRunningJobs(ctx); err != nil {
		s.logf("jobs: cannot fail interrupted runs: %v", err)
	} else if n > 0 {
		s.logf("jobs: %d run(s) were interrupted by an agent restart and are recorded as failed", n)
	}
	interval := s.tick
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// wait blocks until the runs in progress have ended.
func (s *Scheduler) wait() { s.wg.Wait() }

// step handles the current minute if it has not been handled yet.
func (s *Scheduler) step(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	cur, ok := s.minutes.Claim(s.now())
	if !ok {
		return
	}
	m, err := s.Manifest(ctx)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.logf("jobs: manifest unavailable, skipping %s", cur.Format("15:04"))
		}
		return
	}
	for _, name := range slices.Sorted(maps.Keys(m.Jobs)) {
		job := m.Jobs[name]
		if job.Schedule == "" {
			continue
		}
		if sched, err := m.ParseSchedule(job.Schedule); err == nil && sched.Matches(cur) {
			s.occur(ctx, m, name, job, cur)
		}
	}
}

// claim marks the job as running, and reports false when it already was.
func (s *Scheduler) claim(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[name] != nil {
		return false
	}
	if s.running == nil {
		s.running = map[string]*activeRun{}
	}
	ctx, stop := context.WithCancelCause(context.Background())
	s.running[name] = &activeRun{ctx: ctx, stop: stop}
	return true
}

func (s *Scheduler) release(name string) {
	s.mu.Lock()
	if a := s.running[name]; a != nil {
		a.stop(nil)
	}
	delete(s.running, name)
	s.mu.Unlock()
}

// activeRun is the run of a job that is marked running. A cancel ends ctx, with a *Cancelled as its cause, while the command runs.
type activeRun struct {
	ctx       context.Context
	stop      context.CancelCauseFunc
	executing bool // the command is running (guarded by Scheduler.mu)
}

// Cancelled is why a run of a job was stopped by a person or an agent: the cause of the context the command runs with.
type Cancelled struct{ By string }

func (e *Cancelled) Error() string { return "cancelled by " + e.By }

// ErrJobNotRunning means no run of the job is in progress, so there is nothing to cancel.
var ErrJobNotRunning = errors.New("job is not running")

// Cancel stops the run of the job that is in progress: its command is stopped as at a timeout, and the run is recorded as failed
// with the detail "cancelled by <by>". A command that ended on its own first keeps the result it had. It returns
// ErrJobNotRunning when no run is in progress, and ErrUnknownJob for a name that is not in the manifest.
func (s *Scheduler) Cancel(ctx context.Context, name, by string) error {
	s.mu.Lock()
	a := s.running[name]
	if a != nil && a.executing {
		a.stop(&Cancelled{By: by})
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	if m, err := s.Manifest(ctx); err == nil {
		if _, ok := m.Jobs[name]; ok {
			return ErrJobNotRunning
		}
	}
	return ErrUnknownJob
}

// linked returns a context that ends when ctx does, and when a cancel of the job's run is accepted, with the *Cancelled as its
// cause. It marks the run as executing until the returned function is called.
func (s *Scheduler) linked(ctx context.Context, name string) (context.Context, context.CancelFunc) {
	s.mu.Lock()
	a := s.running[name]
	if a == nil {
		s.mu.Unlock()
		return ctx, func() {}
	}
	a.executing = true
	s.mu.Unlock()
	child, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(a.ctx, func() { cancel(context.Cause(a.ctx)) })
	return child, func() {
		stop()
		cancel(context.Canceled)
		s.mu.Lock()
		a.executing = false
		s.mu.Unlock()
	}
}

// occur handles one scheduled occurrence: it skips it when the job is still running, and otherwise starts a run.
func (s *Scheduler) occur(ctx context.Context, m manifest.Manifest, name string, job manifest.Job, at time.Time) {
	now := s.now().UTC()
	if !s.claim(name) {
		_, err := s.Store.StartJobRun(ctx, state.JobRun{Job: name, App: job.App, Trigger: "schedule", Scheduled: at, Started: now, Finished: now, Status: "skipped", Detail: skippedDetail})
		switch {
		case err == nil:
			s.audit(ctx, name, "skipped")
		case !errors.Is(err, state.ErrConflict):
			s.logf("jobs: cannot record the skipped run of %s: %v", name, err)
		}
		return
	}
	id, err := s.Store.StartJobRun(ctx, state.JobRun{Job: name, App: job.App, Trigger: "schedule", Scheduled: at, Started: now})
	if err != nil {
		s.release(name)
		if !errors.Is(err, state.ErrConflict) {
			s.logf("jobs: cannot record the run of %s: %v", name, err)
		}
		return
	}
	s.spawn(ctx, m, name, job, state.JobRun{ID: id, Job: name, App: job.App, Trigger: "schedule", Scheduled: at, Started: now})
}

func (s *Scheduler) spawn(ctx context.Context, m manifest.Manifest, name string, job manifest.Job, run state.JobRun) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.release(name)
		s.execute(ctx, m, name, job, run, "interrupted", "agent stopped during the run")
	}()
}

// begin looks up the job and starts an on-demand run of it. The job is marked running until execute ends.
func (s *Scheduler) begin(ctx context.Context, name, trigger string) (manifest.Manifest, manifest.Job, state.JobRun, error) {
	if trigger != "manual" && trigger != "backup" {
		return manifest.Manifest{}, manifest.Job{}, state.JobRun{}, errors.New("invalid trigger")
	}
	var m manifest.Manifest
	verified, hasVerified := VerifiedFromContext(ctx)
	useVerified := false
	if trigger == "backup" && hasVerified {
		job, ok := verified.Manifest.Jobs[name]
		if !ok {
			return m, manifest.Job{}, state.JobRun{}, ErrUnknownJob
		}
		if job.App == verified.App {
			m = verified.Manifest
			useVerified = true
		}
	}
	if !useVerified {
		var err error
		m, err = s.Manifest(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return m, manifest.Job{}, state.JobRun{}, ErrUnknownJob
		}
		if err != nil {
			return m, manifest.Job{}, state.JobRun{}, err
		}
	}
	job, ok := m.Jobs[name]
	if !ok {
		return m, job, state.JobRun{}, ErrUnknownJob
	}
	if trigger == "backup" && hasVerified && !useVerified && job.App != verified.Manifest.Jobs[name].App {
		return m, job, state.JobRun{}, ErrUnknownJob
	}
	if !s.claim(name) {
		return m, job, state.JobRun{}, ErrJobRunning
	}
	run := state.JobRun{Job: name, App: job.App, Trigger: trigger, Started: s.now().UTC()}
	id, err := s.Store.StartJobRun(ctx, run)
	if err != nil {
		s.release(name)
		return m, job, state.JobRun{}, err
	}
	run.ID = id
	return m, job, run, nil
}

// Start launches one run of the job in the background and returns its run id. trigger is "manual" or "backup". It
// returns ErrJobRunning while a run of the job is in progress, and ErrUnknownJob for a name that is not in the manifest.
// The run does not end with ctx; it ends when the command does, or when Run's context ends.
func (s *Scheduler) Start(ctx context.Context, name, trigger string) (int64, error) {
	m, job, run, err := s.begin(ctx, name, trigger)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	base := s.base
	s.mu.Unlock()
	if base == nil {
		base = context.Background()
	}
	s.spawn(base, m, name, job, run)
	return run.ID, nil
}

// RunAndWait runs the job and waits for the result. A canceled ctx ends the run, which is then recorded as failed.
func (s *Scheduler) RunAndWait(ctx context.Context, name, trigger string) (state.JobRun, error) {
	m, job, run, err := s.begin(ctx, name, trigger)
	if err != nil {
		return state.JobRun{}, err
	}
	defer s.release(name)
	return s.execute(ctx, m, name, job, run, "failed", "run canceled"), nil
}

// holdPoll is how often RunHeld looks again at a job that is running.
const holdPoll = 50 * time.Millisecond

// RunHeld runs the job once for a volume backup, and keeps it marked running until the returned release is called. It first
// waits for an active run of the job to finish, so the backup never cuts one short. While the job is held, an occurrence
// that arrives is recorded as skipped, as for any run in progress. The run is recorded with trigger "backup"; a command
// that fails is a finished run with status failed, not an error. The release is safe to call more than once. A canceled ctx
// ends the wait, or the run, and no release is returned with an error.
func (s *Scheduler) RunHeld(ctx context.Context, name string) (state.JobRun, func(), error) {
	for {
		m, job, run, err := s.begin(ctx, name, "backup")
		switch {
		case err == nil:
			var once sync.Once
			release := func() { once.Do(func() { s.release(name) }) }
			// execute does not release the job: the caller does.
			return s.execute(ctx, m, name, job, run, "failed", "run canceled"), release, nil
		case !errors.Is(err, ErrJobRunning):
			return state.JobRun{}, nil, err
		}
		select {
		case <-ctx.Done():
			return state.JobRun{}, nil, ctx.Err()
		case <-time.After(holdPoll):
		}
	}
}

// execute runs the job's command and records the result. The caller has marked the job running and recorded the run, and
// clears the mark once it has no more use for it.
// A run that ctx ends is recorded with status canceledStatus and detail canceled. A run in the background ends with its
// context when the agent stops, which says nothing about the job, so it is interrupted. A caller that waits for the run
// wants an incomplete run to be a failure.
func (s *Scheduler) execute(ctx context.Context, m manifest.Manifest, name string, job manifest.Job, run state.JobRun, canceledStatus, canceled string) state.JobRun {
	timeout := job.TimeoutDuration()
	runCtx, endRun := s.linked(ctx, name)
	ectx, cancel := context.WithTimeout(runCtx, timeout)
	out, err := s.Runner.Run(ectx, m, name, job, run.ID)
	deadline := errors.Is(ectx.Err(), context.DeadlineExceeded)
	var by *Cancelled
	errors.As(context.Cause(runCtx), &by)
	cancel()
	endRun()
	status, detail := "succeeded", ""
	var output string
	switch {
	case ctx.Err() != nil && (err != nil || out.TimedOut):
		status, detail = canceledStatus, canceled
	case by != nil && (err != nil || out.TimedOut):
		// A person or an agent stopped the run. Its last lines are kept, as for a run that failed.
		status, detail, output = "failed", by.Error(), out.Output
	case out.TimedOut || err != nil && deadline:
		status, detail, output = "failed", "timed out after "+timeout.String(), out.Output
	case err != nil:
		status, detail = "failed", "run failed"
	case out.Exit == nil:
		status, detail = "failed", out.Detail
		if detail == "" {
			detail = "run failed"
		}
	case *out.Exit != 0:
		status, detail, output = "failed", fmt.Sprintf("exit status %d", *out.Exit), out.Output
	}
	// The result is recorded even when the agent is stopping.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if out.Container != "" {
		if err := s.Store.SetJobRunContainer(fctx, run.ID, out.Container); err != nil {
			s.logf("jobs: cannot record the container of %s: %v", name, err)
		}
	}
	if err := s.Store.FinishJobRunWithOutput(fctx, run.ID, status, out.Exit, detail, output); err != nil {
		s.logf("jobs: cannot record the result of %s: %v", name, err)
	}
	s.audit(fctx, name, status)
	run.Container, run.Status, run.ExitCode, run.Detail, run.HasOutput, run.Finished = out.Container, status, out.Exit, detail, output != "", s.now().UTC()
	return run
}

// audit records a run in the audit log, with the job as the caller.
func (s *Scheduler) audit(ctx context.Context, name, outcome string) {
	actor := "job:" + name
	if err := s.Store.RecordAudit(context.WithoutCancel(ctx), state.Actor{ID: actor, Name: actor}, "job_run:"+name, outcome); err != nil {
		s.logf("jobs: cannot audit the run of %s: %v", name, err)
	}
}
