package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/metrics"
	"berthy/internal/monitor"
	"berthy/internal/scope"
	"berthy/internal/state"
	"berthy/internal/telemetry"
)

// hostToken is the admin token of the hosted environment id. The digit makes it differ between environments of one file,
// which refuses a token digest it has twice.
func hostToken(id scope.ID, digit string) string {
	return "bty." + id.Project + "." + id.Environment + "." + strings.Repeat(digit, 64)
}

// hostedFile is an agent.json of the given environments, each with its own temporary state and key and one admin token, and
// an engine on a socket that does not exist. adminOf gives the token of the n-th.
func hostedFile(t *testing.T, ids ...scope.ID) (Shared, File) {
	t.Helper()
	root := t.TempDir()
	f := File{Listen: "127.0.0.1:19500"}
	for n, id := range ids {
		dir := filepath.Join(root, id.Project+"-"+id.Environment)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		key := filepath.Join(root, "key-"+id.Project+"-"+id.Environment)
		if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
			t.Fatal(err)
		}
		f.Environments = append(f.Environments, Hosted{Project: id.Project, Environment: id.Environment, StateDir: dir, KeyFile: key,
			Tokens: []Token{{Name: "admin", Digest: Digest(hostToken(id, string(rune('1'+n)))), Scope: "admin"}}})
	}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	return Shared{Engine: engine.New(filepath.Join(root, "missing.sock"))}, f
}

// adminOf is the admin token hostedFile made for the n-th environment of the file.
func adminOf(f File, n int) string {
	h := f.Environments[n]
	return hostToken(scope.ID{Project: h.Project, Environment: h.Environment}, string(rune('1'+n)))
}

// openHost opens and starts a Host of f and closes it at the end of the test. The test serves it only after Start, as the
// agent command does.
func openHost(t *testing.T, sh Shared, f File) *Host {
	t.Helper()
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	if err = h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return h
}

func serve(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func environmentOfAgentInfo(t *testing.T, w *httptest.ResponseRecorder) scope.ID {
	t.Helper()
	var info struct{ Project, Environment string }
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &info) != nil {
		t.Fatalf("agent: %d %s", w.Code, w.Body.String())
	}
	return scope.ID{Project: info.Project, Environment: info.Environment}
}

func TestEventsURLIsUnderTheProjectPath(t *testing.T) {
	f := newApprovalFixture(t, false, func(c *Config) { c.Project = "demo" })
	// api was deployed and then removed, so removing it again is a plan that needs no Docker.
	recordAppRelease(t, f.s, "api", "api", apManifestSource)
	forgetApp(t, f.s, "api")
	w := f.call("POST", "/remove", apAdminToken, `{"app":"api","request_id":"rm-1"}`)
	var accepted struct {
		OperationID string `json:"operation_id"`
		EventsURL   string `json:"events_url"`
	}
	if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &accepted) != nil {
		t.Fatalf("remove: %d %s", w.Code, w.Body.String())
	}
	want := "/v1/projects/demo/environments/lab/operations/" + accepted.OperationID + "/events"
	if accepted.EventsURL != want || w.Header().Get("Location") != want {
		t.Fatalf("events_url %q, Location %q, want %q", accepted.EventsURL, w.Header().Get("Location"), want)
	}
	// The URL the agent hands out is one it serves.
	events := f.call("GET", "/operations/"+accepted.OperationID+"/events", apReadToken, "")
	if events.Code != http.StatusOK || !strings.Contains(events.Body.String(), "event: terminal") {
		t.Fatalf("events: %d %s", events.Code, events.Body.String())
	}
}

