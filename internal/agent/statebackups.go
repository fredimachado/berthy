package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/manifest"
	"berthy/internal/monitor"
	"berthy/internal/schedule"
	"berthy/internal/state"
	"berthy/internal/statebackup"
)

// stateBackupGrace is how late the state backup may be after its next occurrence before its check is critical.
const stateBackupGrace = 2 * time.Hour

// stateBackupResult decides the state of the state backup's check from its history. firstSeen is when the agent first saw the
// block, from which a state backup that never succeeded is given time. In this order: the newest finished run failed:
// critical; the occurrence after the last success, plus the grace, has passed: critical; no success yet: unknown; any
// other: ok.
func stateBackupResult(sb manifest.StateBackup, h state.StateBackupHistory, firstSeen, now time.Time) monitor.Result {
	if h.Last != nil && h.Last.Status == "failed" {
		return monitor.Result{State: monitor.Critical, Detail: h.Last.Detail}
	}
	since := firstSeen
	if h.Success != nil {
		since = h.Success.Finished
	}
	if sched, err := schedule.Parse(statebackup.Schedule(sb)); err == nil {
		if next := sched.Next(since); !next.IsZero() && now.After(next.Add(stateBackupGrace)) {
			if h.Success == nil {
				return monitor.Result{State: monitor.Critical, Detail: fmt.Sprintf("overdue: no snapshot yet, first seen %s (expected by %s)",
					firstSeen.UTC().Format(time.RFC3339), next.Format(time.RFC3339))}
			}
			return monitor.Result{State: monitor.Critical, Detail: fmt.Sprintf("overdue: last snapshot %s (expected by %s)",
				h.Success.Finished.UTC().Format(time.RFC3339), next.Format(time.RFC3339))}
		}
	}
	if h.Success == nil {
		return monitor.Result{State: monitor.Unknown, Detail: "not run yet"}
	}
	return monitor.Result{State: monitor.OK, Detail: fmt.Sprintf("last snapshot %s ago, %s", api.HumanAge(now.Sub(h.Success.Finished)), api.HumanSize(h.Success.Size))}
}

// stateBackupCheck returns the monitor check of the state backup. It exists only while the running manifest has the block.
func stateBackupCheck(store *state.Store, sb manifest.StateBackup, seen *backupSeen, now func() time.Time) monitor.Check {
	first := seen.first("state_backup", now())
	return monitor.NewFunc("state_backup", backupCheckEvery, 1, func(ctx context.Context) monitor.Result {
		h, err := store.StateBackupHistory(ctx)
		if err != nil {
			return monitor.Result{State: monitor.Unknown, Detail: "state backup runs unavailable"}
		}
		return stateBackupResult(sb, h, first, now())
	})
}

// startState continues an admitted state backup in the background, on a context that ends with the agent. The start and the
// outcome are audited as the actor that asked.
func (b *Backups) startState(p *statebackup.Pending, actor state.Actor) {
	b.mu.Lock()
	base := b.base
	b.mu.Unlock()
	if base == nil {
		base = context.Background()
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.auditState(base, actor, "started")
		res := p.Execute(base)
		b.auditState(base, actor, res.Run.Status)
	}()
}

// auditState records one step of a state backup in the audit log.
func (b *Backups) auditState(ctx context.Context, actor state.Actor, outcome string) {
	if err := b.store.RecordAudit(context.WithoutCancel(ctx), actor, "state_backup", outcome); err != nil {
		b.logf("state backup: cannot audit the run: %v", err)
	}
}

// stateBackupError answers a request that the state backup refused.
func stateBackupError(w http.ResponseWriter, err error) {
	var refusal statebackup.Refusal
	switch {
	case errors.Is(err, statebackup.ErrNotConfigured):
		writeError(w, 404, "not_found", string(statebackup.ErrNotConfigured))
	case errors.Is(err, statebackup.ErrRunning):
		writeError(w, 409, "conflict", string(statebackup.ErrRunning))
	case errors.As(err, &refusal):
		writeError(w, 400, "operation_failed", string(refusal))
	default:
		writeError(w, 503, "state_unavailable", "state unavailable")
	}
}

// serveStateBackup answers the state backup routes: a manual run, which starts and returns at once with its ID, and the runs
// of the state backup, newest first. rest is the path after the environment, and the caller has checked the method and scope.
func serveStateBackup(w http.ResponseWriter, r *http.Request, rest []string, d *deploy.Controller, b *Backups, actor state.Actor) {
	ctx := r.Context()
	if b == nil || b.State == nil {
		writeError(w, 503, "backups_unavailable", "backups unavailable")
		return
	}
	if len(rest) == 2 {
		limit := 20
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 100 {
				writeError(w, 400, "bad_request", "invalid limit")
				return
			}
			limit = n
		}
		runs, err := d.Store.StateBackupRuns(ctx, limit)
		if err != nil {
			writeError(w, 503, "state_unavailable", "state unavailable")
			return
		}
		out := make([]api.StateBackupRun, 0, len(runs))
		for _, run := range runs {
			out = append(out, stateBackupRunStatus(run))
		}
		json.NewEncoder(w).Encode(out)
		return
	}
	m, err := recordedManifest(ctx, d.Store)
	if err != nil {
		writeError(w, 503, "state_unavailable", "state unavailable")
		return
	}
	p, err := b.State.Begin(ctx, m, "manual", time.Time{})
	if err != nil {
		stateBackupError(w, err)
		return
	}
	b.startState(p, actor)
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]int64{"state_backup_run_id": p.ID()})
}

func stateBackupRunStatus(r state.StateBackupRun) api.StateBackupRun {
	out := api.StateBackupRun{ID: r.ID, Key: r.Key, Trigger: r.Trigger, Started: r.Started, Status: r.Status, Size: r.Size, SHA256: r.SHA256, Detail: r.Detail}
	if !r.Finished.IsZero() {
		t := r.Finished
		out.Finished = &t
	}
	if !r.Scheduled.IsZero() {
		t := r.Scheduled
		out.Scheduled = &t
	}
	return out
}
