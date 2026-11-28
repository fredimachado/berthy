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
		{"threshold overflow", `{uptime: 2562047h47m16.854775807s}`, "", ".uptime", "100ms"},
		{name: "implicit timeout permits first poll", health: `{uptime: 119s}`},
		{"implicit timeout equality", `{uptime: 119.9s}`, "", ".interval", "before deploy_timeout"},
		{"explicit timeout equality", `{uptime: 20s}`, "20.1s", ".interval", "before deploy_timeout"},
		{"different startup interval", `{uptime: 20s, interval: 1h, start_interval: 2s}`, "30s", ".start_interval", "portable Swarm scheduling"},
		{"ordinary poll misses deadline", `{uptime: 20s, interval: 1h}`, "30s", ".interval", "before deploy_timeout"},
		{"ordinary cadence rounds up", `{uptime: 20s, interval: 19s, start_period: 40s}`, "38s", ".interval", "before deploy_timeout"},
		{"startup cadence rounds up", `{uptime: 20s, interval: 7s}`, "25s", ".interval", "before deploy_timeout"},
		{name: "both cadences fit", health: `{uptime: 20s, interval: 7s, start_interval: 7000ms}`, timeout: "25.001s"},
		{"cadence overflow", `{uptime: 2000000h, interval: 1500000h}`, "2562047h47m16.854775807s", ".interval", "before deploy_timeout"},
		{"zero grace too short", `{uptime: 20s, interval: 5s, start_period: 0s}`, "", ".start_period", "20.1s"},
		{"positive grace too short", `{uptime: 20s, interval: 30s, start_period: 20s}`, "", ".start_period", "20.1s"},
		{name: "positive grace covers threshold", health: `{uptime: 20s, interval: 7s, start_period: 20.1s}`},
		{"interval below minimum", `{uptime: 20s, interval: 999us}`, "", ".interval", "1ms"},
		{"timeout below minimum", `{uptime: 20s, timeout: 999us}`, "", ".timeout", "1ms"},
		{"zero retries", `{uptime: 20s, retries: 0}`, "", ".retries", "positive"},
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
		want := UptimeTiming{Threshold: 20 * time.Second, Interval: 20100 * time.Millisecond}
		if timing, err := h.UptimeTiming(); err != nil || timing != want {
			t.Fatalf("uptime timing = %+v, %v, want %+v", timing, err, want)
		}
		if h.Timeout != "30s" || h.Retries == nil || *h.Retries != 3 {
			t.Fatalf("wrong timeout or retries defaults: %+v", h)
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
        health: {test: null, uptime: 20s, interval: 7s}
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
	if strings.Contains(string(data), `"test":`) || !strings.Contains(string(data), `"uptime":"20s"`) {
		t.Fatalf("wrong health form in stored manifest: %s", data)
	}
	release, err := DecodeRelease(data, false)
	if err != nil {
		t.Fatal(err)
	}
	h := release.Apps["api"].Roles["worker"].Health
	if h == nil || h.Test != nil || h.Timeout != "500ms" || h.Retries == nil || *h.Retries != 4 {
		t.Fatalf("wrong release health: %+v", h)
	}
	want := UptimeTiming{Threshold: 20 * time.Second, Interval: 7 * time.Second, StartPeriod: 20100 * time.Millisecond}
	for _, form := range []Health{
		{Uptime: h.Uptime, Interval: "7s"},
		*m.Apps["api"].Roles["worker"].Health,
		*h,
	} {
		if timing, err := form.UptimeTiming(); err != nil || timing != want {
			t.Fatalf("uptime timing = %+v, %v, want %+v", timing, err, want)
		}
	}
}

func TestUptimeHealthReservesMountDirectory(t *testing.T) {
	for _, tc := range []struct {
		target   string
		reserved bool
	}{
		{"/.berthy", true},
		{"/.berthy/berthy", true},
		{"/.berthy/sub/data", true},
		{"/.berthy-data", false},
	} {
		t.Run(tc.target, func(t *testing.T) {
			_, err := loadWorkerHealth(t, "        health: {uptime: 20s}\n        volumes: ['data:"+tc.target+":ro']", "")
			if tc.reserved {
				requireHealthProblem(t, err, workerPath+".volumes.0", "reserved")
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUptimeHealthThresholdAndCommandOptions(t *testing.T) {
	for _, tc := range []struct{ health, timeout, field string }{
		{"{uptime: 19.999s}", "20s", ""},
		{"{uptime: 20s}", "20s", ".uptime"},
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
