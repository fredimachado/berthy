package manifest

import (
	"slices"
	"sort"
)

// SecretNeed is a secret the manifest names and what needs it, such as "app api", "registry ghcr.io" or "telemetry". NeededBy is
// sorted and has no repeats.
type SecretNeed struct {
	Name     string   `json:"name"`
	NeededBy []string `json:"needed_by"`
}

// SecretNeeds lists every secret the manifest names, sorted by name, each with what needs it. It covers the variables of every
// app (plain names and mappings), the registry passwords, the destination keys, the telemetry headers, the Telegram bot token
// and the managed ingress token. A manifest that names no secret gives an empty list.
func (m *Manifest) SecretNeeds() []SecretNeed {
	by := map[string][]string{}
	add := func(name, need string) {
		if name != "" {
			by[name] = append(by[name], need)
		}
	}
	for _, app := range sortedKeys(m.Apps) {
		for _, name := range m.Apps[app].Env.SecretNames() {
			add(name, "app "+app)
		}
	}
	for _, reg := range sortedKeys(m.Registries) {
		add(m.Registries[reg].PasswordSecret, "registry "+reg)
	}
	for _, dest := range sortedKeys(m.Destinations) {
		d := m.Destinations[dest]
		add(d.AccessKeySecret, "destination "+dest)
		add(d.SecretKeySecret, "destination "+dest)
	}
	if m.Telemetry != nil {
		add(m.Telemetry.HeadersSecret, "telemetry")
	}
	if m.Alerts != nil && m.Alerts.Telegram != nil {
		add(m.Alerts.Telegram.BotTokenSecret, "alerts telegram")
	}
	if m.Ingress != nil && m.Ingress.Cloudflare != nil {
		add(m.Ingress.Cloudflare.APITokenSecret, "ingress cloudflare")
	}
	needs := make([]SecretNeed, 0, len(by))
	for name, who := range by {
		slices.Sort(who)
		needs = append(needs, SecretNeed{Name: name, NeededBy: slices.Compact(who)})
	}
	sort.Slice(needs, func(i, j int) bool { return needs[i].Name < needs[j].Name })
	return needs
}