func TestAgentInfoNamesTheProject(t *testing.T) {
	f := newApprovalFixture(t, false, func(c *Config) { c.Project = "demo" })
	w := f.call("GET", "/agent", apReadToken, "")
	var info struct {
		Project     string `json:"project"`
		Environment string `json:"environment"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &info) != nil || info.Project != "demo" || info.Environment != "lab" {
		t.Fatalf("agent: %d %s", w.Code, w.Body.String())
	}
}

func TestHostSendsAProjectURLToItsEnvironment(t *testing.T) {
	lab, prod := scope.ID{Project: "demo", Environment: "lab"}, scope.ID{Project: "shop", Environment: "prod"}
	sh, f := hostedFile(t, lab, prod)
	h := openHost(t, sh, f)
	for n, id := range []scope.ID{lab, prod} {
		got := environmentOfAgentInfo(t, serve(h, "GET", id.APIPath()+"agent", adminOf(f, n), ""))
		if got != id {
			t.Errorf("%s answered as %s", id, got)
		}
	}
	if w := serve(h, "GET", "/healthz", "", ""); w.Code != http.StatusOK || w.Body.String() != `{"ok":true}` {
		t.Errorf("healthz: %d %s", w.Code, w.Body.String())
	}
}

func TestTwoEnvironmentsOfOneProjectAreServedApart(t *testing.T) {
	lab, dev := scope.ID{Project: "demo", Environment: "lab"}, scope.ID{Project: "demo", Environment: "dev"}
	sh, f := hostedFile(t, lab, dev)
	h := openHost(t, sh, f)
	if got := environmentOfAgentInfo(t, serve(h, "GET", dev.APIPath()+"agent", adminOf(f, 1), "")); got != dev {
		t.Fatalf("dev answered as %s", got)
	}
	// What one environment records is its own: only its lock is held.
	if w := serve(h, "POST", lab.APIPath()+"lock/acquire", adminOf(f, 0), `{"message":"maintenance"}`); w.Code != http.StatusOK {
		t.Fatalf("lock: %d %s", w.Code, w.Body.String())
	}
	for n, want := range []bool{true, false} {
		id := []scope.ID{lab, dev}[n]
		w := serve(h, "GET", id.APIPath()+"lock", adminOf(f, n), "")
		var lock struct{ Locked bool }
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &lock) != nil || lock.Locked != want {
			t.Errorf("%s lock: %d %s, want locked=%v", id, w.Code, w.Body.String(), want)
		}
	}
}

func TestATokenOfOneEnvironmentIs401OnAnother(t *testing.T) {
	// Two environments are called lab, in two projects, and a third is in the first project.
	demoLab, shopLab, demoDev := scope.ID{Project: "demo", Environment: "lab"}, scope.ID{Project: "shop", Environment: "lab"}, scope.ID{Project: "demo", Environment: "dev"}
	sh, f := hostedFile(t, demoLab, shopLab, demoDev)
	h := openHost(t, sh, f)
	ids := []scope.ID{demoLab, shopLab, demoDev}
	for n, id := range ids {
		for m, other := range ids {
			w := serve(h, "GET", other.APIPath()+"agent", adminOf(f, n), "")
			if want := map[bool]int{true: http.StatusOK, false: http.StatusUnauthorized}[n == m]; w.Code != want {
				t.Errorf("the token of %s on %s: %d, want %d", id, other, w.Code, want)
			}
		}
	}
}

func TestAnUnknownProjectIs401LikeABadToken(t *testing.T) {
	lab := scope.ID{Project: "demo", Environment: "lab"}
	sh, f := hostedFile(t, lab)
	h := openHost(t, sh, f)
	bad := serve(h, "GET", lab.APIPath()+"agent", hostToken(lab, "9"), "")
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("a bad token: %d", bad.Code)
	}
	for _, path := range []string{"/v1/projects/nope/environments/lab/agent", "/v1/projects/demo/environments/nope/agent", "/v1/projects/nope/environments/nope/apps/web/logs"} {
		w := serve(h, "GET", path, adminOf(f, 0), "")
		if w.Code != http.StatusUnauthorized || w.Body.String() != bad.Body.String() {
			t.Errorf("%s: %d %s, want the answer of a bad token %s", path, w.Code, w.Body.String(), bad.Body.String())
		}
	}
}

// A client without a valid token learns nothing about which environments the agent hosts, whatever path it asks for: a hosted
// environment and one that does not exist answer alike.
func TestAHostedEnvironmentAnswersWithoutATokenAsAnUnhostedOneDoes(t *testing.T) {
	lab := scope.ID{Project: "demo", Environment: "lab"}
	sh, f := hostedFile(t, lab)
	h := openHost(t, sh, f)
	unhosted := scope.ID{Project: "demo", Environment: "nope"}
	for _, tc := range []struct{ method, rest string }{
		{"GET", "agent"}, {"GET", "zzz"}, {"GET", "apps/web/logs/extra"}, {"POST", "zzz"}, {"POST", "agent"}, {"DELETE", "deploys/zzz"}, {"PUT", ""},
	} {
		for name, token := range map[string]string{"no token": "", "a bad token": hostToken(lab, "9")} {
			want := serve(h, tc.method, unhosted.APIPath()+tc.rest, token, "")
			got := serve(h, tc.method, lab.APIPath()+tc.rest, token, "")
			if got.Code != http.StatusUnauthorized || got.Code != want.Code || got.Body.String() != want.Body.String() {
				t.Errorf("%s %s with %s: hosted %d %s, unhosted %d %s", tc.method, tc.rest, name, got.Code, got.Body.String(), want.Code, want.Body.String())
			}
		}
	}
	// Once the token is valid the environment may say that the path is not one of its routes.
	if w := serve(h, "GET", lab.APIPath()+"zzz", adminOf(f, 0), ""); w.Code != http.StatusNotFound {
		t.Errorf("a valid token on an unknown path: %d %s", w.Code, w.Body.String())
	}
}

func TestTheURLOfAnEarlierReleaseSaysToUpgradeTheCLI(t *testing.T) {
	lab := scope.ID{Project: "demo", Environment: "lab"}
	sh, f := hostedFile(t, lab)
	h := openHost(t, sh, f)
	for _, path := range []string{"/v1/environments/lab/agent", "/v1/environments/lab/", "/v1/environments/other/apps"} {
		w := serve(h, "GET", path, adminOf(f, 0), "")
		var body struct{ Code, Message string }
		const want = "this agent serves /v1/projects/<project>/environments/<environment>/: upgrade the CLI"
		if w.Code != http.StatusNotFound || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Code != "not_found" || body.Message != want {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/", "/v1", "/v1/projects/demo", "/apps"} {
		if w := serve(h, "GET", path, adminOf(f, 0), ""); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "upgrade") {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
}

// openable fails the test unless the state of h can be opened, which it cannot while another store holds it.
func openable(t *testing.T, h Hosted) {
	t.Helper()
	s, err := state.Open(h.StateDir, h.KeyFile, h.Environment)
	if err != nil {
		t.Fatalf("state of %s/%s: %v", h.Project, h.Environment, err)
	}
	s.Close()
}

func wrongHostedKey(t *testing.T, entry *Hosted) {
	t.Helper()
	openable(t, *entry)
	wrong := entry.KeyFile + ".wrong"
	if err := os.WriteFile(wrong, []byte(strings.Repeat("x", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	entry.KeyFile = wrong
}

func expectEnvironmentFailed(t *testing.T, w *httptest.ResponseRecorder, id scope.ID, reason string) {
	t.Helper()
	var body struct{ Code, Message string }
	if w.Code != http.StatusServiceUnavailable || json.Unmarshal(w.Body.Bytes(), &body) != nil ||
		body.Code != "environment_failed" || !strings.Contains(body.Message, id.String()) || !strings.Contains(body.Message, reason) {
		t.Errorf("%s: got %d %s, want environment_failed with %q", id, w.Code, w.Body.String(), reason)
	}
}

func TestHostServesHealthyPeersAfterStateOpenFails(t *testing.T) {
	ids := []scope.ID{{Project: "demo", Environment: "lab"}, {Project: "demo", Environment: "dev"}, {Project: "shop", Environment: "dev"}}
	for bad, id := range ids {
		t.Run(id.Project+"-"+id.Environment, func(t *testing.T) {
			sh, f := hostedFile(t, ids...)
			original := f.Environments[bad]
			wrongHostedKey(t, &f.Environments[bad])
			h := openHost(t, sh, f)
			for i, id := range ids {
				w := serve(h, "GET", id.APIPath()+"agent", adminOf(f, i), "")
				if i == bad {
					expectEnvironmentFailed(t, w, id, state.ErrWrongKey.Error())
				} else if got := environmentOfAgentInfo(t, w); got != id {
					t.Errorf("agent-info = %v, want %v", got, id)
				}
			}
			h.Close()
			f.Environments[bad] = original
			for _, entry := range f.Environments {
				openable(t, entry)
			}
		})
	}
}

func TestAllFailedEnvironmentsStillServeHealth(t *testing.T) {
	a, b := scope.ID{Project: "one", Environment: "dev"}, scope.ID{Project: "two", Environment: "dev"}
	sh, f := hostedFile(t, a, b)
	wrongHostedKey(t, &f.Environments[0])
	if err := os.WriteFile(filepath.Join(f.Environments[1].StateDir, "state.db"), []byte("invalid database"), 0600); err != nil {
		t.Fatal(err)
	}
	h := openHost(t, sh, f)
	if w := serve(h, "GET", "/healthz", "", ""); w.Code != http.StatusOK || w.Body.String() != `{"ok":true}` {
		t.Errorf("healthz: %d %s", w.Code, w.Body.String())
	}
	expectEnvironmentFailed(t, serve(h, "GET", a.APIPath()+"agent", adminOf(f, 0), ""), a, state.ErrWrongKey.Error())
	expectEnvironmentFailed(t, serve(h, "GET", b.APIPath()+"agent", adminOf(f, 1), ""), b, "cannot open state")
}

func TestKnownNetworkConflictRejectsAdditionWithUnreadablePeer(t *testing.T) {
	owner, added, unreadable := scope.ID{Project: "owner", Environment: "dev"}, scope.ID{Project: "added", Environment: "dev"}, scope.ID{Project: "peer", Environment: "dev"}
	sh, f := hostedFile(t, owner, added, unreadable)
	f.Environments[0].Network = "shared-network"
	initial, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	initial.Close()
	// The configured names are distinct. The owner's store still holds its earlier network.
	f.Environments[0].Network = "owner-current"
	f.Environments[1].Network = "shared-network"
	wrongHostedKey(t, &f.Environments[2])
	h := openHost(t, sh, f)
	expectEnvironmentFailed(t, serve(h, "GET", added.APIPath()+"agent", adminOf(f, 1), ""), added, "cannot reserve configured network")
	if got := environmentOfAgentInfo(t, serve(h, "GET", owner.APIPath()+"agent", adminOf(f, 0), "")); got != owner {
		t.Errorf("agent-info = %s, want %s", got, owner)
	}
}

func TestStartupLogsUnderlyingCauseWithOnlyPublicReasonOverHTTP(t *testing.T) {
	id := scope.ID{Project: "one", Environment: "dev"}
	sh, f := hostedFile(t, id)
	f.Environments[0].KeyFile += ".missing"
	unexpected, cause := OpenRuntime(sh, f.Configs()[0])
	if cause == nil {
		unexpected.Close()
		t.Fatal("fixture unexpectedly opened")
	}
	var log []string
	sh.Log = func(s string) { log = append(log, s) }
	h := openHost(t, sh, f)
	h.Close()
	w := serve(h, "GET", id.APIPath()+"agent", adminOf(f, 0), "")
	expectEnvironmentFailed(t, w, id, "cannot open state")
	if strings.Contains(w.Body.String(), f.Environments[0].KeyFile) {
		t.Errorf("underlying cause in HTTP response: %s", w.Body)
	}
	if got := strings.Join(log, "\n"); !strings.Contains(got, id.String()+":") || !strings.Contains(got, cause.Error()) {
		t.Errorf("journal = %q, want pair and underlying open error", got)
	}
}

func TestDoctorOfEveryRunningEnvironmentReportsFailedPeers(t *testing.T) {
	a, b, c := scope.ID{Project: "one", Environment: "dev"}, scope.ID{Project: "peer", Environment: "dev"}, scope.ID{Project: "two", Environment: "dev"}
	sh, f := hostedFile(t, a, b, c)
	original := f.Environments[1]
	wrongHostedKey(t, &f.Environments[1])
	h := openHost(t, sh, f)
	hostedRows := func(h *Host, id scope.ID, token string) []api.Check {
		t.Helper()
		w := serve(h, "GET", id.APIPath()+"doctor", token, "")
		var body struct{ Checks []api.Check }
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil ||
			len(body.Checks) == 0 || body.Checks[0].Name != "agent" {
			t.Fatalf("%s doctor: %d %s", id, w.Code, w.Body.String())
		}
		return slices.DeleteFunc(body.Checks, func(c api.Check) bool { return !strings.HasPrefix(c.Name, "hosted:") })
	}
	want := []api.Check{{Name: "hosted:peer/dev", Detail: state.ErrWrongKey.Error()}}
	for _, i := range []int{0, 2} {
		id := f.Configs()[i].ID()
		if got := hostedRows(h, id, adminOf(f, i)); !slices.Equal(got, want) {
			t.Errorf("%s doctor hosted rows: got %+v, want %+v", id, got, want)
		}
	}
	expectEnvironmentFailed(t, serve(h, "GET", b.APIPath()+"doctor", adminOf(f, 1), ""), b, state.ErrWrongKey.Error())
	h.Close()
	f.Environments[1] = original
	h = openHost(t, sh, f)
	if got := hostedRows(h, a, adminOf(f, 0)); len(got) != 0 {
		t.Errorf("healthy-only doctor adds hosted rows: %+v", got)
	}
}

type hostedAlertRecorder struct {
	mu     sync.Mutex
	alerts []monitor.Alert
}

func (r *hostedAlertRecorder) Notify(_ context.Context, a monitor.Alert) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alerts = append(r.alerts, a)
	return nil
}

func TestHostedStartupAlertAndRecoveryUseTheSameCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := scope.ID{Project: "one", Environment: "dev"}, scope.ID{Project: "peer", Environment: "dev"}
		sh, f := hostedFile(t, a, b)
		ctx := context.Background()
		const checkName = "hosted:peer/dev"
		start := func() []monitor.Alert {
			t.Helper()
			h, err := Open(sh, f)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			recorder := &hostedAlertRecorder{}
			w := h.runtimes[a].monitoring
			w.mon.Checks = func(ctx context.Context) []monitor.Check {
				checks := w.builtins(ctx, false, nil)
				return slices.DeleteFunc(checks, func(c monitor.Check) bool { return !strings.HasPrefix(c.Name(), "hosted:") })
			}
			w.mon.Notifier = func(context.Context) (monitor.Notifier, time.Duration, error) { return recorder, 0, nil }
			if err = h.Start(ctx); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			h.Close()
			return recorder.alerts
		}

		if alerts := start(); len(alerts) != 0 {
			t.Errorf("initially healthy host sent alerts: %+v", alerts)
		}
		original := f.Environments[1]
		wrongHostedKey(t, &f.Environments[1])
		if alerts := start(); len(alerts) != 1 || alerts[0].Kind != "check" || alerts[0].Name != checkName ||
			alerts[0].State != monitor.Critical || alerts[0].Detail != state.ErrWrongKey.Error() {
			t.Errorf("critical startup notification for %s: got %+v", checkName, alerts)
		}
		f.Environments[1] = original
		if alerts := start(); len(alerts) != 1 || alerts[0].Kind != "check" || alerts[0].Name != checkName || alerts[0].State != monitor.OK {
			t.Errorf("recovery notification for %s: got %+v", checkName, alerts)
		}
	})
}

func TestFailedEnvironmentRequiresItsOwnCredential(t *testing.T) {
	a, b := scope.ID{Project: "one", Environment: "dev"}, scope.ID{Project: "two", Environment: "dev"}
	unknown := scope.ID{Project: "unknown", Environment: "dev"}
	sh, f := hostedFile(t, a, b)
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	failed := h.runtimes[a]
	failed.deployment.Engine = nil
	apiToken, digest, err := NewToken(a)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = failed.deployment.Store.CreateToken(ctx, state.Actor{ID: "admin"}, state.Token{
		Name: "api-reader", Digest: digest, Scope: "read", Created: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err = h.Start(ctx); err != nil {
		t.Fatalf("recovery failure stopped the host: %v", err)
	}
	expectEnvironmentFailed(t, serve(h, "GET", a.APIPath()+"agent", apiToken, ""), a, "deployment recovery failed")
	db, err := sql.Open("sqlite", filepath.Join(f.Environments[0].StateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.ExecContext(ctx, "DROP TABLE tokens"); err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ method, path string }{{"GET", "agent"}, {"POST", "deploy"}} {
		for _, token := range []string{"", "malformed", adminOf(f, 1), hostToken(a, "a"), apiToken} {
			got := serve(h, route.method, a.APIPath()+route.path, token, "{}")
			want := serve(h, route.method, unknown.APIPath()+route.path, token, "{}")
			if got.Code != http.StatusUnauthorized || got.Code != want.Code || got.Body.String() != want.Body.String() {
				t.Errorf("%s: got %d %s, want %d %s", route.method, got.Code, got.Body, want.Code, want.Body)
			}
		}
		expectEnvironmentFailed(t, serve(h, route.method, a.APIPath()+route.path, adminOf(f, 0), "{}"), a, "deployment recovery failed")
	}
}

func TestOpenRefusesAStateOfAnotherProject(t *testing.T) {
	lab, dev := scope.ID{Project: "demo", Environment: "lab"}, scope.ID{Project: "demo", Environment: "dev"}
	sh, f := hostedFile(t, lab, dev)
	first, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	// The state of dev is given to another project, and to another environment of the same project.
	for name, change := range map[string]func(*Hosted){
		"another project":     func(h *Hosted) { h.Project = "shop" },
		"another environment": func(h *Hosted) { h.Environment = "prod" },
	} {
		g := f
		g.Environments = append([]Hosted(nil), f.Environments...)
		change(&g.Environments[1])
		t.Run(name, func(t *testing.T) {
			h := openHost(t, sh, g)
			bad := scope.ID{Project: g.Environments[1].Project, Environment: g.Environments[1].Environment}
			if got := h.runtimes[bad]; got != nil {
				t.Fatal("misassigned state supplied a runtime")
			}
			if _, err := h.foreignClaims(context.Background(), lab); err == nil {
				t.Fatal("misassigned state supplied ownership evidence")
			}
			if got := environmentOfAgentInfo(t, serve(h, "GET", lab.APIPath()+"agent", adminOf(f, 0), "")); got != lab {
				t.Errorf("agent-info = %v, want %v", got, lab)
			}
		})
	}
}

type hostCheck struct{}

func (hostCheck) Name() string            { return "host-test" }
func (hostCheck) Interval() time.Duration { return time.Hour }
func (hostCheck) Failures() int           { return 1 }
func (hostCheck) Successes() int          { return 1 }
func (hostCheck) Run(context.Context) monitor.Result {
	return monitor.Result{State: monitor.Warning}
}

func TestHostStartsLaterWorkersAfterRecoveryFailure(t *testing.T) {
	a, b := scope.ID{Project: "one", Environment: "dev"}, scope.ID{Project: "two", Environment: "dev"}
	sh, f := hostedFile(t, a, b)
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	failed, later := h.runtimes[a], h.runtimes[b]
	failed.deployment.Engine = nil
	recorded := make(chan struct{})
	var once sync.Once
	later.monitoring.mon.Checks = func(context.Context) []monitor.Check { return []monitor.Check{hostCheck{}} }
	later.monitoring.mon.Notifier = func(context.Context) (monitor.Notifier, time.Duration, error) {
		once.Do(func() { close(recorded) })
		return nil, 0, nil
	}
	ctx := context.Background()
	if err = h.Start(ctx); err != nil {
		t.Fatalf("recovery failure stopped the host: %v", err)
	}
	if later.monitorDone == nil {
		t.Fatal("later monitor did not start")
	}
	<-recorded
	if rows, err := later.deployment.Store.Checks(ctx); err != nil || len(rows) != 1 || rows[0].Name != "host-test" {
		t.Errorf("later monitor checks: %+v, %v", rows, err)
	}
	select {
	case <-failed.reportsDone:
	default:
		t.Error("failed environment's reports were not joined")
	}
	if failed.recoveryDone != nil || failed.monitorDone != nil || failed.jobsDone != nil || failed.exportDone != nil {
		t.Error("failed environment started ordinary workers")
	}
	if _, err = h.foreignClaims(ctx, b); err != nil {
		t.Errorf("owner-checked state lost after recovery failure: %v", err)
	}
}

func TestOpenStartsNoWorkerOfAnyEnvironment(t *testing.T) {
	lab, dev := scope.ID{Project: "demo", Environment: "lab"}, scope.ID{Project: "demo", Environment: "dev"}
	sh, f := hostedFile(t, lab, dev)
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	// The notifier runs only after the monitor has recorded the check's warning.
	var recorded sync.WaitGroup
	for _, r := range h.order {
		recorded.Add(1)
		var once sync.Once
		r.monitoring.mon.Checks = func(context.Context) []monitor.Check { return []monitor.Check{hostCheck{}} }
		r.monitoring.mon.Notifier = func(context.Context) (monitor.Notifier, time.Duration, error) {
			once.Do(recorded.Done)
			return nil, 0, nil
		}
		if rows, err := r.deployment.Store.Checks(context.Background()); err != nil || len(rows) != 0 {
			t.Fatalf("%s: checks after Open: %+v %v", r.Config.ID(), rows, err)
		}
	}
	if err = h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	recorded.Wait()
	for _, r := range h.order {
		rows, err := r.deployment.Store.Checks(context.Background())
		if err != nil || len(rows) != 1 || rows[0].Name != "host-test" {
			t.Errorf("%s: checks after Start: %+v %v", r.Config.ID(), rows, err)
		}
	}
}

func TestCloseReleasesEveryState(t *testing.T) {
	lab, dev := scope.ID{Project: "demo", Environment: "lab"}, scope.ID{Project: "shop", Environment: "dev"}
	sh, f := hostedFile(t, lab, dev)
	h := openHost(t, sh, f)
	h.CloseSessions(context.Background())
	h.Close()
	for _, hosted := range f.Environments {
		openable(t, hosted)
	}
}

// hold opens n streams of url and keeps them open until the test ends. Each one is answered before the next starts, so it
// holds its slot when hold returns.
func hold(t *testing.T, url, token string, n int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for i := 0; i < n; i++ {
		resp := get(t, ctx, url, token)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d of %s: %d", i+1, url, resp.StatusCode)
		}
		t.Cleanup(func() { resp.Body.Close() })
	}
}

func get(t *testing.T, ctx context.Context, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAFullFollowerLimitOfOneEnvironmentLeavesTheOthersServing(t *testing.T) {
	lab, dev := scope.ID{Project: "demo", Environment: "lab"}, scope.ID{Project: "shop", Environment: "dev"}
	sh, f := hostedFile(t, lab, dev)
	// The stats stream sends its first sample at once, so it answers before the test holds it.
	sh.Collector = metrics.New(metrics.NewHostReader(), nilDocker{})
	sampled, stop := sh.Collector.Subscribe()
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sh.Collector.Run(ctx)
	<-sampled
	h := openHost(t, sh, f)
	// A running operation of each environment, with an event, so that its stream answers at once.
	ops := map[scope.ID]string{}
	for _, id := range []scope.ID{lab, dev} {
		s := h.runtimes[id].deployment.Store
		op, _, err := s.Admit(ctx, state.Admission{RequestID: "r", ActorID: "a", Kind: "deploy", PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.AppendEvent(ctx, op.ID, state.Event{Kind: "progress", Data: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
		ops[id] = op.ID
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	for name, path := range map[string]func(scope.ID) string{
		"stats followers":    func(id scope.ID) string { return id.APIPath() + "stats?watch=1" },
		"operation watchers": func(id scope.ID) string { return id.APIPath() + "operations/" + ops[id] + "/events" },
	} {
		t.Run(name, func(t *testing.T) {
			hold(t, srv.URL+path(lab), adminOf(f, 0), maxStreams)
			full := get(t, ctx, srv.URL+path(lab), adminOf(f, 0))
			defer full.Body.Close()
			body, _ := io.ReadAll(full.Body)
			if full.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "too_many_followers") {
				t.Fatalf("the stream past the limit of %s: %d %s", lab, full.StatusCode, body)
			}
			other := get(t, ctx, srv.URL+path(dev), adminOf(f, 1))
			defer other.Body.Close()
			if other.StatusCode != http.StatusOK {
				t.Fatalf("the first stream of %s while %s is full: %d", dev, lab, other.StatusCode)
			}
		})
	}
}

func TestTwoOverlappingReservationsOfOneNameHaveOneWinner(t *testing.T) {
	a, b := scope.ID{Project: "one", Environment: "prod"}, scope.ID{Project: "two", Environment: "prod"}
	sh, f := hostedFile(t, a, b)
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	claim := state.Claim{Kind: state.ClaimVolume, Name: "shared"}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, id := range []scope.ID{a, b} {
		go func() { <-start; _, err := h.reserve(id, []state.Claim{claim}); results <- err }()
	}
	close(start)
	successes := 0
	for range 2 {
		if <-results == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful reservations: %d", successes)
	}
	holders := 0
	for _, id := range []scope.ID{a, b} {
		claims, err := h.runtimes[id].deployment.Store.Claims(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range claims {
			if c == claim {
				holders++
			}
		}
	}
	if holders != 1 {
		t.Fatalf("holders: %d", holders)
	}
}

func TestReserveRefusesANameAnotherEnvironmentHolds(t *testing.T) {
	a, b := scope.ID{Project: "one", Environment: "prod"}, scope.ID{Project: "two", Environment: "prod"}
	sh, f := hostedFile(t, a, b)
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	claim := state.Claim{Kind: state.ClaimComposeProject, Name: "shared"}
	if _, err = h.reserve(a, []state.Claim{claim}); err != nil {
		t.Fatal(err)
	}
	_, err = h.reserve(b, []state.Claim{{Kind: state.ClaimVolume, Name: "other"}, claim})
	if err == nil || err.Error() != "name shared is taken by another project or environment on this server" {
		t.Fatalf("refusal: %v", err)
	}
	claims, err := h.runtimes[b].deployment.Store.Claims(context.Background())
	if err != nil || len(claims) != 0 {
		t.Fatalf("partial write: %v, %v", claims, err)
	}
}

func TestAnEnvironmentThatDeployedNothingHoldsItsNetwork(t *testing.T) {
	id := scope.ID{Project: "one", Environment: "prod"}
	sh, f := hostedFile(t, id)
	f.Environments[0].Network = "one-prod"
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	claims, err := h.runtimes[id].deployment.Store.Claims(context.Background())
	if err != nil || len(claims) != 1 || claims[0] != (state.Claim{Kind: state.ClaimNetwork, Name: "one-prod"}) {
		t.Fatalf("network: %v, %v", claims, err)
	}
}

func TestReserveFailsWhenAnotherStateCannotBeRead(t *testing.T) {
	a, b := scope.ID{Project: "one", Environment: "prod"}, scope.ID{Project: "two", Environment: "prod"}
	sh, f := hostedFile(t, a, b)
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.runtimes[b].deployment.Store.Close()
	if _, err = h.reserve(a, []state.Claim{{Kind: state.ClaimVolume, Name: "shared"}}); err == nil {
		t.Fatal("reservation succeeded with unreadable state")
	}
}

func TestUnreadablePeerCannotFreeDockerNames(t *testing.T) {
	a, b, peer := scope.ID{Project: "one", Environment: "dev"}, scope.ID{Project: "two", Environment: "dev"}, scope.ID{Project: "peer", Environment: "dev"}
	sh, f := hostedFile(t, a, b, peer)
	f.Environments[0].Network = "one-dev"
	first, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	f.Environments[1].Network = "two-dev"
	original := f.Environments[2]
	wrongHostedKey(t, &f.Environments[2])
	h := openHost(t, sh, f)
	ctx := context.Background()
	network := state.Claim{Kind: state.ClaimNetwork, Name: "one-dev"}
	held, err := h.runtimes[a].deployment.Store.Claims(ctx)
	if err != nil || !slices.Equal(held, []state.Claim{network}) {
		t.Errorf("existing network claims: %v, %v", held, err)
	}
	deferred, err := h.runtimes[b].deployment.Store.Claims(ctx)
	if err != nil || len(deferred) != 0 {
		t.Errorf("deferred network claims: %v, %v", deferred, err)
	}
	if _, err = h.reserve(a, []state.Claim{{Kind: state.ClaimVolume, Name: "shared"}}); err == nil {
		t.Error("reserved a new shared name without peer evidence")
	}
	if _, err = h.runtimes[a].deployment.Foreign(ctx); err == nil {
		t.Error("destructive inventory received incomplete peer evidence as complete")
	}
	keep := h.runtimes[a].keep()
	if !keep(a.Labels()) || keep(peer.Labels()) || keep(nil) {
		t.Error("stats did not restrict the sample to positively owned containers")
	}
	h.Close()
	f.Environments[2] = original
	reopened, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	claims, err := reopened.runtimes[b].deployment.Store.Claims(ctx)
	if err != nil || !slices.Equal(claims, []state.Claim{{Kind: state.ClaimNetwork, Name: "two-dev"}}) {
		t.Errorf("reconciled network claims: %v, %v", claims, err)
	}
}

func TestOpenRefusesTwoEnvironmentsThatHoldOneName(t *testing.T) {
	a, b := scope.ID{Project: "one", Environment: "prod"}, scope.ID{Project: "two", Environment: "prod"}
	sh, f := hostedFile(t, a, b)
	var workerLogs atomic.Int32
	sh.Log = func(string) { workerLogs.Add(1) }
	claim := state.Claim{Kind: state.ClaimComposeProject, Name: "shared"}
	for _, config := range f.Configs() {
		s, err := state.Open(config.StateDir, config.KeyFile, config.Environment)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AddClaims(context.Background(), []state.Claim{claim}); err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
	h, err := Open(sh, f)
	if h != nil {
		h.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "environments one/prod and two/prod both hold compose shared") {
		t.Fatalf("open: %v", err)
	}
	if workerLogs.Load() != 0 {
		t.Fatalf("worker ran before the conflicting states were rejected")
	}
	for _, config := range f.Configs() {
		s, err := state.Open(config.StateDir, config.KeyFile, config.Environment)
		if err != nil {
			t.Fatalf("state left open: %v", err)
		}
		s.Close()
	}
}

func TestReserveReturnsOnlyClaimsNewlyAdded(t *testing.T) {
	id := scope.ID{Project: "one", Environment: "prod"}
	sh, f := hostedFile(t, id)
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	old := state.Claim{Kind: state.ClaimVolume, Name: "old"}
	fresh := state.Claim{Kind: state.ClaimVolume, Name: "fresh"}
	if _, err := h.reserve(id, []state.Claim{old}); err != nil {
		t.Fatal(err)
	}
	added, err := h.reserve(id, []state.Claim{old, fresh, fresh})
	if err != nil || len(added) != 1 || added[0] != fresh {
		t.Fatalf("added: %v, %v", added, err)
	}
}

func TestLeaseKeepsClaimUntilConcurrentUserFinishes(t *testing.T) {
	ctx := context.Background()
	id := scope.ID{Project: "one", Environment: "prod"}
	other := scope.ID{Project: "two", Environment: "prod"}
	sh, f := hostedFile(t, id, other)
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	claim := state.Claim{Kind: state.ClaimVolume, Name: "shared"}
	added, err := h.reserve(id, []state.Claim{claim})
	if err != nil {
		t.Fatal(err)
	}
	joined, err := h.reserve(id, []state.Claim{claim})
	if err != nil || len(joined) != 0 {
		t.Fatalf("job reservation: %v, %v", joined, err)
	}
	if err = h.finish(ctx, id, []state.Claim{claim}, added, []state.Claim{claim}); err != nil {
		t.Fatal(err)
	}
	if _, err = h.reserve(other, []state.Claim{claim}); err == nil {
		t.Fatal("claim became available while job holds a lease")
	}
	if err = h.finish(ctx, id, []state.Claim{claim}, joined, []state.Claim{claim}); err != nil {
		t.Fatal(err)
	}
	if _, err = h.reserve(other, []state.Claim{claim}); err == nil {
		t.Fatal("lease end released the claim")
	}
}

type deployJobLeaseEngine struct {
	deploy.Engine
	jobStarted chan struct{}
	finishJob  chan struct{}
}

func (e *deployJobLeaseEngine) InspectNetwork(_ context.Context, name string) (engine.Network, error) {
	return engine.Network{ID: "net", Name: name, Driver: "overlay", Scope: "swarm", Attachable: true}, nil
}
func (e *deployJobLeaseEngine) InspectImage(_ context.Context, ref string) (engine.Image, error) {
	if strings.Contains(ref, "example/api") {
		return engine.Image{}, errors.New("image unavailable")
	}
	return engine.Image{ID: "image"}, nil
}
func (e *deployJobLeaseEngine) InspectService(context.Context, string) (engine.ServiceSnapshot, error) {
	return engine.ServiceSnapshot{}, &engine.StatusError{Code: 404}
}
func (e *deployJobLeaseEngine) VolumeExists(context.Context, string) (bool, error) { return false, nil }
func (e *deployJobLeaseEngine) CreateContainer(context.Context, string, json.RawMessage) (string, error) {
	close(e.jobStarted)
	return "job", nil
}
func (e *deployJobLeaseEngine) StartContainer(context.Context, string) error { return nil }
func (e *deployJobLeaseEngine) WaitContainer(context.Context, string) (int, error) {
	<-e.finishJob
	return 0, nil
}
func (e *deployJobLeaseEngine) ContainerLogs(context.Context, string) ([]byte, error) {
	return nil, nil
}
func (e *deployJobLeaseEngine) InspectContainer(context.Context, string) (engine.ContainerSnapshot, error) {
	return engine.ContainerSnapshot{}, nil
}
func (e *deployJobLeaseEngine) StopContainer(context.Context, string, int) error { return nil }
func (e *deployJobLeaseEngine) RemoveContainer(context.Context, string) error    { return nil }

func TestVolumeFreeJobRunsWithUnreadableSibling(t *testing.T) {
	ctx := context.Background()
	a, b := scope.ID{Project: "one", Environment: "dev"}, scope.ID{Project: "two", Environment: "dev"}
	sh, f := hostedFile(t, a, b)
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	c := h.runtimes[a].deployment
	m := manifest.Manifest{Project: "one", Network: "overlay", Apps: map[string]manifest.App{"web": {
		Kind: "image", Image: "example/web", Roles: map[string]manifest.Role{"web": {Health: &manifest.Health{Test: []string{"CMD", "true"}}}},
	}}}
	seed, _, err := c.Store.Admit(ctx, state.Admission{RequestID: "seed", ActorID: "actor", Kind: "deploy", App: "web", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Store.Finish(ctx, seed.ID, state.Outcome{State: "succeeded", Release: &state.Release{
		App: "web", Digest: "example/web@sha256:" + strings.Repeat("a", 64), Manifest: raw,
	}}); err != nil {
		t.Fatal(err)
	}
	h.runtimes[b].deployment.Store.Close()
	finish := make(chan struct{})
	close(finish)
	c.Engine = &deployJobLeaseEngine{jobStarted: make(chan struct{}), finishJob: finish}
	out, err := c.RunJob(ctx, m, "smoke", manifest.Job{App: "web", Role: "web", Command: []string{"true"}}, 1)
	if err != nil || out.Exit == nil || *out.Exit != 0 || out.TimedOut {
		t.Errorf("volume-free job did not succeed: %+v, %v", out, err)
	}
	if _, err = h.reserve(a, []state.Claim{{Kind: state.ClaimVolume, Name: "shared"}}); err == nil {
		t.Error("reserved a new shared name without peer evidence")
	}
	if claims, err := c.Store.Claims(ctx); err != nil || len(claims) != 0 || h.leases[a] != nil {
		t.Errorf("unexpected claims or leases: %v, %v, %v", claims, h.leases[a], err)
	}
}

func TestDeployFailureKeepsVolumeWhileConcurrentJobUsesIt(t *testing.T) {
	ctx := context.Background()
	id := scope.ID{Project: "one", Environment: "prod"}
	other := scope.ID{Project: "two", Environment: "prod"}
	sh, f := hostedFile(t, id, other)
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	c := h.runtimes[id].deployment
	s := c.Store
	one := 1
	m := manifest.Manifest{Project: "one", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Roles: map[string]manifest.Role{"web": {Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}, Volumes: []string{"shared:/data"}}}}}}
	seed, _, err := s.Admit(ctx, state.Admission{RequestID: "seed", ActorID: "actor", Kind: "deploy", App: "web", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(m)
	if err = s.Finish(ctx, seed.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "web", Digest: "example/web@sha256:" + strings.Repeat("a", 64), Manifest: raw}}); err != nil {
		t.Fatal(err)
	}
	docker := &deployJobLeaseEngine{jobStarted: make(chan struct{}), finishJob: make(chan struct{})}
	c.Engine = docker
	reserved := make(chan struct{})
	resume := make(chan struct{})
	original := c.Reserve
	c.Reserve = func(ctx context.Context, claims []state.Claim) ([]state.Claim, error) {
		added, err := original(ctx, claims)
		if err != nil {
			return nil, err
		}
		for _, claim := range claims {
			if claim.Kind == state.ClaimService && claim.Name == "one-prod-api-web" {
				close(reserved)
				<-resume
				break
			}
		}
		return added, nil
	}
	m.Apps["api"] = manifest.App{Kind: "image", Image: "example/api", Roles: map[string]manifest.Role{"web": {Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}, Volumes: []string{"shared:/data"}}}}
	req := deploy.Request{RequestID: "deploy", App: "api", Version: "sha256:" + strings.Repeat("b", 64), Manifest: m}
	op, _, err := c.Admit(ctx, deploy.Actor{ID: "actor"}, req)
	if err != nil {
		t.Fatal(err)
	}
	<-reserved
	jobResult := make(chan error, 1)
	go func() {
		_, err := c.RunJob(ctx, m, "report", manifest.Job{App: "web", Role: "web", Command: []string{"true"}, Timeout: "1m"}, 1)
		jobResult <- err
	}()
	<-docker.jobStarted
	close(resume)
	if err = c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := s.Operation(ctx, op.ID)
	if err != nil || result.State != "failed" {
		t.Fatalf("deploy: %+v, %v", result, err)
	}
	claim := state.Claim{Kind: state.ClaimVolume, Name: "one-prod_shared"}
	_, duringErr := h.reserve(other, []state.Claim{claim})
	close(docker.finishJob)
	if err = <-jobResult; err != nil {
		t.Fatal(err)
	}
	if duringErr == nil {
		t.Fatal("another environment took the volume while the job held it")
	}
	if _, err = h.reserve(other, []state.Claim{claim}); err == nil {
		t.Fatal("job end released the claim")
	}
}

func TestRemovalDoesNotReleaseAnActivelyLeasedClaim(t *testing.T) {
	ctx := context.Background()
	id := scope.ID{Project: "one", Environment: "prod"}
	other := scope.ID{Project: "two", Environment: "prod"}
	sh, f := hostedFile(t, id, other)
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	claim := state.Claim{Kind: state.ClaimVolume, Name: "shared"}
	added, err := h.reserve(id, []state.Claim{claim})
	if err != nil {
		t.Fatal(err)
	}
	if err = h.finish(ctx, id, nil, []state.Claim{claim}, []state.Claim{claim}); err != nil {
		t.Fatal(err)
	}
	if _, err = h.reserve(other, []state.Claim{claim}); err == nil {
		t.Fatal("removed while leased")
	}
	if err = h.finish(ctx, id, []state.Claim{claim}, added, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = h.reserve(other, []state.Claim{claim}); err == nil {
		t.Fatal("lease end released claim")
	}
	if err = h.finish(ctx, id, nil, []state.Claim{claim}, []state.Claim{claim}); err != nil {
		t.Fatal(err)
	}
	if _, err = h.reserve(other, []state.Claim{claim}); err != nil {
		t.Fatalf("sweep: %v", err)
	}
}

type absentNetworkLeaseEngine struct{ *deployJobLeaseEngine }

type changingVolumeEngine struct {
	*deployJobLeaseEngine
	exists atomic.Bool
}

func (e *changingVolumeEngine) VolumeExists(context.Context, string) (bool, error) {
	return e.exists.Load(), nil
}

func TestReleaseRechecksAbsenceAfterAnotherOperationEnds(t *testing.T) {
	ctx := context.Background()
	id := scope.ID{Project: "one", Environment: "prod"}
	other := scope.ID{Project: "two", Environment: "prod"}
	sh, f := hostedFile(t, id, other)
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	c := h.runtimes[id].deployment
	docker := &changingVolumeEngine{deployJobLeaseEngine: &deployJobLeaseEngine{}}
	c.Engine = docker
	claim := state.Claim{Kind: state.ClaimVolume, Name: "shared"}
	if err = c.Store.AddClaims(ctx, []state.Claim{claim}); err != nil {
		t.Fatal(err)
	}
	checked := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		<-checked
		added, err := h.reserve(id, []state.Claim{claim})
		if err == nil {
			docker.exists.Store(true)
			err = h.finish(ctx, id, []state.Claim{claim}, added, nil)
		}
		done <- err
	}()
	// The removal observed the volume absent before the concurrent operation created it.
	close(checked)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if err = c.FinishReservation(ctx, nil, []state.Claim{claim}, []state.Claim{claim}); err != nil {
		t.Fatal(err)
	}
	if _, err = h.reserve(other, []state.Claim{claim}); err == nil {
		t.Fatal("released a claim after the volume was created")
	}
}

func (*absentNetworkLeaseEngine) InspectNetwork(context.Context, string) (engine.Network, error) {
	return engine.Network{}, &engine.StatusError{Code: 404}
}
func TestDestroySweepKeepsTheNetworkAndActiveLeases(t *testing.T) {
	ctx := context.Background()
	id := scope.ID{Project: "one", Environment: "prod"}
	other := scope.ID{Project: "two", Environment: "prod"}
	sh, f := hostedFile(t, id, other)
	f.Environments[0].Network = "one-prod-net"
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	c := h.runtimes[id].deployment
	c.Engine = &absentNetworkLeaseEngine{&deployJobLeaseEngine{}}
	claim := state.Claim{Kind: state.ClaimVolume, Name: "shared"}
	added, err := h.reserve(id, []state.Claim{claim})
	if err != nil {
		t.Fatal(err)
	}
	destroy := func(request string) {
		t.Helper()
		op, _, err := c.Admit(ctx, deploy.Actor{ID: "admin", Admin: true}, deploy.Request{RequestID: request, Kind: "destroy", Confirm: id.String()})
		if err != nil {
			t.Fatal(err)
		}
		if err = c.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		done, err := c.Store.Operation(ctx, op.ID)
		if err != nil || done.State != "succeeded" {
			t.Fatalf("destroy: %+v %v", done, err)
		}
	}
	destroy("first")
	held, err := c.Store.Claims(ctx)
	network := state.Claim{Kind: state.ClaimNetwork, Name: "one-prod-net"}
	if err != nil || !slices.Contains(held, claim) || !slices.Contains(held, network) {
		t.Fatalf("claims with lease: %v %v", held, err)
	}
	if err = h.finish(ctx, id, []state.Claim{claim}, added, nil); err != nil {
		t.Fatal(err)
	}
	destroy("second")
	held, err = c.Store.Claims(ctx)
	if err != nil || slices.Contains(held, claim) || !slices.Contains(held, network) {
		t.Fatalf("claims after lease: %v %v", held, err)
	}
}

func (e *deployJobLeaseEngine) RemoveVolume(context.Context, string) error {
	return &engine.StatusError{Code: 404}
}
func TestRepeatedRemoveReleasesClaimAfterLeaseEnds(t *testing.T) {
	ctx := context.Background()
	id := scope.ID{Project: "one", Environment: "prod"}
	sh, f := hostedFile(t, id)
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	c := h.runtimes[id].deployment
	c.Engine = &deployJobLeaseEngine{}
	m := manifest.Manifest{Project: "one", Apps: map[string]manifest.App{"web": {Kind: "image", Roles: map[string]manifest.Role{"web": {Volumes: []string{"shared:/data"}}}}}}
	op, _, err := c.Store.Admit(ctx, state.Admission{RequestID: "seed", ActorID: "admin", Kind: "deploy", App: "web", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(m)
	if err = c.Store.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "web", Digest: "digest", Manifest: raw}}); err != nil {
		t.Fatal(err)
	}
	claim := state.Claim{Kind: state.ClaimVolume, Name: "one-prod_shared"}
	added, err := h.reserve(id, []state.Claim{claim})
	if err != nil {
		t.Fatal(err)
	}
	remove := func(request string) {
		t.Helper()
		op, _, err := c.Admit(ctx, deploy.Actor{ID: "admin", Admin: true}, deploy.Request{RequestID: request, Kind: "remove", App: "web", Volumes: true})
		if err != nil {
			t.Fatal(err)
		}
		if err = c.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		done, err := c.Store.Operation(ctx, op.ID)
		if err != nil || done.State != "succeeded" {
			t.Fatalf("remove: %+v %v", done, err)
		}
	}
	remove("first")
	held, err := c.Store.Claims(ctx)
	if err != nil || !slices.Contains(held, claim) {
		t.Fatalf("lease not held: %v %v", held, err)
	}
	if err = h.finish(ctx, id, []state.Claim{claim}, added, nil); err != nil {
		t.Fatal(err)
	}
	remove("second")
	held, err = c.Store.Claims(ctx)
	if err != nil || slices.Contains(held, claim) {
		t.Fatalf("claim after repeated remove: %v %v", held, err)
	}
}

func TestTheFirstInventoryReadOfTwoRuntimesIsScoped(t *testing.T) {
	a, b := scope.ID{Project: "one", Environment: "dev"}, scope.ID{Project: "two", Environment: "dev"}
	sh, f := hostedFile(t, a, b)
	sh.Engine = inventoryDocker(t)
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()
	for i, name := range []string{"foreign", "unclaimed"} {
		if err := h.order[i].deployment.Store.AddClaims(ctx, []state.Claim{{Kind: state.ClaimService, Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	reads := make(chan []api.App, 2)
	for _, r := range h.order {
		r := r
		var once sync.Once
		r.monitoring.mon.Checks = func(ctx context.Context) []monitor.Check {
			once.Do(func() {
				apps, err := r.monitoring.inventory(ctx)
				if err != nil {
					t.Errorf("inventory: %v", err)
				}
				reads <- apps
			})
			return nil
		}
	}
	if err := h.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		apps := <-reads
		n := 0
		for _, a := range apps {
			if a.Ref.Kind == "swarm" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("first inventory: %+v", apps)
		}
	}
}

func TestForeignIncludesTheOtherRuntimesRetainedImages(t *testing.T) {
	a, b := scope.ID{Project: "one", Environment: "dev"}, scope.ID{Project: "two", Environment: "dev"}
	sh, f := hostedFile(t, a, b)
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()
	s := h.runtimes[b].deployment.Store
	for _, r := range []struct{ id, digest string }{{"one", "old"}, {"two", "current"}} {
		op, _, err := s.Admit(ctx, state.Admission{RequestID: r.id, ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "app", Digest: r.digest, Manifest: []byte(`{"apps":{"app":{"deploy":{"retain":2}}}}`)}}); err != nil {
			t.Fatal(err)
		}
	}
	foreign, err := h.runtimes[a].deployment.Foreign(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !foreign.Images["old"] || !foreign.Images["current"] {
		t.Fatalf("foreign images: %v", foreign.Images)
	}
}

func TestReservationDoesNotReadPeerReleaseHistory(t *testing.T) {
	a, b := scope.ID{Project: "one", Environment: "dev"}, scope.ID{Project: "two", Environment: "dev"}
	sh, f := hostedFile(t, a, b)
	f.Environments[0].Network = "one-dev"
	peer := f.Environments[1]
	s, err := state.Open(peer.StateDir, peer.KeyFile, peer.Environment)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "bad", ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "app", Digest: "image", Manifest: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	h, err := Open(sh, f)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()
	if _, err = h.reserve(a, []state.Claim{{Kind: state.ClaimService, Name: "one-dev-web"}}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
}

func TestPruneAndDestroyReportUnreadableForeignHistory(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	f.ctrl.Engine = inventoryDocker(t)
	f.ctrl.Foreign = func(context.Context) (deploy.Foreign, error) {
		return deploy.Foreign{}, errors.New("peer history broken")
	}
	for _, kind := range []string{"prune", "destroy"} {
		t.Run(kind, func(t *testing.T) {
			body := `{"dry_run":true,"confirm":"demo/lab"}`
			w := f.call("POST", "/"+kind, apAdminToken, body)
			if w.Code != 400 || !strings.Contains(w.Body.String(), "release history unavailable") {
				t.Errorf("dry run: %d %s", w.Code, w.Body.String())
			}
			w = f.call("POST", "/"+kind, apAdminToken, `{"request_id":"`+kind+`","confirm":"demo/lab"}`)
			if w.Code != 202 {
				t.Fatalf("admit: %d %s", w.Code, w.Body.String())
			}
			if err := f.ctrl.Drain(context.Background()); err != nil {
				t.Fatal(err)
			}
			var accepted struct {
				OperationID string `json:"operation_id"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &accepted); err != nil {
				t.Fatal(err)
			}
			op, err := f.s.Operation(context.Background(), accepted.OperationID)
			if err != nil || op.State != "failed" || op.Err != "release history unavailable" {
				t.Fatalf("operation %+v %v", op, err)
			}
		})
	}
}

