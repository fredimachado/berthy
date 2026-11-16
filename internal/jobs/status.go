package jobs

import (
	"context"
	"maps"
	"slices"
	"time"

	"berthy/internal/api"
	"berthy/internal/manifest"
	"berthy/internal/schedule"
	"berthy/internal/state"
)

// RunStatus converts a stored run to its API form.
func RunStatus(r state.JobRun) api.JobRun {
	out := api.JobRun{ID: r.ID, Job: r.Job, Trigger: r.Trigger, Started: r.Started, Status: r.Status, ExitCode: r.ExitCode, Detail: r.Detail, Container: r.Container}
	if !r.Scheduled.IsZero() {
		t := r.Scheduled
		out.Scheduled = &t
	}
	if !r.Finished.IsZero() {
		t := r.Finished
		out.Finished = &t
	}
	return out
}

// TargetName returns the role or Compose service a job runs in. An image app's job may leave its role out when the app
// has one role; the result is empty for a Compose job that names no service.
func TargetName(m manifest.Manifest, job manifest.Job) string {
	if job.Service != "" {
		return job.Service
	}
	if job.Role != "" {
		return job.Role
	}
	if app, ok := m.Apps[job.App]; ok && app.Kind == "image" && len(app.Roles) == 1 {
		return slices.Sorted(maps.Keys(app.Roles))[0]
	}
	return ""
}

// Statuses returns the jobs of the manifest, sorted by name, each with its newest run of any status and, when it has a
// schedule, its next occurrence after now. app limits the result to one app's jobs; empty means every job.
func Statuses(ctx context.Context, store *state.Store, m manifest.Manifest, app string, now time.Time) ([]api.JobStatus, error) {
	out := []api.JobStatus{}
	for _, name := range slices.Sorted(maps.Keys(m.Jobs)) {
		job := m.Jobs[name]
		if app != "" && job.App != app {
			continue
		}
		st := api.JobStatus{Name: name, App: job.App, Target: TargetName(m, job), Schedule: job.Schedule, Timeout: job.TimeoutDuration().String()}
		if job.Schedule != "" {
			if sched, err := schedule.Parse(job.Schedule); err == nil {
				if next := sched.Next(now); !next.IsZero() {
					st.Next = &next
				}
			}
		}
		runs, err := store.JobRuns(ctx, name, 1)
		if err != nil {
			return nil, err
		}
		if len(runs) > 0 {
			last := RunStatus(runs[0])
			st.Last = &last
		}
		out = append(out, st)
	}
	return out, nil
}
