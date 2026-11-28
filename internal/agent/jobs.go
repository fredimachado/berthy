package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/jobs"
	"berthy/internal/manifest"
	"berthy/internal/monitor"
	"berthy/internal/schedule"
	"berthy/internal/state"
)

// NewJobs returns the scheduler of an agent with state and Docker, or nil for an agent without either. Its jobs come from
// the running manifest. log receives operational messages, which never contain secrets.
func NewJobs(c Config, e *engine.Engine, d *deploy.Controller, log func(string)) *jobs.Scheduler {
	if e == nil || d == nil || d.Store == nil {
		return nil
	}
	store := d.Store
	return &jobs.Scheduler{
		Env:   c.Environment,
		Store: store,
		Manifest: func(ctx context.Context) (manifest.Manifest, error) {
			m, err := runningManifest(ctx, store)
			return m, err
		},
		Runner: jobs.RunnerFunc(d.RunJob),
		Log:    log,
	}
}

// appJobs returns the jobs of the running manifest that belong to an app of the inventory, or nil when there are
// none or the manifest cannot be read. An image app is listed under its manifest name, and a Compose app under its
// Compose project name.
func appJobs(ctx context.Context, d *deploy.Controller, app api.App, now time.Time) []api.JobStatus {
	if d == nil || d.Store == nil {
		return nil
	}
	m, err := runningManifest(ctx, d.Store)
	if err != nil {
		return nil
	}
	name := manifestAppName(m, d.Environment, app.Ref)
	if name == "" {
		return nil
	}
	list, err := jobs.Statuses(ctx, d.Store, m, name, now)
	if err != nil || len(list) == 0 {
		return nil
	}
	return list
}

// manifestAppName returns the name the manifest gives the app of an inventory entry, or "" when it names none. An image app is
// listed under its manifest name, and a Compose app under its Compose project name.
func manifestAppName(m manifest.Manifest, environment string, ref api.AppRef) string {
	for n, a := range m.Apps {
		switch {
		case ref.Kind == "image" && a.Kind == "image" && n == ref.Name,
			ref.Kind == "compose" && a.Kind == "compose" && m.ComposeProject(environment, n) == ref.Name:
			return n
		}
	}
	return ""
}

// appBackups returns the backups of the running manifest that belong to an app of the inventory, with their ages, or nil
// when there are none or the manifest cannot be read.
func appBackups(ctx context.Context, d *deploy.Controller, app api.App, now time.Time) []api.BackupStatus {
	if d == nil || d.Store == nil {
		return nil
	}
	m, err := runningManifest(ctx, d.Store)
	if err != nil {
		return nil
	}
	name := manifestAppName(m, d.Environment, app.Ref)
	if name == "" {
		return nil
	}
	list, err := backupStatuses(ctx, d.Store, m, name, now)
	if err != nil || len(list) == 0 {
		return nil
	}
	return list
}

// jobCheck reports failures and scheduled occurrences that have not been handled.
func jobCheck(store *state.Store, name string, sched schedule.Schedule, firstSeen time.Time, now func() time.Time) monitor.Check {
	return monitor.NewFunc("job:"+name, 30*time.Second, 1, func(ctx context.Context) monitor.Result {
		last, err := store.LastJobRuns(ctx)
		if err != nil {
			return monitor.Result{State: monitor.Unknown, Detail: "job runs unavailable"}
		}
		occurrence, err := store.LastJobOccurrence(ctx, name)
		if err != nil {
			return monitor.Result{State: monitor.Unknown, Detail: "job runs unavailable"}
		}
		run, ok := last[name]
		if ok && run.Status == "failed" {
			return monitor.Result{State: monitor.Critical, Detail: run.Detail + jobs.LogsHint(name, run)}
		}
		baseline := firstSeen
		if !occurrence.IsZero() && sched.Next(occurrence.Add(-time.Minute)).Equal(occurrence) {
			baseline = occurrence
		}
		slot := sched.Next(baseline)
		if !slot.IsZero() && now().After(occurrenceDeadline(sched, slot)) {
			return monitor.Result{State: monitor.Warning, Detail: "missed scheduled run at " + slot.UTC().Format(time.RFC3339)}
		}
		if !ok {
			return monitor.Result{State: monitor.Unknown, Detail: "not run yet"}
		}
		return monitor.Result{State: monitor.OK, Detail: "last run succeeded"}
	})
}

