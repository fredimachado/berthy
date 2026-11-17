package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// deployedApps returns the apps of m that have an applied release. An app without one runs nothing: it was never deployed,
// or berthy remove took it down. The last applied manifest is the newest release of any app, so it can still name such an
// app.
func deployedApps(ctx context.Context, s *state.Store, m manifest.Manifest) (map[string]bool, error) {
	applied, err := s.AppliedApps(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, name := range applied {
		if _, ok := m.Apps[name]; ok {
			out[name] = true
		}
	}
	return out, nil
}

// deployedManifest is the last applied manifest without the apps that have no applied release, and without their jobs, their
// backups and the service probes aimed at them. Monitoring, scheduling and the app listings read it, so a removed app is never
// monitored or scheduled while the manifest still names it. Settings that belong to the environment, such as peers, alerts
// and telemetry, stay as they are.
func deployedManifest(ctx context.Context, s *state.Store) (manifest.Manifest, time.Time, error) {
	m, at, err := s.LatestManifest(ctx)
	if err != nil {
		return m, at, err
	}
	deployed, err := deployedApps(ctx, s, m)
	if err != nil {
		return manifest.Manifest{}, time.Time{}, err
	}
	m.Apps = filtered(m.Apps, func(name string, _ manifest.App) bool { return deployed[name] })
	m.Jobs = filtered(m.Jobs, func(_ string, j manifest.Job) bool { return deployed[j.App] })
	m.Probes = filtered(m.Probes, func(_ string, p manifest.Probe) bool { return p.Service == nil || deployed[p.Service.App] })
	m.Backups = filtered(m.Backups, func(_ string, b manifest.Backup) bool { return deployed[b.App] })
	return m, at, nil
}

// filtered returns the entries of in that keep accepts. A map with no entry left stays non-nil only when in was.
func filtered[V any](in map[string]V, keep func(string, V) bool) map[string]V {
	if in == nil {
		return nil
	}
	out := make(map[string]V, len(in))
	for k, v := range in {
		if keep(k, v) {
			out[k] = v
		}
	}
	return out
}

// orphanAppsCheck is the doctor's check of apps that are deployed but that the last applied manifest does not name. They
// keep running and keep their routes until berthy remove takes them down. It reports nothing when there are none.
func orphanAppsCheck(ctx context.Context, s *state.Store, m manifest.Manifest) (api.Check, bool) {
	applied, err := s.AppliedApps(ctx)
	if err != nil {
		return api.Check{}, false
	}
	var orphans []string
	for _, name := range applied {
		if _, ok := m.Apps[name]; !ok && name != manifest.ConnectorAppName {
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
		project := m.ComposeProject(app)
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
