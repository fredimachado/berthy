package deploy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// writableWeb mounts a read-write volume on the web role of the test manifest.
func writableWeb(app *manifest.App) {
	r := app.Roles["web"]
	r.Volumes = []string{"uploads:/data"}
	app.Roles["web"] = r
}

func TestServiceSpecSendsTheUpdateOrderToUpdateAndRollback(t *testing.T) {
	one := 1
	r := manifest.Role{Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}
	for _, order := range []string{"start-first", "stop-first"} {
		b, err := serviceSpec("env", "proj", "app", "web", (&manifest.Manifest{Project: "proj"}).ServiceName("env", "app", "web"), "op", "network", "image", func(v string) string { return (&manifest.Manifest{Project: "proj"}).VolumeName("env", v) }, nil, r, order, "10s", nil, "", "")
		if err != nil {
			t.Fatal(err)
		}
		var v struct{ UpdateConfig, RollbackConfig map[string]any }
		if err = json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		if v.UpdateConfig["Order"] != order || v.RollbackConfig["Order"] != order {
			t.Fatalf("order %s: update %v, rollback %v", order, v.UpdateConfig["Order"], v.RollbackConfig["Order"])
		}
		// Only the order is decided by the app; the failure handling does not change with it.
		if v.UpdateConfig["FailureAction"] != "continue" || v.RollbackConfig["FailureAction"] != "rollback" {
			t.Fatalf("failure actions %v %v", v.UpdateConfig, v.RollbackConfig)
		}
	}
}

// planOf is the dry-run plan of the test manifest after change has edited its app.
func planOf(t *testing.T, change func(*manifest.App)) Plan {
	t.Helper()
	m := dockerManifest(nil)
	app := m.Apps["web"]
	change(&app)
	m.Apps["web"] = app
	s := openStore(t)
	c := &Controller{Engine: tagResolver{Engine: &boundFake{}, digest: "sha256:" + strings.Repeat("c", 64)}, Store: s, Environment: "test"}
	plan, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{RequestID: "plan", App: "web", Version: "v1.0.2", DryRun: true, Manifest: m})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestDryRunPlanSaysWhichRolesStopFirst(t *testing.T) {
	if got := planOf(t, writableWeb).Notes; len(got) != 1 || got[0] != "role web: stop-first, because it mounts volume uploads" {
		t.Fatalf("writable volume: notes %q", got)
	}
	if got := planOf(t, func(app *manifest.App) { app.Deploy.Order = "stop-first" }).Notes; len(got) != 1 || got[0] != "role web: stop-first, set by deploy.order" {
		t.Fatalf("deploy.order: notes %q", got)
	}
	if got := planOf(t, func(app *manifest.App) {}).Notes; len(got) != 0 {
		t.Fatalf("a start-first role has no note: %q", got)
	}
}

func TestFailedStopFirstRolloutSaysTheRoleWasDown(t *testing.T) {
	f := newDiagFake()
	f.tasksFor = func(generation, image string) []engine.ServiceTask {
		labels := map[string]string{"berthy.operation": generation}
		if strings.Contains(image, strings.Repeat("b", 64)) {
			return []engine.ServiceTask{{ID: "bad", ContainerID: "c-bad", State: "running", DesiredState: "running", Slot: 1, Labels: labels, Image: image, Timestamp: time.Now()}}
		}
		return []engine.ServiceTask{{ID: "good", ContainerID: "c-good", State: "running", DesiredState: "running", Slot: 1, Labels: labels, Image: image}}
	}
	probe := func(sec int) engine.HealthProbe {
		return engine.HealthProbe{Start: time.Date(2026, 11, 16, 10, 0, sec, 0, time.UTC), ExitCode: 1, Output: "not ready"}
	}
	f.diagnose = map[string]engine.ContainerDiagnosis{"c-good": {State: "running", Health: "healthy"}, "c-bad": {State: "running", Health: "starting", HealthLog: []engine.HealthProbe{probe(1), probe(2), probe(3)}}}
	f.tails = map[string]string{"c-bad": "2026-11-16T10:00:01Z stdout waiting\n"}
	c, s := diagController(t, f)
	if op := deployWeb(t, c, s, "first", "a", webManifest(testDeployTimeout, writableWeb)); op.State != "succeeded" {
		t.Fatalf("first deploy %s %s", op.State, op.Err)
	}
	op := deployWeb(t, c, s, "second", "b", webManifest("300ms", writableWeb))
	want := "role web: container never became healthy (last check: exit 1); role web was stopped first, so it was down until the previous version came back"
	if op.State != "rolled_back" || op.Err != "rollout failed and was cleaned up" || op.Reason != want {
		t.Fatalf("operation %s %q reason %q", op.State, op.Err, op.Reason)
	}
}

func TestStoppedFirstNoteCoversOnlyUpdatedRoles(t *testing.T) {
	app := manifest.App{Kind: "image", Deploy: manifest.Deploy{Order: "stop-first"}, Roles: map[string]manifest.Role{"web": {}, "worker": {}}}
	work := []roleWork{{intent: state.Intent{Role: "web", Kind: "update"}}, {intent: state.Intent{Role: "worker", Kind: "create"}}}
	if got := addStoppedFirst("rollout failed", app, work); got != "rollout failed; role web was stopped first, so it was down until the previous version came back" {
		t.Fatalf("reason %q", got)
	}
	created := []roleWork{{intent: state.Intent{Role: "worker", Kind: "create"}}}
	if got := addStoppedFirst("rollout failed", app, created); got != "rollout failed" {
		t.Fatalf("a created role was never down before: %q", got)
	}
}

func TestStoppedFirstNoteKeepsTheReasonWithinItsLimit(t *testing.T) {
	app := manifest.App{Kind: "image", Deploy: manifest.Deploy{Order: "stop-first"}, Roles: map[string]manifest.Role{"web": {}}}
	long := strings.Repeat("x", maxReasonBytes)
	got := addStoppedFirst(long, app, []roleWork{{intent: state.Intent{Role: "web", Kind: "update"}}})
	if len(got) > maxReasonBytes || !strings.HasSuffix(got, "so it was down until the previous version came back") {
		t.Fatalf("len %d: %q", len(got), got)
	}
}
