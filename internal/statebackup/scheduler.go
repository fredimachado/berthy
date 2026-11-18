package statebackup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"berthy/internal/manifest"
	"berthy/internal/schedule"
	"berthy/internal/state"
)

// ScheduledActor is the audit identity of scheduled state backups.
const ScheduledActor = "state-backup"

// Scheduler takes the state backup on its schedule, once per UTC occurrence. The state's unique occurrence record decides, so a
// restart or a second process cannot take an occurrence twice. Occurrences that pass while the agent is down are not made up.
// Its fields are set before Run.
type Scheduler struct {
	Env    string
	Store  *state.Store
	Runner *Runner
	// Manifest returns the running manifest, whose state_backup block is environment-wide. It returns sql.ErrNoRows when
	// nothing was ever applied, which means no state backup.
	Manifest func(ctx context.Context) (manifest.Manifest, error)
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Log receives operational messages. They never contain secrets.
	Log func(string)

	tick    time.Duration // how often Run looks at the clock; zero means a second
	minutes schedule.Minutes
	wg      sync.WaitGroup // the snapshots in progress
}

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

// Run starts the scheduler and returns when ctx ends and every snapshot it started has ended and recorded its outcome.
func (s *Scheduler) Run(ctx context.Context) {
	defer s.Wait()
	interval := s.tick
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.Step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Wait blocks until the snapshots in progress have ended.
func (s *Scheduler) Wait() { s.wg.Wait() }

// Step handles the current UTC minute if it has not been handled yet. Run calls it every tick.
func (s *Scheduler) Step(ctx context.Context) {
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
			s.logf("state backup: manifest unavailable, skipping %s", cur.Format("15:04"))
		}
		return
	}
	if m.StateBackup == nil {
		return
	}
	sched, err := schedule.Parse(Schedule(*m.StateBackup))
	if err != nil || !sched.Matches(cur) {
		return
	}
	p, err := s.Runner.Begin(ctx, m, "schedule", cur)
	switch {
	case errors.Is(err, ErrRunning):
		s.logf("state backup: %s: skipped: previous snapshot still running", cur.Format("15:04"))
		return
	case errors.Is(err, state.ErrConflict):
		return
	case err != nil:
		s.logf("state backup: %s: cannot start: %v", cur.Format("15:04"), err)
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.audit(ctx, "started")
		res := p.Execute(ctx)
		s.audit(ctx, res.Run.Status)
	}()
}

// audit records a step of a scheduled snapshot in the audit log, as the state backup.
func (s *Scheduler) audit(ctx context.Context, outcome string) {
	actor := state.Actor{ID: ScheduledActor, Name: ScheduledActor}
	if err := s.Store.RecordAudit(context.WithoutCancel(ctx), actor, "state_backup", outcome); err != nil {
		s.logf("state backup: cannot audit the run: %v", err)
	}
}
