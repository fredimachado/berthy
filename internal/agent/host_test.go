package agent

import (
	"context"
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
	"time"

	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/metrics"
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

func (e *deployJobLeaseEngine) InspectNetwork(context.Context, string) (engine.Network, error) {
	return engine.Network{ID: "net", Driver: "overlay", Scope: "swarm", Attachable: true}, nil
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
		op, _, err := c.Admit(ctx, deploy.Actor{ID: "admin", Admin: true}, deploy.Request{RequestID: request, Kind: "destroy", Confirm: id.Environment})
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