func TestTheFirstExportOfTwoRuntimesIsScoped(t *testing.T) {
	lab, prod := scope.ID{Project: "demo", Environment: "lab"}, scope.ID{Project: "shop", Environment: "prod"}
	sh, f := hostedFile(t, lab, prod)
	sh.Collector = sampledCollector(t, metrics.Host{}, []metrics.ContainerRef{
		{ID: "lab-web", Labels: lab.Labels()},
		{ID: "prod-web", Labels: prod.Labels()},
	})
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	sinks := []*otlpSink{newOTLPSink(t), newOTLPSink(t)}
	for i, r := range h.order {
		endpoint := sinks[i].srv.URL
		r.exporter.Config = func(context.Context) (*telemetry.Config, error) {
			return &telemetry.Config{Endpoint: endpoint}, nil
		}
	}
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i, id := range []scope.ID{lab, prod} {
		if err := h.order[i].exporter.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
		ids := containerIDsIn(sinks[i].wait(t, t.Context()))
		want := id.Environment + "-web"
		other := "prod-web"
		if id == prod {
			other = "lab-web"
		}
		if !ids[want] {
			t.Errorf("%s first export missing own container: %v", id, ids)
		}
		if ids[other] {
			t.Errorf("%s first export included the other environment: %v", id, ids)
		}
	}
}

