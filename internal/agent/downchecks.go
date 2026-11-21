package agent

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"slices"
	"time"

	"berthy/internal/api"
	"berthy/internal/manifest"
	"berthy/internal/monitor"
)

// downQuietKinds are the operations that change the containers of an app. Its down checks are not judged while one of them
// runs, nor for monitor.DownQuietAfter after it ends. A restore writes a volume and changes no container, so it is not one.
// A destroy quiets every app while it runs.
var downQuietKinds = []string{"deploy", "rollback", "restart", "scale", "remove", "ingress"}

// downApps returns the apps the down checks observe: the managed apps, and every Compose project of the manifest that has no
// containers, with no roles, so that its services count as stopped.
func downApps(managed []api.App, m *manifest.Manifest, environment string) []api.App {
	out := slices.Clone(managed)
	if m == nil {
		return out
	}
	present := map[api.AppRef]bool{}
	for _, app := range managed {
		present[app.Ref] = true
	}
	for _, name := range slices.Sorted(maps.Keys(m.Apps)) {
		ref := api.AppRef{Kind: "compose", Name: m.ComposeProject(environment, name)}
		if m.Apps[name].Kind == "compose" && !present[ref] {
			out = append(out, api.App{Ref: ref})
		}
	}
	return out
}

// quietApps returns which apps are not judged at now. The app of a running operation of downQuietKinds is quiet, and so is
// the app of an operation of those kinds with an event within monitor.DownQuietAfter of now. While a destroy runs, every app
// is quiet. An error means the operations could not be read, and nothing is judged then.
func (w *Monitoring) quietApps(ctx context.Context, m *manifest.Manifest, now time.Time) (func(api.AppRef) bool, error) {
	quiet := map[api.AppRef]bool{}
	active, err := w.store.Active(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		if active.Kind == "destroy" {
			return func(api.AppRef) bool { return true }, nil
		}
		if slices.Contains(downQuietKinds, active.Kind) {
			quiet[opAppRef(m, w.cfg.Environment, active.App)] = true
		}
	}
	ended, err := w.store.LatestEvents(ctx, downQuietKinds, now.Add(-monitor.DownQuietAfter))
	if err != nil {
		return nil, err
	}
	for app := range ended {
		quiet[opAppRef(m, w.cfg.Environment, app)] = true
	}
	return func(ref api.AppRef) bool { return quiet[ref] }, nil
}

// opAppRef names the app of an operation the way the monitor names it: a Compose app by its project, any other by its name.
func opAppRef(m *manifest.Manifest, environment, app string) api.AppRef {
	if m != nil {
		if a, ok := m.Apps[app]; ok && a.Kind == "compose" {
			return api.AppRef{Kind: "compose", Name: m.ComposeProject(environment, app)}
		}
	}
	return api.AppRef{Kind: "image", Name: app}
}
