// Package jobs runs the manifest's named commands in a running container of their target, on their cron schedules or on
// demand, and records each run in the agent's state. A job runs in one container only, even when its role has several
// replicas. Each scheduled occurrence runs at most once: the state's unique index decides, so a restart or a second
// scheduler cannot run it twice. Occurrences missed while the agent was down are not made up. A command's output is
// never stored, because it may hold secrets.
package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	// ErrUnknownJob means the last applied manifest has no job with that name.
	ErrUnknownJob = errors.New("unknown job")
)

// TargetError is a reason a job has no container to run in, worded for the job's run record.
type TargetError string

func (e TargetError) Error() string { return string(e) }

// Target finds the container a job runs in.
type Target interface {
	Container(ctx context.Context, m manifest.Manifest, job manifest.Job) (id string, err error)
}

// Executor runs a command inside a container and returns its exit code. The command's output is read and discarded.
type Executor interface {
	ExecDiscard(ctx context.Context, containerID string, command []string) (exitCode int, err error)
}

// Scheduler runs the jobs of the last applied manifest. Its fields are set before Run or Start.
type Scheduler struct {
	Env   string
	Store *state.Store
	// Manifest returns the last applied manifest. It returns sql.ErrNoRows when nothing is deployed, which means no jobs.
	Manifest func(ctx context.Context) (manifest.Manifest, error)
	Target   Target
	Exec     Executor
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Log receives operational messages. They never contain secrets.
	Log func(string)

	tick time.Duration // how often Run looks at the clock; zero means a second

	minutes schedule.Minutes // the minutes handled
	mu      sync.Mutex
	running map[string]bool // job name -> a run is in progress (one writer: guarded by mu)
	base    context.Context // Run's context, which on-demand runs outlive their request with
	wg      sync.WaitGroup  // the runs in progress
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
		if sched, err := schedule.Parse(job.Schedule); err == nil && sched.Matches(cur) {
			s.occur(ctx, m, name, job, cur)
		}
	}
}

// claim marks the job as running, and reports false when it already was.
func (s *Scheduler) claim(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[name] {
		return false
	}
	if s.running == nil {
		s.running = map[string]bool{}
	}
	s.running[name] = true
	return true
}

func (s *Scheduler) release(name string) {
	s.mu.Lock()
	delete(s.running, name)
	s.mu.Unlock()
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
	m, err := s.Manifest(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return m, manifest.Job{}, state.JobRun{}, ErrUnknownJob
	}
	if err != nil {
		return m, manifest.Job{}, state.JobRun{}, err
	}
	job, ok := m.Jobs[name]
	if !ok {
		return m, job, state.JobRun{}, ErrUnknownJob
	}
	if !s.claim(name) {
		return m, job, state.JobRun{}, ErrJobRunning
	}
	run := state.JobRun{Job: name, App: job.App, Trigger: trigger, Started: s.now().UTC()}
	run.ID, err = s.Store.StartJobRun(ctx, run)
	if err != nil {
		s.release(name)
		return m, job, state.JobRun{}, err
	}
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
	status, detail := "succeeded", ""
	var exit *int
	container, err := s.Target.Container(ctx, m, job)
	if err != nil {
		status, detail = "failed", "container lookup failed"
		var te TargetError
		switch {
		case errors.As(err, &te):
			detail = string(te)
		case ctx.Err() != nil:
			status, detail = canceledStatus, canceled
		}
	} else {
		run.Container = container
		if err := s.Store.SetJobRunContainer(ctx, run.ID, container); err != nil {
			s.logf("jobs: cannot record the container of %s: %v", name, err)
		}
		timeout := job.TimeoutDuration()
		ectx, cancel := context.WithTimeout(ctx, timeout)
		code, err := s.Exec.ExecDiscard(ectx, container, job.Command)
		timedOut := errors.Is(ectx.Err(), context.DeadlineExceeded)
		cancel()
		switch {
		case err != nil && timedOut && ctx.Err() == nil:
			status, detail = "failed", "timed out after "+timeout.String()
		case err != nil && ctx.Err() != nil:
			status, detail = canceledStatus, canceled
		case err != nil:
			status, detail = "failed", "exec failed"
		default:
			exit = &code
			if code != 0 {
				status, detail = "failed", fmt.Sprintf("exit status %d", code)
			}
		}
	}
	// The result is recorded even when the agent is stopping.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.Store.FinishJobRun(fctx, run.ID, status, exit, detail); err != nil {
		s.logf("jobs: cannot record the result of %s: %v", name, err)
	}
	s.audit(fctx, name, status)
	run.Status, run.ExitCode, run.Detail, run.Finished = status, exit, detail, s.now().UTC()
	return run
}

// audit records a run in the audit log, with the job as the caller.
func (s *Scheduler) audit(ctx context.Context, name, outcome string) {
	actor := "job:" + name
	if err := s.Store.RecordAudit(context.WithoutCancel(ctx), state.Actor{ID: actor, Name: actor}, "job_run:"+name, outcome); err != nil {
		s.logf("jobs: cannot audit the run of %s: %v", name, err)
	}
}