func TestClosingTheHostDoesNotRaceAnInFlightExport(t *testing.T) {
	lab, prod := scope.ID{Project: "demo", Environment: "lab"}, scope.ID{Project: "shop", Environment: "prod"}
	sh, f := hostedFile(t, lab, prod)
	sh.Collector = sampledCollector(t, metrics.Host{}, []metrics.ContainerRef{
		{ID: "lab-web", Labels: lab.Labels()},
		{ID: "prod-web", Labels: prod.Labels()},
	})
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	sinks := []*otlpSink{newOTLPSink(t), newOTLPSink(t)}
	for i, r := range h.order {
		endpoint := sinks[i].srv.URL
		r.exporter.Config = func(context.Context) (*telemetry.Config, error) {
			return &telemetry.Config{Endpoint: endpoint}, nil
		}
	}
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{}, len(h.order))
	var wg sync.WaitGroup
	for _, r := range h.order {
		wg.Add(1)
		go func(exp *telemetry.Exporter) {
			defer wg.Done()
			if err := exp.Flush(ctx); err != nil && ctx.Err() == nil {
				t.Errorf("flush: %v", err)
			}
			started <- struct{}{}
			for ctx.Err() == nil {
				_ = exp.Flush(ctx)
			}
		}(r.exporter)
	}
	for range h.order {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("export never started")
		}
	}
	h.Close()
	cancel()
	wg.Wait()
}

