package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/engine"
	"berthy/internal/monitor"
	"berthy/internal/scope"
	"berthy/internal/state"
)

// hostToken is the admin token of the hosted environment id. The digit makes it differ between environments of one file,
// which refuses a token digest it has twice.
func hostToken(id scope.ID, digit string) string {
	return "bty." + id.Environment + "." + strings.Repeat(digit, 64)
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

func TestOpenClosesWhatItOpenedWhenOneEnvironmentFails(t *testing.T) {
	lab, dev := scope.ID{Project: "demo", Environment: "lab"}, scope.ID{Project: "demo", Environment: "dev"}
	sh, f := hostedFile(t, lab, dev)
	// The second state is made with one key and is given another.
	s, err := state.Open(f.Environments[1].StateDir, f.Environments[1].KeyFile, "dev")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	wrong := filepath.Join(filepath.Dir(f.Environments[1].KeyFile), "wrong")
	if err = os.WriteFile(wrong, []byte(strings.Repeat("x", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	f.Environments[1].KeyFile = wrong
	if h, err := Open(sh, f); err == nil {
		h.Close()
		t.Fatal("Open accepted a wrong key")
	}
	openable(t, f.Environments[0])
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
		h, err := Open(sh, g)
		if err == nil {
			h.Close()
			t.Fatalf("%s: Open accepted the state", name)
		}
		if !strings.Contains(err.Error(), "state in "+f.Environments[1].StateDir+":") || !strings.Contains(err.Error(), "state belongs to demo/dev, not ") {
			t.Errorf("%s: %v", name, err)
		}
		// The state of lab, opened before the refusal, is closed again.
		openable(t, f.Environments[0])
	}
}

type hostCheck struct{}

func (hostCheck) Name() string            { return "host-test" }
func (hostCheck) Interval() time.Duration { return time.Hour }
func (hostCheck) Failures() int           { return 1 }
func (hostCheck) Run(context.Context) monitor.Result {
	return monitor.Result{State: monitor.Warning}
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
