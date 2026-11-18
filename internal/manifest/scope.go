package manifest

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

// A manifest has two scopes. The app scope of an app is apps.<app>, the jobs and backups that name it, and the service probes
// aimed at it: a deploy of the app applies it, and a rollback of the app takes it back. The environment scope is everything
// else: the project, the network, the registries, the destinations, the state backup, telemetry, peers, alerts, the probes that
// name no app, and the rest. A deploy of any app, and berthy apply, applies it, and a rollback never changes it.

// EnvScope returns m without its apps, and without the jobs, backups and probes that name an app.
func (m Manifest) EnvScope() Manifest {
	m.Apps, m.Jobs, m.Backups = nil, nil, nil
	m.Probes = filterMap(m.Probes, func(p Probe) bool { return p.Service == nil })
	return m
}

// AppScope returns the part of m that belongs to app: the app, its jobs and backups, and the probes aimed at it. It is empty
// when m does not name the app.
func (m Manifest) AppScope(app string) Manifest {
	var out Manifest
	if a, ok := m.Apps[app]; ok {
		out.Apps = map[string]App{app: a}
	}
	out.Jobs = filterMap(m.Jobs, func(j Job) bool { return j.App == app })
	out.Backups = filterMap(m.Backups, func(b Backup) bool { return b.App == app })
	out.Probes = filterMap(m.Probes, func(p Probe) bool { return p.Service != nil && p.Service.App == app })
	return out
}

// Assemble is the manifest the agent runs from: the environment's settings, with each app's part from that app's own manifest.
// apps maps an app's name to a manifest that holds its scope, such as AppScope of its applied release. The result is not
// validated: a reference to a destination or a job that is gone is reported by whatever needs it. When two apps name a job
// or a backup alike, the app whose name sorts last wins.
func Assemble(env Manifest, apps map[string]Manifest) Manifest {
	out := env
	out.Apps, out.Jobs, out.Backups = nil, nil, nil
	out.Probes = maps.Clone(env.Probes)
	for _, name := range slices.Sorted(maps.Keys(apps)) {
		part := apps[name]
		out.Apps = union(out.Apps, part.Apps)
		out.Jobs = union(out.Jobs, part.Jobs)
		out.Backups = union(out.Backups, part.Backups)
		out.Probes = union(out.Probes, part.Probes)
	}
	return out
}

// filterMap returns the entries of in that keep accepts, or nil when there are none.
func filterMap[V any](in map[string]V, keep func(V) bool) map[string]V {
	var out map[string]V
	for k, v := range in {
		if keep(v) {
			if out == nil {
				out = map[string]V{}
			}
			out[k] = v
		}
	}
	return out
}

// union adds the entries of add to into, which it creates when it is nil and add has entries.
func union[V any](into, add map[string]V) map[string]V {
	if len(add) == 0 {
		return into
	}
	if into == nil {
		into = make(map[string]V, len(add))
	}
	maps.Copy(into, add)
	return into
}

// DecodeSettings decodes the environment scope that the agent stored, and applies the defaults a stored manifest takes.
func DecodeSettings(data []byte) (Manifest, error) {
	var m Manifest
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("stored settings unreadable: %w", err)
	}
	m.defaults()
	return m, nil
}

// SettingsChanges lists what applying next instead of prev changes in the environment scope, one line each, such as
// "alerts: changed", "probes: + api-up", "destinations: ~ offsite", "registries: - ghcr.io" and "network: a -> b". The app-scoped
// members of either manifest are not compared. A line names a secret by name at most, never its value: a manifest holds
// only names.
func SettingsChanges(prev, next Manifest) []string {
	prev, next = prev.EnvScope(), next.EnvScope()
	var out []string
	scalar := func(name, a, b string) {
		if a != b {
			out = append(out, fmt.Sprintf("%s: %s -> %s", name, a, b))
		}
	}
	scalar("project", prev.Project, next.Project)
	scalar("network", prev.Network, next.Network)
	scalar("protected", fmt.Sprint(prev.Protected), fmt.Sprint(next.Protected))
	out = append(out, mapChanges("registries", prev.Registries, next.Registries)...)
	out = append(out, mapChanges("destinations", prev.Destinations, next.Destinations)...)
	out = append(out, pointerChange("state_backup", prev.StateBackup, next.StateBackup)...)
	out = append(out, pointerChange("telemetry", prev.Telemetry, next.Telemetry)...)
	out = append(out, mapChanges("probes", prev.Probes, next.Probes)...)
	out = append(out, mapChanges("peers", prev.Peers, next.Peers)...)
	out = append(out, pointerChange("alerts", prev.Alerts, next.Alerts)...)
	out = append(out, pointerChange("ingress", prev.Ingress, next.Ingress)...)
	out = append(out, mapChanges("aliases", prev.Aliases, next.Aliases)...)
	scalar("min_version", prev.MinVersion, next.MinVersion)
	scalar("timezone", cmp.Or(prev.Timezone, "UTC"), cmp.Or(next.Timezone, "UTC"))
	return out
}

// mapChanges names the entries of next that prev lacks (+), the ones next lacks (-) and the ones that differ (~).
func mapChanges[V any](name string, prev, next map[string]V) []string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(next)) {
		p, ok := prev[k]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("%s: + %s", name, k))
		case !sameJSON(p, next[k]):
			out = append(out, fmt.Sprintf("%s: ~ %s", name, k))
		}
	}
	for _, k := range slices.Sorted(maps.Keys(prev)) {
		if _, ok := next[k]; !ok {
			out = append(out, fmt.Sprintf("%s: - %s", name, k))
		}
	}
	return out
}

// pointerChange says that an optional section was added, removed or changed.
func pointerChange[V any](name string, prev, next *V) []string {
	switch {
	case prev == nil && next == nil:
		return nil
	case prev == nil:
		return []string{name + ": added"}
	case next == nil:
		return []string{name + ": removed"}
	case !sameJSON(*prev, *next):
		return []string{name + ": changed"}
	}
	return nil
}

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
