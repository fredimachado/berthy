package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"berthy/internal/manifest"
)

func TestUptimePlanIsStableAndShowsProbe(t *testing.T) {
	m := uptimeManifest("20s", "1m")
	app := m.Apps["web"]
	app.Roles["aux"] = uptimeManifest("12s", "1m").Apps["web"].Roles["web"]
	s := openStore(t)
	c := &Controller{Store: s, Engine: &boundFake{}, Environment: "test", ProbeSource: testProbeSource}
	ctx := context.Background()
	actor := Actor{ID: "admin", Admin: true}
	req := Request{App: "web", Version: digestOfLetter("b"), DryRun: true, Manifest: m}
	want := []string{
		`role aux: uptime 12s, health check ["CMD","/.berthy/berthy","probe","uptime","12s"], read-only bind /usr/local/bin/berthy -> /.berthy/berthy`,
		`role web: uptime 20s, health check ["CMD","/.berthy/berthy","probe","uptime","20s"], read-only bind /usr/local/bin/berthy -> /.berthy/berthy`,
	}
	check := func(t *testing.T, req Request) []byte {
		t.Helper()
		plan, err := c.Plan(ctx, actor, req)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(plan.Roles, []string{"aux", "web"}) || !slices.Equal(plan.Notes, want) {
			t.Errorf("roles %v, notes %q, want %q", plan.Roles, plan.Notes, want)
		}
		data, err := json.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	t.Run("first deploy", func(t *testing.T) {
		if first, again := check(t, req), check(t, req); !bytes.Equal(first, again) {
			t.Fatalf("plan changed from %s to %s", first, again)
		}
	})
	seedUptimeRelease(t, s, m)
	predecessor, err := s.Applied(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("update", func(t *testing.T) { check(t, req) })
	t.Run("rollback", func(t *testing.T) {
		check(t, Request{App: "web", Kind: "rollback", TargetRelease: predecessor.ID, DryRun: true})
	})
}

func TestReleaseDiffShowsUptimeTransitions(t *testing.T) {
	uptime, later := uptimeManifest("20s", "1m"), uptimeManifest("25s", "1m")
	command, inherited := dockerManifest(nil), dockerManifest(nil)
	for _, m := range []manifest.Manifest{command, inherited} {
		app := m.Apps["web"]
		app.Deploy.DeployTimeout = "1m"
		m.Apps["web"] = app
	}
	role := inherited.Apps["web"].Roles["web"]
	role.Health = nil
	inherited.Apps["web"].Roles["web"] = role
	for _, tc := range []struct {
		name         string
		from, to     manifest.Manifest
		uptime, bind string
	}{
		{"inherit to uptime", inherited, uptime, "health.uptime none -> 20s", "+ read-only probe bind /.berthy/berthy"},
		{"command to uptime", command, uptime, "health.uptime none -> 20s", "+ read-only probe bind /.berthy/berthy"},
		{"duration only", uptime, later, "health.uptime 20s -> 25s", ""},
		{"uptime to inherit", uptime, inherited, "health.uptime 20s -> none", "- read-only probe bind /.berthy/berthy"},
		{"uptime to command", uptime, command, "health.uptime 20s -> none", "- read-only probe bind /.berthy/berthy"},
		{name: "unchanged", from: uptime, to: uptime},
	} {
		t.Run(tc.name, func(t *testing.T) {
			diff := ReleaseDiff("web", &ReleaseSide{Manifest: tc.from}, &ReleaseSide{Manifest: tc.to})
			var changes []string
			for _, c := range diff.Changes {
				if strings.HasPrefix(c.Change, "health.uptime ") || strings.Contains(c.Change, "probe bind") {
					changes = append(changes, c.Change)
					if c.Area != "web" {
						t.Errorf("area %q for %q", c.Area, c.Change)
					}
				}
			}
			var want []string
			if tc.uptime != "" {
				want = append(want, tc.uptime)
			}
			if tc.bind != "" {
				want = append(want, tc.bind)
			}
			if !slices.Equal(changes, want) || diff.Same != (tc.uptime == "") {
				t.Fatalf("diff %+v, want uptime changes %q", diff, want)
			}
		})
	}
}

func TestAdoptionShowsDesiredOnlyUptimeMountAndCheck(t *testing.T) {
	for _, tc := range []struct {
		name            string
		volume, command bool
	}{
		{name: "absent live fields"}, {name: "unrelated named volume", volume: true}, {name: "existing command", command: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := adoptionRole()
			if tc.volume {
				old.Volumes = []string{"assets:/data"}
			}
			live := changedByHand(t, old, func(_, container, _ map[string]any) {
				if !tc.command {
					delete(container, "Healthcheck")
				}
			})
			want := old
			threshold := "20s"
			want.Health = &manifest.Health{Uptime: &threshold}
			c := Controller{Engine: loggingPositive{&positiveEngine{spec: live}, "json-file", nil}, Environment: "test", ProbeSource: testProbeSource}
			req := adoptionRequest(want)
			app := req.Manifest.Apps["web"]
			app.Deploy.DeployTimeout = "1m"
			req.Manifest.Apps["web"] = app
			plan, _, err := c.planAdoption(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Join(plan.Changes, "\n")
			if strings.Count(lines, `["CMD","/.berthy/berthy","probe","uptime","20s"]`) != 1 ||
				strings.Count(lines, "read-only bind /usr/local/bin/berthy -> /.berthy/berthy") != 1 ||
				strings.Contains(lines, "volumes ") || strings.Contains(lines, "...") {
				t.Fatalf("changes %q, want one full check and bind", plan.Changes)
			}
			if tc.command && !strings.Contains(lines, `(now ["CMD","true"])`) {
				t.Fatalf("old command missing: %q", plan.Changes)
			}
		})
	}
}