func TestForeignIncludesPeerIngress(t *testing.T) {
	ids := []scope.ID{{Project: "one", Environment: "prod"}, {Project: "two", Environment: "prod"}}
	sh, file := hostedFile(t, ids...)
	h, err := Open(sh, file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	ctx := context.Background()
	peer := h.runtimes[ids[1]].deployment.Store
	op, _, err := peer.Admit(ctx, state.Admission{RequestID: "ingress", ActorID: "admin", Kind: "ingress", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.SetIngress(ctx, op.ID, state.IngressState{AccountID: "acct", APITokenSecret: "TOKEN", TunnelID: "tunnel", TunnelName: "shared"}); err != nil {
		t.Fatal(err)
	}
	f, err := h.foreign(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Ingress) != 1 || f.Ingress[0].AccountID != "acct" || f.Ingress[0].TunnelID != "tunnel" {
		t.Fatalf("peer ingress missing: %+v", f)
	}
	if err := peer.Finish(ctx, op.ID, state.Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
}

func TestForeignIncludesPeerKeptTunnel(t *testing.T) {
	ids := []scope.ID{{Project: "one", Environment: "prod"}, {Project: "two", Environment: "prod"}}
	sh, file := hostedFile(t, ids...)
	h, err := Open(sh, file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	ctx := context.Background()
	peer := h.runtimes[ids[1]].deployment.Store
	op, _, err := peer.Admit(ctx, state.Admission{RequestID: "kept", ActorID: "admin", Kind: "destroy", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = peer.SetKeptTunnel(ctx, op.ID, state.IngressState{AccountID: "acct", APITokenSecret: "TOKEN", TunnelID: "kept-id", TunnelName: "shared"}); err != nil {
		t.Fatal(err)
	}
	if err = peer.Finish(ctx, op.ID, state.Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	foreign, err := h.foreign(ctx, ids[0])
	if err != nil || len(foreign.Ingress) != 1 || foreign.Ingress[0].TunnelID != "kept-id" {
		t.Fatalf("foreign: %+v, %v", foreign, err)
	}
}

type hostedSecretInventory struct {
	generateEngine
	calls atomic.Int32
}

func (e *hostedSecretInventory) ComposeContainers(context.Context, string) ([]engine.ComposeContainer, error) {
	e.calls.Add(1)
	return []engine.ComposeContainer{{State: "exited"}}, nil
}
func (e *hostedSecretInventory) ProjectVolumes(context.Context, string) ([]string, error) {
	e.calls.Add(1)
	return []string{"private-volume"}, nil
}
func (e *hostedSecretInventory) VolumeExists(context.Context, string) (bool, error) {
	e.calls.Add(1)
	return true, nil
}
func (e *hostedSecretInventory) InspectService(context.Context, string) (engine.ServiceSnapshot, error) {
	e.calls.Add(1)
	return engine.ServiceSnapshot{}, &engine.StatusError{Code: 404}
}

func TestGenerateDeclaredAcrossTwoEnvironments(t *testing.T) {
	owner, requester := scope.ID{Project: "one", Environment: "prod"}, scope.ID{Project: "two", Environment: "prod"}
	sh, f := hostedFile(t, owner, requester)
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	inventory := &hostedSecretInventory{}
	h.runtimes[requester].deployment.Engine = inventory
	if err := h.runtimes[owner].deployment.Store.AddClaims(t.Context(), []state.Claim{{Kind: state.ClaimComposeProject, Name: "shared"}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	body := `{"manifest":{"project":"two","network":"overlay","apps":{"db":{"kind":"compose","file":"compose.yml","project_name":"shared","env":{"secret":["DB_PASS"]}}},"secrets":{"DB_PASS":{"generate":{"format":"hex"}}}},"compose_files":{"db":"volumes: {data: {name: private-volume}}"}}`
	w := serve(h, "POST", requester.APIPath()+"secrets/generate-declared", adminOf(f, 1), body)
	refused := strings.Contains(w.Body.String(), "Compose project name is taken by another project or environment on this server")
	volumeExposed := strings.Contains(w.Body.String(), "private-volume")
	if w.Code != 400 || !refused || volumeExposed {
		t.Fatalf("status %d, expected refusal %t, volume exposed %t", w.Code, refused, volumeExposed)
	}
	if n := inventory.calls.Load(); n != 0 {
		t.Fatalf("foreign inventory calls: %d", n)
	}
	secrets, err := h.runtimes[requester].deployment.Store.Secrets(t.Context())
	if err != nil || len(secrets) != 0 {
		t.Fatalf("secrets: %v %v", secrets, err)
	}
}
