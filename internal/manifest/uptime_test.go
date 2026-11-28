package manifest

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const healthApp = `project: demo
network: demo
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    roles:
      worker:
`

const workerPath = "apps.api.roles.worker"

func loadWorkerHealth(t *testing.T, role, timeout string) (*Manifest, error) {
	t.Helper()
	base := healthApp + role
	if timeout != "" {
		base += "\n    deploy:\n      deploy_timeout: " + timeout
	}
	return Load(writeFiles(t, map[string]string{"berthy.yml": base}), "prod")
}

func requireHealthProblem(t *testing.T, err error, path, message string) {
	t.Helper()
	var problems Problems
	if !errors.As(err, &problems) {
		t.Fatalf("got %v, want a problem at %s", err, path)
	}
	for _, p := range problems {
		if p.Path == path && strings.Contains(p.Message, message) {
			return
		}
	}
	t.Fatalf("got %+v, want %s containing %q", []Problem(problems), path, message)
}

func TestUptimeHealthFormsAndBudget(t *testing.T) {
	for _, tc := range []struct{ name, health, timeout, field, message string }{
		{name: "inherits image check"},
		{name: "command check", health: `{test: [CMD, 'true'], interval: 1ns, start_interval: 2ns}`},
		{"empty command", `{test: []}`, "", ".test", "invalid health"},
		{"neither", `{}`, "", ".test", "invalid health"},
		{"both", `{test: [], uptime: 20s}`, "", "", "choose exactly one"},
		{"unknown form", `{ready: 20s}`, "", ".ready", "unknown field"},
		{"empty uptime", `{uptime: ''}`, "", ".uptime", "positive duration"},
		{"zero uptime", `{uptime: 0s}`, "", ".uptime", "positive duration"},
		{"negative uptime", `{uptime: -1s}`, "", ".uptime", "positive duration"},
		{"threshold above default timeout", "{uptime: 121s}", "", ".uptime", "shorter than deploy_timeout"},
		{name: "implicit timeout permits threshold", health: "{uptime: 119.999s}"},
		{"implicit timeout equality", "{uptime: 120s}", "", ".uptime", "shorter than deploy_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			role := "        port: 0"
			if tc.health != "" {
				role = "        health: " + tc.health
			}
			_, err := loadWorkerHealth(t, role, tc.timeout)
			if tc.message != "" {
				requireHealthProblem(t, err, workerPath+".health"+tc.field, tc.message)
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("default fits 21s", func(t *testing.T) {
		m, err := loadWorkerHealth(t, "        health: {uptime: 20s}", "21s")
		if err != nil {
			t.Fatal(err)
		}
		h := m.Apps["api"].Roles["worker"].Health
		if threshold, err := h.UptimeThreshold(); err != nil || threshold != 20*time.Second {
			t.Fatalf("uptime threshold = %s, %v", threshold, err)
		}
	})
	t.Run("Compose role", func(t *testing.T) {
		base := strings.Replace(healthApp, "kind: image\n    image: ghcr.io/acme/api", "kind: compose\n    file: compose.yml", 1)
		_, err := Load(writeFiles(t, map[string]string{"berthy.yml": base + "        health: {uptime: 20s}"}), "prod")
		requireHealthProblem(t, err, workerPath+".health.uptime", "image apps")
	})
}

func TestUptimeHealthRoundTripAndOverlay(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"berthy.yml": healthApp + `        health: {test: [CMD, 'true'], timeout: 500ms, retries: 4}`,
		"berthy.prod.yml": `apps:
  api:
    roles:
      worker:
        health: {test: null, interval: null, timeout: null, start_period: null, start_interval: null, retries: null, uptime: 20s}
`,
	})
	m, err := Load(dir, "prod")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"health":{"uptime":"20s"}`) {
		t.Fatalf("wrong health form in stored manifest: %s", data)
	}
	release, err := DecodeRelease(data, false)
	if err != nil {
		t.Fatal(err)
	}
	h := release.Apps["api"].Roles["worker"].Health
	if h == nil || h.Test != nil || h.Timeout != "" || h.Retries != nil {
		t.Fatalf("wrong release health: %+v", h)
	}
	if threshold, err := h.UptimeThreshold(); err != nil || threshold != 20*time.Second {
		t.Fatalf("round-trip threshold = %s, %v", threshold, err)
	}
}

func TestUptimeHealthThresholdAndCommandOptions(t *testing.T) {
	for _, tc := range []struct{ health, timeout, field string }{
		{"{uptime: 19.999s}", "20s", ""},
		{"{uptime: 20s}", "20s", ".uptime"},
		{"{uptime: 1s, interval: ''}", "20s", ".interval"},
		{"{uptime: 1s, interval: 1s}", "20s", ".interval"},
		{"{uptime: 1s, timeout: 1s}", "20s", ".timeout"},
		{"{uptime: 1s, start_period: 1s}", "20s", ".start_period"},
		{"{uptime: 1s, start_interval: 1s}", "20s", ".start_interval"},
		{"{uptime: 1s, retries: 1}", "20s", ".retries"},
	} {
		t.Run(tc.health, func(t *testing.T) {
			m, err := loadWorkerHealth(t, "        health: "+tc.health, tc.timeout)
			if tc.field != "" {
				requireHealthProblem(t, err, workerPath+".health"+tc.field, "")
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			h := m.Apps["api"].Roles["worker"].Health
			if h.Timeout != "" || h.Retries != nil {
				t.Fatalf("unexpected command defaults: %+v", h)
			}
		})
	}
}

func TestUptimeAllowsNamedVolumeAtFormerProbePath(t *testing.T) {
	_, err := loadWorkerHealth(t, "        health: {uptime: 20s}\n        volumes: ['data:/.berthy:ro']", "")
	if err != nil {
		t.Fatal(err)
	}
}
