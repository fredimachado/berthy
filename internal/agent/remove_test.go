package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/jobs"
	"berthy/internal/state"
)

// forgetApp removes an app's applied release, as berthy remove does.
func forgetApp(t *testing.T, s *state.Store, app string) {
	t.Helper()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "forget-" + app, ActorID: "admin", Kind: "remove", App: app, PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ForgetApp(ctx, op.ID, app); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
}

// removedAppManifest has two image apps with a job each, and a service probe against each.
const removedAppManifest = `{"project":"demo","network":"demo",
	"apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}},"worker":{"kind":"image","image":"ghcr.io/acme/worker","roles":{"job":{}}}},
	"jobs":{"tick":{"app":"api","role":"web","schedule":"* * * * *","command":["date"]},"sweep":{"app":"worker","role":"job","schedule":"* * * * *","command":["date"]}},
	"probes":{"api-up":{"service":{"app":"api","role":"web","port":8080}},"worker-up":{"service":{"app":"worker","role":"job","port":9000}},"site":{"http":"http://127.0.0.1:1/"}}}`

func checkNames(ctx context.Context, w *Monitoring) map[string]bool {
	out := map[string]bool{}
	for _, c := range w.checks(ctx) {
		out[c.Name()] = true
	}
	return out
}

func TestRemovedAppsAreNotMonitoredOrScheduledEvenWhileTheManifestNamesThem(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	recordRelease(t, s, "release", removedAppManifest)
	d := &deploy.Controller{Store: s, Environment: "lab"}
	w := NewMonitoring(monitoringConfig(""), nil, nil, d, func(string) {})
	before := checkNames(ctx, w)
	for _, name := range []string{"job:tick", "job:sweep", "probe:api-up", "probe:worker-up", "probe:site"} {
		if !before[name] {
			t.Fatalf("%s is missing before the removal: %v", name, before)
		}
	}
	sched := NewJobs(monitoringConfig(""), engine.New(filepath.Join(t.TempDir(), "none.sock")), d, func(string) {})
	if m, err := sched.Manifest(ctx); err != nil || len(m.Jobs) != 2 {
		t.Fatalf("scheduler manifest before: %+v %v", m.Jobs, err)
	}

	forgetApp(t, s, "api")

	after := checkNames(ctx, w)
	for _, name := range []string{"job:tick", "probe:api-up"} {
		if after[name] {
			t.Errorf("%s still runs for a removed app", name)
		}
	}
	for _, name := range []string{"job:sweep", "probe:worker-up", "probe:site"} {
		if !after[name] {
			t.Errorf("%s of a deployed app or of no app disappeared", name)
		}
	}
	m, err := sched.Manifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Jobs["tick"]; ok || len(m.Jobs) != 1 || m.Jobs["sweep"].App != "worker" {
		t.Fatalf("the scheduler would still run a removed app's job: %+v", m.Jobs)
	}
	if got := appJobs(ctx, d, api.App{Ref: api.AppRef{Kind: "image", Name: "api"}}, time.Now()); got != nil {
		t.Errorf("describe lists the jobs of a removed app: %+v", got)
	}
	if got := appJobs(ctx, d, api.App{Ref: api.AppRef{Kind: "image", Name: "worker"}}, time.Now()); len(got) != 1 {
		t.Errorf("describe lost the jobs of a deployed app: %+v", got)
	}
	if _, err = sched.Start(ctx, "tick", "manual"); err != jobs.ErrUnknownJob {
		t.Errorf("a removed app's job can still be started: %v", err)
	}
}

func TestJobsRoutesSkipRemovedApps(t *testing.T) {
	s := openIngressStore(t)
	recordRelease(t, s, "release", removedAppManifest)
	forgetApp(t, s, "api")
	h := jobsHandler(t, s, &blockingExec{})
	w := serveRequest(t, h, http.MethodGet, "/v1/projects/demo/environments/lab/jobs", monitoringToken("a"))
	var list []api.JobStatus
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list) != 1 || list[0].Name != "sweep" {
		t.Fatalf("jobs: %d %s", w.Code, w.Body.String())
	}
	if w = serveRequest(t, h, http.MethodGet, "/v1/projects/demo/environments/lab/jobs/tick/runs", monitoringToken("a")); w.Code != http.StatusNotFound {
		t.Fatalf("runs of a removed app's job: %d %s", w.Code, w.Body.String())
	}
}

