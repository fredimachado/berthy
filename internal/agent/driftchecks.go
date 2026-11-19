package agent

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/manifest"
	"berthy/internal/monitor"
	"berthy/internal/state"
)

const (
	// driftEvery is how often an app's drift check compares what runs with its release.
	driftEvery = 5 * time.Minute
	// driftTimeout bounds one comparison, which reads Docker and, for a Compose app, runs Compose.
	driftTimeout = 30 * time.Second
	// maxDriftDetail caps the detail of a drift check, so a service changed in many ways keeps one short line.
	maxDriftDetail = 400
)

// driftFunc compares what an app runs with its applied release.
type driftFunc func(ctx context.Context, app string) (deploy.DriftReport, error)

// driftResult is the state of the drift check of app. It is a warning, never critical, and it reports and corrects nothing.
// It is unknown while an operation on the app runs, because a deploy changes the services one by one and would only
// raise a warning that is gone a moment later, and while Docker or the release cannot be read.
func driftResult(ctx context.Context, s *state.Store, drift driftFunc, app string) monitor.Result {
	if operationRunsOn(ctx, s, app) {
		return monitor.Result{State: monitor.Unknown, Detail: "an operation is running on the app"}
	}
	dctx, cancel := context.WithTimeout(ctx, driftTimeout)
	defer cancel()
	report, err := drift(dctx, app)
	if err != nil {
		return monitor.Result{State: monitor.Unknown, Detail: "drift unknown: " + err.Error()}
	}
	// An operation that began while Docker was read may have changed a service half way, so what was read proves nothing.
	if operationRunsOn(ctx, s, app) {
		return monitor.Result{State: monitor.Unknown, Detail: "an operation is running on the app"}
	}
	if len(report.Lines) > 0 {
		return monitor.Result{State: monitor.Warning, Detail: capDetail(strings.Join(report.Lines, "; "))}
	}
	detail := "matches release " + report.Release
	if len(report.Scaled) > 0 {
		detail += "; " + strings.Join(report.Scaled, "; ")
	}
	return monitor.Result{State: monitor.OK, Detail: capDetail(detail)}
}

// operationRunsOn reports whether an operation that holds the environment is on app, or on none in particular. When the
// store cannot say, it answers yes, so nothing is judged on a guess.
func operationRunsOn(ctx context.Context, s *state.Store, app string) bool {
	op, err := s.Active(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	return err != nil || op.App == "" || op.App == app
}

// capDetail cuts a detail to maxDriftDetail characters.
func capDetail(s string) string {
	if r := []rune(s); len(r) > maxDriftDetail {
		return string(r[:maxDriftDetail]) + "..."
	}
	return s
}

// driftApps are the deployed apps whose drift is checked: every applied app except the ingress connector, which Berthy
// itself keeps.
func driftApps(ctx context.Context, s *state.Store) ([]string, error) {
	applied, err := s.AppliedApps(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range applied {
		if name != manifest.ConnectorAppName {
			out = append(out, name)
		}
	}
	return out, nil
}

// appDrift is the drift lines and the scaled roles that berthy describe shows for an app of the inventory, computed now. It
// returns none for an app that Berthy did not deploy, while an operation runs on the app, and when the comparison cannot be made.
func appDrift(ctx context.Context, d *deploy.Controller, app api.App) (lines, scaled []string) {
	if d == nil || d.Store == nil {
		return nil, nil
	}
	name := app.Ref.Name
	switch app.Ref.Kind {
	case "image":
	case "compose":
		m, err := runningManifest(ctx, d.Store)
		if err != nil {
			return nil, nil
		}
		if name = manifestAppName(m, app.Ref); name == "" {
			return nil, nil
		}
	default:
		return nil, nil
	}
	if operationRunsOn(ctx, d.Store, name) {
		return nil, nil
	}
	dctx, cancel := context.WithTimeout(ctx, driftTimeout)
	defer cancel()
	report, err := d.Drift(dctx, name)
	if err != nil {
		return nil, nil
	}
	return report.Lines, report.Scaled
}

// driftChecks returns the drift check of every deployed app. While the apps cannot be listed, the running checks stay as they are.
func (w *Monitoring) driftChecks(ctx context.Context) []monitor.Check {
	apps, err := driftApps(ctx, w.store)
	if err != nil {
		return nil
	}
	out := make([]monitor.Check, 0, len(apps))
	for _, app := range apps {
		out = append(out, monitor.NewFunc("drift:"+app, driftEvery, 1, func(ctx context.Context) monitor.Result {
			return driftResult(ctx, w.store, w.deploy.Drift, app)
		}))
	}
	return out
}

// driftDoctorChecks is the doctor's drift check of every deployed app. It compares now, so the doctor never shows an age. A
// warning is not OK and never critical.
func driftDoctorChecks(ctx context.Context, d *deploy.Controller) []api.Check {
	apps, err := driftApps(ctx, d.Store)
	if err != nil {
		return nil
	}
	out := make([]api.Check, 0, len(apps))
	for _, app := range apps {
		res := driftResult(ctx, d.Store, d.Drift, app)
		out = append(out, api.Check{Name: "drift:" + app, OK: res.State != monitor.Warning, Detail: res.State + ": " + res.Detail})
	}
	return out
}
