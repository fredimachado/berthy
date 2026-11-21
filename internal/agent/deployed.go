package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// settingsManifest is the environment's stored settings as a manifest without apps. It returns sql.ErrNoRows while nothing was
// ever applied. Whatever needs only the environment's own settings, such as the alerts or the telemetry, reads it.
func settingsManifest(ctx context.Context, s *state.Store) (manifest.Manifest, error) {
	st, ok, err := s.Settings(ctx)
	if err != nil {
		return manifest.Manifest{}, err
	}
	if !ok {
		return manifest.Manifest{}, sql.ErrNoRows
	}
	return manifest.DecodeSettings(st.Manifest)
}

// assembled is the environment's settings with the app scope of the newest release of each app that keep accepts. The
// ingress connector is not an app of the manifest.
func assembled(ctx context.Context, s *state.Store, keep func(app string) bool) (manifest.Manifest, error) {
	env, err := settingsManifest(ctx, s)
	if err != nil {
		return manifest.Manifest{}, err
	}
	releases, err := s.LatestReleases(ctx)
	if err != nil {
		return manifest.Manifest{}, err
	}
	parts := map[string]manifest.Manifest{}
	for app, r := range releases {
		if app == manifest.ConnectorAppName || !keep(app) {
			continue
		}
		m, err := manifest.DecodeRelease(r.Manifest, false)
		if err != nil {
			return manifest.Manifest{}, fmt.Errorf("stored manifest of %s unreadable: %w", app, err)
		}
		parts[app] = m.AppScope(app)
	}
	return manifest.Assemble(env, parts), nil
}

// runningManifest is the manifest the agent runs from: the environment's settings and, for each app that has an applied
// release, the app's part of that release. An app that has none runs nothing, because it was never deployed or berthy remove
// took it down, so its jobs, backups and probes are not here. Monitoring, scheduling and the app listings read it. It returns
// sql.ErrNoRows while nothing was ever applied.
func runningManifest(ctx context.Context, s *state.Store) (manifest.Manifest, error) {
	applied, err := s.AppliedApps(ctx)
	if err != nil {
		return manifest.Manifest{}, err
	}
	return assembled(ctx, s, func(app string) bool { return slices.Contains(applied, app) })
}

// recordedManifest is runningManifest with the app part of every app ever deployed, whether or not it is applied now. Listing
// and restoring read it, so that a backup stays restorable after berthy remove or destroy. Running a backup needs the app, and
// reads runningManifest instead. It is empty when nothing was ever applied.
func recordedManifest(ctx context.Context, s *state.Store) (manifest.Manifest, error) {
	m, err := assembled(ctx, s, func(string) bool { return true })
	if errors.Is(err, sql.ErrNoRows) {
		return manifest.Manifest{}, nil
	}
	return m, err
}

// orphanAppsCheck is the doctor's check of apps that are deployed but that the last apply or deploy did not name. They
// keep running and keep their routes until berthy remove takes them down. It reports nothing when there are none.
func orphanAppsCheck(ctx context.Context, s *state.Store) (api.Check, bool) {
	applied, err := s.AppliedApps(ctx)
	if err != nil {
		return api.Check{}, false
	}
	st, ok, err := s.Settings(ctx)
	if err != nil || !ok {
		return api.Check{}, false
	}
	var orphans []string
	for _, name := range applied {
		if !slices.Contains(st.Apps, name) && name != manifest.ConnectorAppName {
			orphans = append(orphans, name)
		}
	}
	if len(orphans) == 0 {
		return api.Check{}, false
	}
	slices.Sort(orphans)
	return api.Check{Name: "orphan_apps", Detail: fmt.Sprintf("deployed but not in the manifest: %s", strings.Join(orphans, ", "))}, true
}

// daemonConfigPath is the Docker daemon's configuration file. The doctor reads its log-opts from it.
var daemonConfigPath = "/etc/docker/daemon.json"

// daemonLogMaxSize is log-opts.max-size of the daemon's configuration, or "" when the file is missing or unreadable, is not
// JSON, or sets no size. A "" is no limit, and the warning says what to set; the doctor never fails on the file.
func daemonLogMaxSize() string {
	b, err := os.ReadFile(daemonConfigPath)
	if err != nil {
		return ""
	}
	var cfg struct {
		LogOpts map[string]string `json:"log-opts"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return ""
	}
	return cfg.LogOpts["max-size"]
}

// composeLogCheck is the doctor's warning about the services of deployed Compose apps whose logs have no size limit. Berthy
// does not rewrite a Compose file, so the operator sets the limit there or in the daemon's log-opts. It reports nothing
// when no Compose app is deployed, when every service has a limit, or when the Compose configuration or the daemon cannot
// be read. The check is never critical.
func composeLogCheck(ctx context.Context, d *deploy.Controller, e *engine.Engine, m manifest.Manifest) (api.Check, bool) {
	if d == nil || e == nil {
		return api.Check{}, false
	}
	var apps []string
	for name, a := range m.Apps {
		if a.Kind == "compose" {
			apps = append(apps, name)
		}
	}
	if len(apps) == 0 {
		return api.Check{}, false
	}
	slices.Sort(apps)
	driver, err := e.LoggingDriver(ctx)
	if err != nil {
		return api.Check{}, false
	}
	maxSize := daemonLogMaxSize()
	var unlimited []string
	for _, app := range apps {
		project := m.ComposeProject(d.Environment, app)
		services, err := d.ComposeServicesWithoutLogLimit(ctx, project, driver, maxSize)
		if err != nil {
			continue
		}
		for _, s := range services {
			unlimited = append(unlimited, project+"/"+s)
		}
	}
	if len(unlimited) == 0 {
		return api.Check{}, false
	}
	return api.Check{Name: "compose_log_limit", Detail: fmt.Sprintf("logs without a size limit: %s; set logging options in the Compose file, or log-opts max-size in /etc/docker/daemon.json", strings.Join(unlimited, ", "))}, true
}
