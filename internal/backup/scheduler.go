package backup

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

// Scheduler runs the backups of the manifest that have a schedule, once per UTC cron occurrence. Each occurrence runs at most
// once: the state's unique index decides, so a restart or a second process cannot run it twice. Occurrences that pass while the
// agent is down are not made up. Its fields are set before Run.
type Scheduler struct {
	Env    string
	Store  *state.Store
	Runner *Runner
	// Manifest returns the manifest of the deployed apps. It returns sql.ErrNoRows when nothing is deployed, which means no backups.
	Manifest func(ctx context.Context) (manifest.Manifest, error)
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Log receives operational messages. They never contain secrets.
	Log func(string)

	tick    time.Duration // how often Run looks at the clock; zero means a second
	minutes schedule.Minutes
	wg      sync.WaitGroup // the runs in progress
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

// Run starts the scheduler and returns when ctx ends and every run it started has ended and recorded its outcome. Every tick it
// looks at the UTC minute, and when the minute is new it starts each backup whose schedule matches it.
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

// Wait blocks until the runs in progress have ended.
func (s *Scheduler) Wait() { s.wg.Wait() }

// Step handles the current minute if it has not been handled yet. Run calls it every tick.
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
			s.logf("backups: manifest unavailable, skipping %s", cur.Format("15:04"))
		}
		return
	}
	for _, name := range slices.Sorted(maps.Keys(m.Backups)) {
		b := m.Backups[name]
		if b.Schedule == "" {
			continue
		}
		if sched, err := m.ParseSchedule(b.Schedule); err == nil && sched.Matches(cur) {
			s.occur(ctx, m, name, cur)
		}
	}
}

// occur handles one occurrence. A backup of the same volume that is still running makes the occurrence a skip: nothing is
// recorded, because a skip is no failure and must not alert, and it is logged. An occurrence that another process recorded
// first is left to it.
func (s *Scheduler) occur(ctx context.Context, m manifest.Manifest, name string, at time.Time) {
	p, err := s.Runner.BeginAt(ctx, m, name, "schedule", at)
	switch {
	case errors.Is(err, ErrBackupRunning):
		s.logf("backups: %s at %s: skipped: previous backup still running", name, at.Format("15:04"))
		return
	case errors.Is(err, state.ErrConflict):
		return
	case err != nil:
		// A refusal was recorded as a failed run; anything else is not worth a record.
		s.logf("backups: %s at %s: cannot start: %v", name, at.Format("15:04"), err)
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.audit(ctx, name, "started")
		res := p.Execute(ctx)
		s.audit(ctx, name, res.Run.Status)
	}()
}

// audit records a step of a scheduled run in the audit log, with the backup as the caller.
func (s *Scheduler) audit(ctx context.Context, name, outcome string) {
	actor := "backup:" + name
	if err := s.Store.RecordAudit(context.WithoutCancel(ctx), state.Actor{ID: actor, Name: actor}, "backup_run:"+name, outcome); err != nil {
		s.logf("backups: cannot audit the run of %s: %v", name, err)
	}
}
