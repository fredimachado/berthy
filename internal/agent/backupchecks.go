package agent

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"berthy/internal/api"
	"berthy/internal/manifest"
	"berthy/internal/monitor"
	"berthy/internal/schedule"
	"berthy/internal/state"
)

const (
	// backupCheckEvery is how often a backup's check looks at its runs.
	backupCheckEvery = time.Minute
	// maxBackupGrace caps how late a scheduled backup may be before it counts as overdue.
	maxBackupGrace = time.Hour
)

// backupResult decides the state of a backup's check from its history. loc is the zone its schedule is read in. firstSeen is when the agent first saw the backup in the
// manifest, from which a backup that never ran is given time to run. In this order:
//
//   - the newest finished run failed: critical, with its detail;
//   - a scheduled backup is overdue: critical;
//   - the newest success is under half the size of the one before: warning;
//   - a backup that never ran is unknown, so it never alerts; and any other is ok.
func backupResult(b manifest.Backup, loc *time.Location, h state.BackupHistory, firstSeen, now time.Time) monitor.Result {
	if h.Last != nil && h.Last.Status == "failed" {
		return monitor.Result{State: monitor.Critical, Detail: h.Last.Detail}
	}
	var lastSuccess *state.BackupRun
	if len(h.Successes) > 0 {
		lastSuccess = &h.Successes[0]
	}
	if b.Schedule != "" {
		if sched, err := schedule.ParseIn(b.Schedule, loc); err == nil {
			since := firstSeen
			if lastSuccess != nil {
				since = lastSuccess.Finished
			}
			if by := overdueBy(sched, since); !by.IsZero() && now.After(by) {
				if lastSuccess == nil {
					return monitor.Result{State: monitor.Critical, Detail: fmt.Sprintf("overdue: no success yet, first seen %s (expected by %s)", firstSeen.UTC().Format(time.RFC3339), by.Format(time.RFC3339))}
				}
				return monitor.Result{State: monitor.Critical, Detail: fmt.Sprintf("overdue: last success %s (expected by %s)", lastSuccess.Finished.UTC().Format(time.RFC3339), by.Format(time.RFC3339))}
			}
		}
	}
	if lastSuccess == nil {
		return monitor.Result{State: monitor.Unknown, Detail: "not run yet"}
	}
	if len(h.Successes) > 1 {
		prev := h.Successes[1].Size
		if prev > 0 && lastSuccess.Size*2 < prev {
			if pct := lastSuccess.Size * 100 / prev; pct > 0 {
				return monitor.Result{State: monitor.Warning, Detail: fmt.Sprintf("size dropped to %d%% of the previous backup", pct)}
			}
			return monitor.Result{State: monitor.Warning, Detail: "size dropped to under 1% of the previous backup"}
		}
	}
	return monitor.Result{State: monitor.OK, Detail: fmt.Sprintf("last success %s ago, %s", api.HumanAge(now.Sub(lastSuccess.Finished)), api.HumanSize(lastSuccess.Size))}
}

// overdueBy is the time after which a backup that last succeeded at since is overdue: its next occurrence, and a grace of the
// interval between that occurrence and the one after it, at most an hour. It is zero when the schedule has no next occurrence.
func overdueBy(sched schedule.Schedule, since time.Time) time.Time {
	next := sched.Next(since)
	if next.IsZero() {
		return time.Time{}
	}
	grace := maxBackupGrace
	if after := sched.Next(next); !after.IsZero() {
		grace = min(grace, after.Sub(next))
	}
	return next.Add(grace)
}

// backupSeen remembers when the agent first saw each backup. A backup that never ran is given its grace from then.
type backupSeen struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// first returns when name was first seen, noting now if it is the first sighting.
func (s *backupSeen) first(name string, now time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]time.Time{}
	}
	if t, ok := s.seen[name]; ok {
		return t
	}
	s.seen[name] = now
	return now
}

// backupCheck returns the monitor check of a backup of the manifest.
func backupCheck(store *state.Store, name string, b manifest.Backup, loc *time.Location, seen *backupSeen, now func() time.Time) monitor.Check {
	first := seen.first(name, now())
	return monitor.NewFunc("backup:"+name, backupCheckEvery, 1, func(ctx context.Context) monitor.Result {
		h, err := store.BackupHistory(ctx, name)
		if err != nil {
			return monitor.Result{State: monitor.Unknown, Detail: "backup runs unavailable"}
		}
		return backupResult(b, loc, h, first, now())
	})
}

// backupStatuses returns the backups of the manifest that belong to app, sorted by name, with when each last succeeded and
// how long ago. app is the manifest name of the app. An empty app means every backup.
func backupStatuses(ctx context.Context, store *state.Store, m manifest.Manifest, app string, now time.Time) ([]api.BackupStatus, error) {
	var out []api.BackupStatus
	for _, name := range slices.Sorted(maps.Keys(m.Backups)) {
		b := m.Backups[name]
		if app != "" && b.App != app {
			continue
		}
		h, err := store.BackupHistory(ctx, name)
		if err != nil {
			return nil, err
		}
		st := api.BackupStatus{Name: name, Volume: b.Volume, Schedule: b.Schedule}
		if len(h.Successes) > 0 {
			t := h.Successes[0].Finished
			st.LastSuccess, st.Size, st.Age = &t, h.Successes[0].Size, api.HumanAge(now.Sub(t))
		}
		if h.Last != nil {
			st.LastStatus = h.Last.Status
			if h.Last.Status == "failed" {
				st.Detail = h.Last.Detail
			}
		}
		out = append(out, st)
	}
	return out, nil
}