func TestDoctorListsDeployedAppsTheManifestNoLongerNames(t *testing.T) {
	s := openIngressStore(t)
	recordRelease(t, s, "both", removedAppManifest)
	h := NewDeploymentHandler(monitoringConfig(""), nil, nil, &deploy.Controller{Store: s, Environment: "lab"})
	if _, ok := findCheck(doctorChecks(t, h), "orphan_apps"); ok {
		t.Fatal("a manifest that names every deployed app has orphans")
	}
	// api is deployed again from a manifest that no longer names worker, which keeps running.
	recordAppRelease(t, s, "api-only", "api", monitoredManifest(``))
	check, ok := findCheck(doctorChecks(t, h), "orphan_apps")
	if !ok || check.OK || check.Critical || check.Detail != "deployed but not in the manifest: worker" {
		t.Fatalf("orphan_apps: %+v %v", check, ok)
	}
	forgetApp(t, s, "worker")
	if _, ok = findCheck(doctorChecks(t, h), "orphan_apps"); ok {
		t.Fatal("an app that was removed is not an orphan")
	}
}

func TestRemoveRouteNeedsAnAdministratorAndReachesTheController(t *testing.T) {
	s := openIngressStore(t)
	readToken := "bty.demo.lab." + strings.Repeat("a", 64)
	deployToken := "bty.demo.lab." + strings.Repeat("b", 64)
	adminToken := "bty.demo.lab." + strings.Repeat("c", 64)
	cfg := Config{Project: "demo", Environment: "lab", Tokens: []Token{{Digest: Digest(readToken), Scope: "read"}, {Digest: Digest(deployToken), Scope: "deploy"}, {Digest: Digest(adminToken), Scope: "admin"}}}
	h := NewDeploymentHandler(cfg, nil, nil, &deploy.Controller{Store: s, Environment: "lab"})
	call := func(method, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/v1/projects/demo/environments/lab/remove", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	body := `{"app":"api","request_id":"r1"}`
	if w := call("POST", readToken, body); w.Code != http.StatusForbidden {
		t.Fatalf("read scope: %d", w.Code)
	}
	if w := call("POST", deployToken, body); w.Code != http.StatusForbidden {
		t.Fatalf("deploy scope: %d", w.Code)
	}
	if w := call("POST", "bty.demo.lab."+strings.Repeat("d", 64), body); w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown token: %d", w.Code)
	}
	if w := call("GET", adminToken, ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", w.Code)
	}
	if w := call("POST", adminToken, body); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "app is not deployed") {
		t.Fatalf("admin on an unknown app: %d %s", w.Code, w.Body.String())
	}
	if w := call("POST", adminToken, `{"app":"ingress","request_id":"r2","dry_run":true}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "use ingress apply without an ingress block") {
		t.Fatalf("the connector: %d %s", w.Code, w.Body.String())
	}
}

func TestDestroyRouteNeedsAnAdministratorAndTheEnvironmentName(t *testing.T) {
	s := openIngressStore(t)
	readToken := "bty.demo.lab." + strings.Repeat("a", 64)
	deployToken := "bty.demo.lab." + strings.Repeat("b", 64)
	adminToken := "bty.demo.lab." + strings.Repeat("c", 64)
	cfg := Config{Project: "demo", Environment: "lab", Tokens: []Token{{Digest: Digest(readToken), Scope: "read"}, {Digest: Digest(deployToken), Scope: "deploy"}, {Digest: Digest(adminToken), Scope: "admin"}}}
	h := NewDeploymentHandler(cfg, nil, nil, &deploy.Controller{Store: s, Project: "demo", Environment: "lab"})
	call := func(method, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/v1/projects/demo/environments/lab/destroy", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	body := `{"request_id":"r1","confirm":"demo/lab"}`
	if w := call("POST", readToken, body); w.Code != http.StatusForbidden {
		t.Fatalf("read scope: %d", w.Code)
	}
	if w := call("POST", deployToken, body); w.Code != http.StatusForbidden {
		t.Fatalf("deploy scope: %d", w.Code)
	}
	if w := call("GET", adminToken, ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", w.Code)
	}
	for _, b := range []string{`{"request_id":"r2"}`, `{"request_id":"r3","confirm":"prod"}`, `{"request_id":"r4","confirm":"lab"}`, `{"request_id":"r5","confirm":"demo/lab","dry_run":true,"tunnel":true,"volumes":true}`} {
		w := call("POST", adminToken, b)
		wantBad := !strings.Contains(b, `"confirm":"demo/lab"`)
		if wantBad && (w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "confirmation must name the project and the environment, as project/environment")) {
			t.Fatalf("%s: %d %s", b, w.Code, w.Body.String())
		}
		if !wantBad && w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", b, w.Code, w.Body.String())
		}
	}
}