func (rt *routeRuntime) handleJobs(call routeCall) {
	w, ctx := call.w, call.r.Context()
	d := rt.d
	m, err := runningManifest(ctx, d.Store)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, 503, "state_unavailable", "state unavailable")
		return
	}
	list, err := jobs.Statuses(ctx, d.Store, m, "", time.Now())
	if err != nil {
		writeError(w, 503, "state_unavailable", "state unavailable")
		return
	}
	json.NewEncoder(w).Encode(list)
}

func (rt *routeRuntime) handleJobRuns(call routeCall) {
	w, r, rest := call.w, call.r, call.rest
	ctx, d := r.Context(), rt.d
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeError(w, 400, "bad_request", "invalid limit")
			return
		}
		limit = n
	}
	runs, err := d.Store.JobRuns(ctx, rest[1], limit)
	if err != nil {
		writeError(w, 503, "state_unavailable", "state unavailable")
		return
	}
	if len(runs) == 0 {
		// A job with no runs is known only through the manifest.
		if m, err := runningManifest(ctx, d.Store); err != nil {
			writeError(w, 404, "not_found", "not found")
			return
		} else if _, ok := m.Jobs[rest[1]]; !ok {
			writeError(w, 404, "not_found", "not found")
			return
		}
	}
	out := make([]api.JobRun, 0, len(runs))
	for _, run := range runs {
		out = append(out, jobs.RunStatus(run))
	}
	json.NewEncoder(w).Encode(out)
}

func (rt *routeRuntime) handleJobLogs(call routeCall) {
	w, r, rest := call.w, call.r, call.rest
	ctx, d := r.Context(), rt.d
	var id int64
	if v := r.URL.Query().Get("run"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			writeError(w, 400, "bad_request", "invalid run")
			return
		}
		id = n
	}
	run, output, err := d.Store.JobOutput(ctx, rest[1], id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "not_found", "no output kept")
		return
	}
	if err != nil {
		writeError(w, 503, "state_unavailable", "state unavailable")
		return
	}
	logs := api.JobLogs{Job: run.Job, RunID: run.ID, Status: run.Status, Detail: run.Detail, Output: output}
	if !run.Finished.IsZero() {
		logs.Finished = &run.Finished
	}
	json.NewEncoder(w).Encode(logs)
}

func (rt *routeRuntime) handleJobCancel(call routeCall) {
	w, ctx, rest := call.w, call.r.Context(), call.rest
	d, sched, actor := rt.d, rt.sched, call.stateActor()
	if sched == nil {
		writeError(w, 503, "jobs_unavailable", "jobs unavailable")
		return
	}
	// A job has at most one run at a time, so its name says which run to stop.
	err := sched.Cancel(ctx, rest[1], actor.Name)
	switch {
	case errors.Is(err, jobs.ErrUnknownJob):
		writeError(w, 404, "not_found", "not found")
		return
	case errors.Is(err, jobs.ErrJobNotRunning):
		_ = d.Store.RecordAudit(ctx, actor, "job_cancel:"+rest[1], "refused")
		writeError(w, 409, "cancel_refused", "job is not running")
		return
	case err != nil:
		writeError(w, 503, "state_unavailable", "state unavailable")
		return
	}
	_ = d.Store.RecordAudit(ctx, actor, "job_cancel:"+rest[1], "accepted")
	json.NewEncoder(w).Encode(map[string]any{"job": rest[1], "accepted": true})
}

func (rt *routeRuntime) handleJobRun(call routeCall) {
	w, ctx, rest := call.w, call.r.Context(), call.rest
	d, sched, actor := rt.d, rt.sched, call.stateActor()
	if sched == nil {
		writeError(w, 503, "jobs_unavailable", "jobs unavailable")
		return
	}
	id, err := sched.Start(ctx, rest[1], "manual")
	switch {
	case errors.Is(err, jobs.ErrUnknownJob):
		writeError(w, 404, "not_found", "not found")
		return
	case errors.Is(err, jobs.ErrJobRunning):
		writeError(w, 409, "conflict", "job is already running")
		return
	case err != nil:
		writeError(w, 503, "state_unavailable", "state unavailable")
		return
	}
	// The run itself is audited as the job; this records who asked for it.
	_ = d.Store.RecordAudit(ctx, actor, "job_start:"+rest[1], "started")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]int64{"run_id": id})
}
