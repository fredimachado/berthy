package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"berthy/internal/cloudflare"
	"berthy/internal/cloudflare/cftest"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/scope"
	"berthy/internal/state"
)

// The Cloudflare token and secret names are test values. They never leave the fake.
const (
	ingressAccount = "acct"
	ingressToken   = "cf-test-token-not-real"
	ingressSecret  = "CF_TOKEN"
	ingressEnv     = "test"
	ingressTunnel  = "demo-test"
	apiTarget      = "http://demo-test-api-web:8080"
)

var ingressID = scope.ID{Project: "demo", Environment: ingressEnv}

// updatableFake is boundFake whose service updates store the new spec, as Docker does.
type updatableFake struct{ *boundFake }

func (f updatableFake) UpdateService(_ context.Context, id string, _ uint64, spec json.RawMessage, _ *engine.RegistryAuth) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.services {
		if f.services[i].ID == id {
			f.services[i].Spec = append(json.RawMessage(nil), spec...)
			return nil
		}
	}
	return &engine.StatusError{Code: 404}
}

// callLog records, in order, the writes a test may need to order: Docker's service and one-off changes, and Cloudflare's
// requests that are not reads.
type callLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *callLog) add(entry string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entry)
}

func (l *callLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.entries...)
}

func (l *callLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = nil
}

// index returns the position of the first entry that starts with prefix, or -1.
func (l *callLog) index(prefix string) int {
	for i, e := range l.all() {
		if strings.HasPrefix(e, prefix) {
			return i
		}
	}
	return -1
}

// count returns how many entries start with prefix.
func (l *callLog) count(prefix string) int {
	n := 0
	for _, e := range l.all() {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

// loggedEngine is updatableFake that records service writes and one-off containers in the call log. Every one-off
// container exits with exit.
type loggedEngine struct {
	updatableFake
	calls *callLog
	exit  int
}

func (e *loggedEngine) CreateService(ctx context.Context, spec json.RawMessage, auth *engine.RegistryAuth) (string, error) {
	e.calls.add("service create " + specName(spec))
	return e.updatableFake.CreateService(ctx, spec, auth)
}

func (e *loggedEngine) UpdateService(ctx context.Context, id string, version uint64, spec json.RawMessage, auth *engine.RegistryAuth) error {
	e.calls.add("service update " + specName(spec))
	return e.updatableFake.UpdateService(ctx, id, version, spec, auth)
}

func (e *loggedEngine) CreateContainer(_ context.Context, name string, _ json.RawMessage) (string, error) {
	e.calls.add("one-off " + name)
	return name, nil
}

func (e *loggedEngine) StartContainer(context.Context, string) error { return nil }

func (e *loggedEngine) WaitContainer(context.Context, string) (int, error) { return e.exit, nil }

func (e *loggedEngine) ContainerLogs(context.Context, string) ([]byte, error) {
	return []byte("v1\n"), nil
}

func (e *loggedEngine) InspectContainer(context.Context, string) (engine.ContainerSnapshot, error) {
	return engine.ContainerSnapshot{}, &engine.StatusError{Code: 404}
}

func (e *loggedEngine) StopContainer(context.Context, string, int) error { return nil }

// logRequests records each Cloudflare request that is not a read, before the fake answers it.
func logRequests(calls *callLog, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			calls.add("cloudflare " + r.Method + " " + strings.TrimPrefix(r.URL.Path, "/client/v4"))
		}
		next.ServeHTTP(w, r)
	})
}

// ingressFixture is one environment: a store, a Docker fake, and a Cloudflare account with one zone. calls records the
// writes to either.
type ingressFixture struct {
	t      *testing.T
	store  *state.Store
	docker *loggedEngine
	bound  *boundFake
	calls  *callLog
	cf     *cftest.Fake
	api    *cloudflare.Client
	zone   string
	ctrl   *Controller
}

func newIngressFixture(t *testing.T) *ingressFixture {
	t.Helper()
	s := openStore(t)
	if err := s.PutSecret(context.Background(), state.Actor{ID: "admin"}, state.SecretInfo{Name: ingressSecret}, []byte(ingressToken)); err != nil {
		t.Fatal(err)
	}
	fake := cftest.New(ingressToken, ingressAccount)
	calls := &callLog{}
	srv := httptest.NewServer(logRequests(calls, fake.Handler()))
	t.Cleanup(srv.Close)
	bound := &boundFake{}
	docker := &loggedEngine{updatableFake: updatableFake{bound}, calls: calls}
	ctrl := &Controller{Engine: docker, Store: s, Project: ingressID.Project, Environment: ingressEnv, CloudflareURL: srv.URL + "/client/v4"}
	// The hostnames of the fixture's apps lead nowhere, and a test must not ask the real network for them.
	ctrl.Outside = &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("no network in tests")
	}}}
	ctrl.outsidePause = time.Millisecond
	return &ingressFixture{
		t:      t,
		store:  s,
		docker: docker,
		bound:  bound,
		calls:  calls,
		cf:     fake,
		api:    &cloudflare.Client{BaseURL: srv.URL + "/client/v4", Token: ingressToken},
		zone:   fake.AddZone("example.com"),
		ctrl:   ctrl,
	}
}

// ingressManifest is a manifest with managed ingress and one image app, api, whose web role publishes api.example.com.
// extra is appended to the cloudflare block, for example `,"dns":false`.
func ingressManifest(t *testing.T, extra string) manifest.Manifest {
	t.Helper()
	return ingressManifestImage(t, "cloudflare/cloudflared@sha256:"+strings.Repeat("c", 64), extra)
}

// ingressManifestImage is ingressManifest with the connector image set to image, a pinned reference.
func ingressManifestImage(t *testing.T, image, extra string) manifest.Manifest {
	t.Helper()
	input := `{"project":"demo","network":"overlay","ingress":{"cloudflare":{"account_id":"acct","api_token_secret":"CF_TOKEN","image":"` + image + `"` + extra + `}},` + apiApp(true) + `}`
	m, err := manifest.DecodeJSON([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	return *m
}

// plainManifest has no ingress block, so an ingress run with it removes managed ingress.
func plainManifest(t *testing.T) manifest.Manifest {
	t.Helper()
	m, err := manifest.DecodeJSON([]byte(`{"project":"demo","network":"overlay",` + apiApp(false) + `}`))
	if err != nil {
		t.Fatal(err)
	}
	return *m
}

// apiApp is the apps block: one image app, api, with a web role on port 8080 that may publish a hostname.
func apiApp(publish bool) string {
	hosts := ""
	if publish {
		hosts = `,"hostnames":["api.example.com"]`
	}
	return `"apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{"port":8080,"health":{"test":["CMD","true"]}` + hosts + `}}}}`
}

// seedDeployed records a succeeded deploy of app, bound to the given services, so the app counts as deployed.
func seedDeployed(t *testing.T, s *state.Store, app string, bindings ...state.Binding) {
	t.Helper()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "seed-" + app, ActorID: "admin", Kind: "deploy", App: app, PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	release := &state.Release{App: app, Digest: "digest-" + app, Manifest: json.RawMessage(`{}`), Bindings: bindings}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: release}); err != nil {
		t.Fatal(err)
	}
}

// apiBinding is the web role of the deployed api app, bound to its service.
func apiBinding() state.Binding {
	return state.Binding{App: "api", Role: "web", Name: "demo-test-api-web", ServiceID: "api-web-id"}
}

// run admits an ingress request and waits for it to finish.
func (f *ingressFixture) run(req Request) state.Operation {
	f.t.Helper()
	op, _, err := admit(f.t, f.ctrl, context.Background(), Actor{ID: "admin", Admin: true}, req)
	if err != nil {
		f.t.Fatal(err)
	}
	return waitOperation(f.t, f.store, op.ID)
}

// tunnel returns the environment's tunnel, or nil when Cloudflare has none with the configured name.
func (f *ingressFixture) tunnel() *cloudflare.Tunnel {
	f.t.Helper()
	tunnel, err := f.api.FindTunnel(context.Background(), ingressAccount, ingressTunnel)
	if err != nil {
		f.t.Fatal(err)
	}
	return tunnel
}

// tunnelConfig returns a tunnel's configuration as Cloudflare stores it.
func (f *ingressFixture) tunnelConfig(id string) cloudflare.TunnelConfig {
	f.t.Helper()
	var cfg cloudflare.TunnelConfig
	if raw := f.cf.Config(id); len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			f.t.Fatal(err)
		}
	}
	return cfg
}

// connectorMissing reports whether the connector service is absent from the engine.
func (f *ingressFixture) connectorMissing() bool {
	_, err := f.docker.InspectService(context.Background(), "demo-test-ingress-connector")
	return serviceMissing(err)
}

// describeRules lists each rule as hostname|path|service, in order.
func describeRules(cfg cloudflare.TunnelConfig) []string {
	var out []string
	for _, r := range cfg.Ingress {
		out = append(out, r.Hostname+"|"+r.Path+"|"+r.Service)
	}
	return out
}

func rule(hostname, service string) cloudflare.Rule {
	return cloudflare.Rule{Hostname: hostname, Service: service}
}

func rulesOf(rs ...cloudflare.Rule) cloudflare.TunnelConfig {
	return cloudflare.TunnelConfig{Ingress: rs}
}

func TestMergeRoutesPlacesBerthyFirstAndKeepsTheRest(t *testing.T) {
	apiWant := []RouteWant{{Hostname: "api.example.com", App: "api", Role: "web", Service: apiTarget}}
	apiOwned := map[string]state.IngressRoute{"api.example.com": {Hostname: "api.example.com", App: "api", Role: "web", Service: apiTarget, RouteCreated: true, DNSCreated: true}}
	removable := map[string]bool{"api.example.com": true}
	origin := cloudflare.Rule{Hostname: "other.example.com", Service: "http://other:80", Extra: map[string]json.RawMessage{"originRequest": json.RawMessage(`{"connectTimeout":"5s"}`)}}
	cases := []struct {
		name      string
		cfg       cloudflare.TunnelConfig
		wants     []RouteWant
		owned     map[string]state.IngressRoute
		removable map[string]bool
		replace   bool
		want      []string
		routes    map[string]string
		conflicts int
	}{
		{
			name:   "empty tunnel gets the route and a 404 catch-all",
			wants:  apiWant,
			want:   []string{"api.example.com||" + apiTarget, "||http_status:404"},
			routes: map[string]string{"api.example.com": "add"},
		},
		{
			name:   "foreign rules keep their order behind Berthy's and the catch-all stays last",
			cfg:    rulesOf(origin, rule("second.example.com", "http://second:80"), rule("", "http_status:503")),
			wants:  apiWant,
			want:   []string{"api.example.com||" + apiTarget, "other.example.com||http://other:80", "second.example.com||http://second:80", "||http_status:503"},
			routes: map[string]string{"api.example.com": "add"},
		},
		{
			name:   "a foreign rule with the same target is adopted as it is",
			cfg:    rulesOf(rule("api.example.com", apiTarget), rule("", "http_status:404")),
			wants:  apiWant,
			want:   []string{"api.example.com||" + apiTarget, "||http_status:404"},
			routes: map[string]string{"api.example.com": "adopt"},
		},
		{
			name:      "a foreign rule with another target is a conflict and nothing is merged",
			cfg:       rulesOf(rule("api.example.com", "http://old:80"), rule("", "http_status:404")),
			wants:     apiWant,
			want:      []string{"api.example.com||http://old:80", "||http_status:404"},
			routes:    map[string]string{},
			conflicts: 1,
		},
		{
			name:    "replace takes over a foreign rule with another target",
			cfg:     rulesOf(rule("api.example.com", "http://old:80"), rule("", "http_status:404")),
			wants:   apiWant,
			replace: true,
			want:    []string{"api.example.com||" + apiTarget, "||http_status:404"},
			routes:  map[string]string{"api.example.com": "change"},
		},
		{
			name:   "an owned rule edited in the dashboard is overwritten",
			cfg:    rulesOf(rule("api.example.com", "http://edited:80"), rule("", "http_status:404")),
			wants:  apiWant,
			owned:  apiOwned,
			want:   []string{"api.example.com||" + apiTarget, "||http_status:404"},
			routes: map[string]string{"api.example.com": "change"},
		},
		{
			name:   "an owned rule that matches is kept",
			cfg:    rulesOf(rule("api.example.com", apiTarget), rule("", "http_status:404")),
			wants:  apiWant,
			owned:  apiOwned,
			want:   []string{"api.example.com||" + apiTarget, "||http_status:404"},
			routes: map[string]string{"api.example.com": "keep"},
		},
		{
			name:      "an owned hostname that is no longer wanted is removed",
			cfg:       rulesOf(rule("api.example.com", apiTarget), rule("other.example.com", "http://other:80"), rule("", "http_status:404")),
			owned:     apiOwned,
			removable: removable,
			want:      []string{"other.example.com||http://other:80", "||http_status:404"},
			routes:    map[string]string{"api.example.com": "remove"},
		},
		{
			name:   "an owned hostname that may not be removed is left untouched",
			cfg:    rulesOf(rule("api.example.com", apiTarget), rule("", "http_status:404")),
			owned:  apiOwned,
			want:   []string{"api.example.com||" + apiTarget, "||http_status:404"},
			routes: map[string]string{},
		},
		{
			name:   "a route Berthy created for another app that already matches is adopted",
			cfg:    rulesOf(rule("api.example.com", apiTarget), rule("", "http_status:404")),
			wants:  apiWant,
			owned:  map[string]state.IngressRoute{"api.example.com": {Hostname: "api.example.com", App: "admin", Role: "web", Service: apiTarget}},
			want:   []string{"api.example.com||" + apiTarget, "||http_status:404"},
			routes: map[string]string{"api.example.com": "keep"},
		},
		{
			name:      "a route Berthy created for another app with another target is a conflict",
			cfg:       rulesOf(rule("api.example.com", "http://old:80"), rule("", "http_status:404")),
			wants:     apiWant,
			owned:     map[string]state.IngressRoute{"api.example.com": {Hostname: "api.example.com", App: "admin", Role: "web", Service: "http://old:80"}},
			want:      []string{"api.example.com||http://old:80", "||http_status:404"},
			routes:    map[string]string{},
			conflicts: 1,
		},
		{
			name:    "replace hands a route Berthy created for another app over",
			cfg:     rulesOf(rule("api.example.com", "http://old:80"), rule("", "http_status:404")),
			wants:   apiWant,
			owned:   map[string]state.IngressRoute{"api.example.com": {Hostname: "api.example.com", App: "admin", Role: "web", Service: "http://old:80"}},
			replace: true,
			want:    []string{"api.example.com||" + apiTarget, "||http_status:404"},
			routes:  map[string]string{"api.example.com": "change"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, changes, conflicts := mergeRoutes(tc.cfg, tc.wants, tc.owned, tc.removable, tc.replace)
			if got := describeRules(merged); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("rules %v, want %v", got, tc.want)
			}
			if len(conflicts) != tc.conflicts {
				t.Fatalf("conflicts %v", conflicts)
			}
			got := map[string]string{}
			for _, ch := range changes {
				got[ch.Hostname] = ch.Route
			}
			if !reflect.DeepEqual(got, tc.routes) && !(len(got) == 0 && len(tc.routes) == 0) {
				t.Fatalf("changes %v, want %v", got, tc.routes)
			}
		})
	}
}

func TestMergeRoutesReportsPreviousAndKeepsExtra(t *testing.T) {
	origin := json.RawMessage(`{"connectTimeout":"9s"}`)
	cfg := rulesOf(cloudflare.Rule{Hostname: "api.example.com", Service: "http://old:80", Extra: map[string]json.RawMessage{"originRequest": origin}}, rule("", "http_status:404"))
	want := []RouteWant{{Hostname: "api.example.com", App: "api", Role: "web", Service: apiTarget}}
	owned := map[string]state.IngressRoute{"api.example.com": {Hostname: "api.example.com", App: "api", Role: "web", Service: "http://old:80", RouteCreated: true, DNSCreated: true}}
	merged, changes, conflicts := mergeRoutes(cfg, want, owned, nil, false)
	if len(conflicts) != 0 || len(changes) != 1 || changes[0].Previous != "http://old:80" || changes[0].Target != apiTarget {
		t.Fatalf("changes %+v conflicts %v", changes, conflicts)
	}
	if got := string(merged.Ingress[0].Extra["originRequest"]); got != string(origin) {
		t.Fatalf("origin request %q, want %q", got, origin)
	}
	_, removal, _ := mergeRoutes(rulesOf(rule("api.example.com", apiTarget), rule("", "http_status:404")), nil, owned, map[string]bool{"api.example.com": true}, false)
	if len(removal) != 1 || removal[0].Route != "remove" || removal[0].Previous != apiTarget {
		t.Fatalf("removal %+v", removal)
	}
}

// openIngressPlan admits an ingress operation that only holds the lock, so tests can seed state under it.
func openIngressPlan(t *testing.T, s *state.Store) state.Operation {
	t.Helper()
	op, _, err := s.Admit(context.Background(), state.Admission{RequestID: "plan-" + t.Name(), ActorID: "admin", Kind: "ingress", App: "ingress", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Finish(context.Background(), op.ID, state.Outcome{State: "failed"}) })
	return op
}

func TestDNSPlanningDecidesEachRecord(t *testing.T) {
	const host = "api.example.com"
	wantAPI := []RouteWant{{Hostname: host, App: "api", Role: "web", Service: apiTarget}}
	managed := func(f *ingressFixture, content string, proxied bool) string {
		return f.cf.AddRecord(f.zone, cloudflare.DNSRecord{Type: "CNAME", Name: host, Content: content, Proxied: proxied, Comment: cloudflare.RecordComment(ingressID)})
	}
	foreignCNAME := func(f *ingressFixture, content string) string {
		return f.cf.AddRecord(f.zone, cloudflare.DNSRecord{Type: "CNAME", Name: host, Content: content, Proxied: true})
	}
	routeOf := func(t *testing.T, f *ingressFixture, op state.Operation, recordID string) {
		t.Helper()
		route := state.IngressRoute{Hostname: host, App: "api", Role: "web", Service: apiTarget, ZoneID: f.zone, DNSRecordID: recordID, RouteCreated: true, DNSCreated: true}
		if err := f.store.SetIngressRoute(context.Background(), op.ID, route); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name         string
		wants        []RouteWant
		replace      bool
		seed         func(t *testing.T, f *ingressFixture, tunnelID string, op state.Operation)
		wantDNS      string
		wantConflict string
	}{
		{
			name:    "no record is added",
			wants:   wantAPI,
			wantDNS: "add",
		},
		{
			name:  "an owned managed record pointing at the tunnel is kept",
			wants: wantAPI,
			seed: func(t *testing.T, f *ingressFixture, tid string, op state.Operation) {
				routeOf(t, f, op, managed(f, cloudflare.TunnelTarget(tid), true))
			},
			wantDNS: "keep",
		},
		{
			name:  "an owned managed record with another target is changed",
			wants: wantAPI,
			seed: func(t *testing.T, f *ingressFixture, tid string, op state.Operation) {
				routeOf(t, f, op, managed(f, "old.cfargotunnel.com", true))
			},
			wantDNS: "change",
		},
		{
			name:  "a proxied CNAME to the tunnel that Berthy did not create is adopted",
			wants: wantAPI,
			seed: func(t *testing.T, f *ingressFixture, tid string, _ state.Operation) {
				foreignCNAME(f, cloudflare.TunnelTarget(tid))
			},
			wantDNS: "adopt",
		},
		{
			name:  "a record that is not ours stops the run",
			wants: wantAPI,
			seed: func(t *testing.T, f *ingressFixture, _ string, _ state.Operation) {
				f.cf.AddRecord(f.zone, cloudflare.DNSRecord{Type: "A", Name: host, Content: "203.0.113.9"})
			},
			wantConflict: "DNS name " + host + " is used by another record",
		},
		{
			name:    "replace updates a single foreign CNAME",
			wants:   wantAPI,
			replace: true,
			seed: func(t *testing.T, f *ingressFixture, _ string, _ state.Operation) {
				foreignCNAME(f, "elsewhere.example.net")
			},
			wantDNS: "change",
		},
		{
			name:    "replace adds ours and deletes foreign address records",
			wants:   wantAPI,
			replace: true,
			seed: func(t *testing.T, f *ingressFixture, _ string, _ state.Operation) {
				f.cf.AddRecord(f.zone, cloudflare.DNSRecord{Type: "A", Name: host, Content: "203.0.113.9"})
			},
			wantDNS: "add",
		},
		{
			name:         "no zone is a conflict even with replace",
			wants:        []RouteWant{{Hostname: "api.other.test", App: "api", Role: "web", Service: apiTarget}},
			replace:      true,
			wantConflict: "no Cloudflare zone for api.other.test",
		},
		{
			name: "removal deletes the managed record Berthy recorded",
			seed: func(t *testing.T, f *ingressFixture, tid string, op state.Operation) {
				routeOf(t, f, op, managed(f, cloudflare.TunnelTarget(tid), true))
			},
			wantDNS: "remove",
		},
		{
			name: "removal leaves a record Berthy did not record",
			seed: func(t *testing.T, f *ingressFixture, tid string, op state.Operation) {
				routeOf(t, f, op, "not-the-record")
				foreignCNAME(f, cloudflare.TunnelTarget(tid))
			},
			wantDNS: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newIngressFixture(t)
			tid := f.cf.AddTunnel(ingressTunnel, true)
			op := openIngressPlan(t, f.store)
			if tc.seed != nil {
				tc.seed(t, f, tid, op)
			}
			sy := &ingressSync{c: f.ctrl, cf: f.api, opID: op.ID, st: state.IngressState{AccountID: ingressAccount, APITokenSecret: ingressSecret, TunnelID: tid, TunnelName: ingressTunnel, DNS: true}}
			plan, _, steps, err := sy.plan(context.Background(), tc.wants, everyRoute, tc.replace)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantConflict != "" {
				if len(plan.Conflicts) != 1 || plan.Conflicts[0] != tc.wantConflict {
					t.Fatalf("conflicts %v, want %q", plan.Conflicts, tc.wantConflict)
				}
				return
			}
			if len(plan.Conflicts) != 0 {
				t.Fatalf("unexpected conflicts %v", plan.Conflicts)
			}
			got := ""
			for _, ch := range plan.Changes {
				if ch.Hostname == host {
					got = ch.DNS
				}
			}
			if got != tc.wantDNS {
				t.Fatalf("dns %q, want %q (steps %+v)", got, tc.wantDNS, steps)
			}
			if tc.wantDNS == "add" && strings.Contains(tc.name, "adds ours") && len(steps[0].Replace) != 1 {
				t.Fatalf("replace records %+v", steps[0].Replace)
			}
		})
	}
}

func TestIngressApplyCreatesTunnelConnectorRouteAndRecord(t *testing.T) {
	f := newIngressFixture(t)
	seedDeployed(t, f.store, "api", apiBinding())
	op := f.run(Request{RequestID: "apply-1", Kind: "ingress", Commit: "c1", Manifest: ingressManifest(t, "")})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	ctx := context.Background()
	tunnel := f.tunnel()
	if tunnel == nil || !tunnel.RemoteConfig {
		t.Fatalf("tunnel %+v", tunnel)
	}

	svc, err := f.docker.InspectService(ctx, "demo-test-ingress-connector")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Labels       map[string]string
		TaskTemplate struct {
			ContainerSpec struct {
				Args        []string
				Env         []string
				Healthcheck struct{ Test []string }
			}
		}
	}
	if err = json.Unmarshal(svc.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	container := spec.TaskTemplate.ContainerSpec
	if want := []string{"tunnel", "--no-autoupdate", "--metrics", "0.0.0.0:20241", "run"}; !reflect.DeepEqual(container.Args, want) {
		t.Fatalf("args %v", container.Args)
	}
	if !strings.Contains(strings.Join(container.Healthcheck.Test, " "), "tunnel --metrics 127.0.0.1:20241 ready") {
		t.Fatalf("healthcheck %v", container.Healthcheck.Test)
	}
	// The connector carries the tunnel token and, like every image app, its Berthy identity.
	if want := "TUNNEL_TOKEN=" + f.cf.TunnelToken(tunnel.ID); !slices.Contains(container.Env, want) || !slices.Contains(container.Env, "BERTHY_APP=ingress") {
		t.Fatalf("connector environment %q does not carry the tunnel token and the Berthy identity", container.Env)
	}
	if spec.Labels["berthy.app"] != "ingress" || spec.Labels["berthy.environment"] != ingressEnv {
		t.Fatalf("labels %v", spec.Labels)
	}
	if _, value, err := f.store.Secret(ctx, manifest.TunnelTokenSecret); err != nil || string(value) != f.cf.TunnelToken(tunnel.ID) {
		t.Fatalf("stored tunnel token: %v", err)
	}
	if _, err = f.store.Applied(ctx, "ingress"); err != nil {
		t.Fatalf("no connector release: %v", err)
	}

	if got := describeRules(f.tunnelConfig(tunnel.ID)); !reflect.DeepEqual(got, []string{"api.example.com||" + apiTarget, "||http_status:404"}) {
		t.Fatalf("tunnel rules %v", got)
	}
	records := f.cf.Records(f.zone)
	if len(records) != 1 || records[0].Type != "CNAME" || records[0].Name != "api.example.com" || records[0].Content != cloudflare.TunnelTarget(tunnel.ID) || !records[0].Proxied || records[0].Comment != cloudflare.RecordComment(ingressID) {
		t.Fatalf("records %+v", records)
	}
	routes, err := f.store.IngressRoutes(ctx, "")
	if err != nil || len(routes) != 1 || routes[0].Service != apiTarget || routes[0].DNSRecordID != records[0].ID || routes[0].ZoneID != f.zone {
		t.Fatalf("routes %+v %v", routes, err)
	}

	puts, writes := f.cf.ConfigPuts, f.cf.RecordWrites
	second := f.run(Request{RequestID: "apply-2", Kind: "ingress", Commit: "c2", Manifest: ingressManifest(t, "")})
	if second.State != "succeeded" {
		t.Fatalf("second apply %s: %s", second.State, second.Err)
	}
	if f.cf.ConfigPuts != puts || f.cf.RecordWrites != writes {
		t.Fatalf("second apply wrote: puts %d->%d writes %d->%d", puts, f.cf.ConfigPuts, writes, f.cf.RecordWrites)
	}
}

// connectorService returns the live connector service of the environment, or fails the test.
func (f *ingressFixture) connectorService() engine.ServiceSnapshot {
	f.t.Helper()
	svc, err := f.docker.InspectService(context.Background(), "demo-test-ingress-connector")
	if err != nil {
		f.t.Fatal(err)
	}
	return svc
}

func TestRemoveConnectorRefusesAnotherProject(t *testing.T) {
	f := newIngressFixture(t)
	seedDeployed(t, f.store, "api", apiBinding())
	f.applyIngress("apply", ingressManifest(t, ""))
	svc := f.connectorService()
	var spec map[string]any
	if err := json.Unmarshal(svc.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	spec["Labels"].(map[string]any)["berthy.project"] = "other"
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.docker.UpdateService(context.Background(), svc.ID, 0, raw, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.ctrl.removeConnector(context.Background()); err == nil || f.connectorMissing() {
		t.Fatalf("foreign connector was removed: %v", err)
	}
}

// connectorEnv returns the environment the connector service runs with, as Docker reports it.
func (f *ingressFixture) connectorEnv() []string {
	f.t.Helper()
	var spec struct {
		TaskTemplate struct {
			ContainerSpec struct{ Env []string }
		}
	}
	if err := json.Unmarshal(f.connectorService().Spec, &spec); err != nil {
		f.t.Fatal(err)
	}
	return spec.TaskTemplate.ContainerSpec.Env
}

// connectorReleases returns how many releases the connector app has.
func (f *ingressFixture) connectorReleases() int {
	f.t.Helper()
	history, err := f.store.History(context.Background(), manifest.ConnectorAppName)
	if err != nil {
		f.t.Fatal(err)
	}
	return len(history)
}

// outageEngine fails the connector's service writes while down is set, as Docker would when the connector cannot be
// started or updated. Everything else reaches the wrapped engine.
type outageEngine struct {
	Engine
	down *atomic.Bool
}

func (e outageEngine) CreateService(ctx context.Context, spec json.RawMessage, auth *engine.RegistryAuth) (string, error) {
	if e.down.Load() && specName(spec) == "demo-test-ingress-connector" {
		return "", &engine.StatusError{Code: 500}
	}
	return e.Engine.CreateService(ctx, spec, auth)
}

func (e outageEngine) UpdateService(ctx context.Context, id string, version uint64, spec json.RawMessage, auth *engine.RegistryAuth) error {
	if e.down.Load() && specName(spec) == "demo-test-ingress-connector" {
		return &engine.StatusError{Code: 500}
	}
	return e.Engine.UpdateService(ctx, id, version, spec, auth)
}

// connectorOutage makes the fixture's connector writes fail while the returned switch is set. Call it before the
// fixture runs anything.
func connectorOutage(f *ingressFixture) *atomic.Bool {
	down := &atomic.Bool{}
	f.ctrl.Engine = outageEngine{Engine: f.ctrl.Engine, down: down}
	return down
}

// operationSteps returns the data of each step event an operation recorded, in order.
func operationSteps(t *testing.T, s *state.Store, opID string) []string {
	t.Helper()
	events, err := s.EventsAfter(context.Background(), opID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range events {
		if ev.Kind == "step" {
			out = append(out, string(ev.Data))
		}
	}
	return out
}

func TestRepeatIngressApplyLeavesTheConnectorRunning(t *testing.T) {
	f := newIngressFixture(t)
	seedDeployed(t, f.store, "api", apiBinding())
	m := ingressManifest(t, "")
	f.applyIngress("apply-1", m)
	tunnelID := f.tunnel().ID
	service, releases := f.connectorService().ID, f.connectorReleases()

	f.calls.reset()
	repeat := f.run(Request{RequestID: "apply-2", Kind: "ingress", Commit: "c2", Manifest: m})
	if repeat.State != "succeeded" {
		t.Fatalf("repeat apply %s: %s", repeat.State, repeat.Err)
	}
	if calls := f.calls.all(); len(calls) != 0 {
		t.Fatalf("a repeat apply with nothing to change wrote: %v", calls)
	}
	if got := f.connectorService().ID; got != service {
		t.Fatalf("connector service replaced: %s -> %s", service, got)
	}
	if n := f.connectorReleases(); n != releases {
		t.Fatalf("repeat apply recorded a connector release: %d -> %d", releases, n)
	}
	if steps := operationSteps(t, f.store, repeat.ID); !reflect.DeepEqual(steps, []string{`{"step":"connector unchanged"}`}) {
		t.Fatalf("steps %v", steps)
	}

	// Routes are still synced on a repeat apply: a rule that Cloudflare lost is written back, and the connector is untouched.
	f.cf.SetConfig(tunnelID, json.RawMessage(`{"ingress":[{"service":"http_status:404"}]}`))
	f.calls.reset()
	f.applyIngress("apply-3", m)
	if got := describeRules(f.tunnelConfig(tunnelID)); !reflect.DeepEqual(got, []string{"api.example.com||" + apiTarget, "||http_status:404"}) {
		t.Fatalf("rules after the repeat apply %v", got)
	}
	if n := f.calls.count("service "); n != 0 {
		t.Fatalf("connector touched by a route sync: %v", f.calls.all())
	}
}

func TestChangedConnectorImageRollsTheConnector(t *testing.T) {
	f := newIngressFixture(t)
	ctx := context.Background()
	seedDeployed(t, f.store, "api", apiBinding())
	f.applyIngress("apply-1", ingressManifest(t, ""))
	releases := f.connectorReleases()

	f.calls.reset()
	digest := strings.Repeat("f", 64)
	f.applyIngress("apply-2", ingressManifest(t, "")) // nothing changed, so the connector must stay as it is
	if n := f.calls.count("service "); n != 0 {
		t.Fatalf("baseline apply touched the connector: %v", f.calls.all())
	}
	f.calls.reset()
	f.applyIngress("apply-3", ingressManifestImage(t, "cloudflare/cloudflared@sha256:"+digest, ""))
	if n := f.calls.count("service update demo-test-ingress-connector"); n != 1 {
		t.Fatalf("connector updates %d, want 1: %v", n, f.calls.all())
	}
	if n := f.connectorReleases(); n != releases+1 {
		t.Fatalf("connector releases %d, want %d", n, releases+1)
	}
	if rel, err := f.store.Applied(ctx, manifest.ConnectorAppName); err != nil || !strings.HasSuffix(rel.Digest, digest) {
		t.Fatalf("applied connector %+v: %v", rel, err)
	}
	if env := f.connectorEnv(); !slices.Contains(env, "TUNNEL_TOKEN="+f.cf.TunnelToken(f.tunnel().ID)) {
		t.Fatal("rolled connector lost the tunnel token")
	}
}

func TestChangedTunnelTokenRollsTheConnector(t *testing.T) {
	f := newIngressFixture(t)
	seedDeployed(t, f.store, "api", apiBinding())
	m := ingressManifest(t, "")
	f.applyIngress("apply-1", m)
	f.cf.DeleteTunnel(f.tunnel().ID)
	// A tunnel created under the same name has a new token. The apply must store it and roll the connector onto it.
	fresh := f.cf.AddTunnel(ingressTunnel, true)

	f.calls.reset()
	f.applyIngress("apply-2", m)
	if n := f.calls.count("service update demo-test-ingress-connector"); n != 1 {
		t.Fatalf("connector updates %d, want 1: %v", n, f.calls.all())
	}
	if env := f.connectorEnv(); !slices.Contains(env, "TUNNEL_TOKEN="+f.cf.TunnelToken(fresh)) {
		t.Fatal("connector does not run with the new tunnel token")
	}
	if got := describeRules(f.tunnelConfig(fresh)); !reflect.DeepEqual(got, []string{"api.example.com||" + apiTarget, "||http_status:404"}) {
		t.Fatalf("rules on the new tunnel %v", got)
	}
}

// A rollout that fails after the new token is stored leaves the old connector running. The next apply sees the stored
// token as unchanged, so only the connector's own environment shows that it must roll.
func TestConnectorStillOnAnOldTokenIsRolledOnTheNextApply(t *testing.T) {
	f := newIngressFixture(t)
	seedDeployed(t, f.store, "api", apiBinding())
	down := connectorOutage(f)
	m := ingressManifest(t, "")
	f.applyIngress("apply-1", m)
	f.cf.DeleteTunnel(f.tunnel().ID)
	fresh := f.cf.AddTunnel(ingressTunnel, true)

	down.Store(true)
	if op := f.run(Request{RequestID: "apply-2", Kind: "ingress", Manifest: m}); op.State == "succeeded" {
		t.Fatal("apply succeeded while the connector could not be updated")
	}
	down.Store(false)
	if env := f.connectorEnv(); slices.Contains(env, "TUNNEL_TOKEN="+f.cf.TunnelToken(fresh)) {
		t.Fatal("the failed rollout already runs the new token")
	}

	f.calls.reset()
	f.applyIngress("apply-3", m)
	if n := f.calls.count("service update demo-test-ingress-connector"); n != 1 {
		t.Fatalf("connector updates %d, want 1: %v", n, f.calls.all())
	}
	if env := f.connectorEnv(); !slices.Contains(env, "TUNNEL_TOKEN="+f.cf.TunnelToken(fresh)) {
		t.Fatal("connector still runs an old token")
	}
}

func TestConnectorServiceDeletedOutsideBerthyIsRecreated(t *testing.T) {
	f := newIngressFixture(t)
	ctx := context.Background()
	seedDeployed(t, f.store, "api", apiBinding())
	m := ingressManifest(t, "")
	f.applyIngress("apply-1", m)
	gone := f.connectorService()
	if err := f.docker.DeleteService(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}

	f.calls.reset()
	f.applyIngress("apply-2", m)
	if n := f.calls.count("service create demo-test-ingress-connector"); n != 1 {
		t.Fatalf("connector creates %d, want 1: %v", n, f.calls.all())
	}
	recreated := f.connectorService()
	if recreated.ID == gone.ID {
		t.Fatal("connector service was not recreated")
	}
	bindings, err := f.store.Bindings(ctx, manifest.ConnectorAppName)
	if err != nil || len(bindings) != 1 || bindings[0].ServiceID != recreated.ID {
		t.Fatalf("bindings %+v: %v", bindings, err)
	}
}

func TestStoreTunnelTokenReportsOnlyChanges(t *testing.T) {
	f := newIngressFixture(t)
	ctx := context.Background()
	op := openIngressPlan(t, f.store)
	for _, tc := range []struct {
		token   string
		changed bool
	}{{"first", true}, {"first", false}, {"second", true}, {"second", false}} {
		changed, err := f.ctrl.storeTunnelToken(ctx, op.ID, tc.token)
		if err != nil || changed != tc.changed {
			t.Fatalf("store %q: changed %v, want %v: %v", tc.token, changed, tc.changed, err)
		}
	}
	if _, value, err := f.store.Secret(ctx, manifest.TunnelTokenSecret); err != nil || string(value) != "second" {
		t.Fatalf("stored token: %v", err)
	}
}

// scaleService sets the replica count of a fake service, as an operator scaling it outside Berthy would.
func (f *boundFake) scaleService(t *testing.T, name string, replicas int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.services {
		if specName(f.services[i].Spec) != name {
			continue
		}
		var spec map[string]any
		if err := json.Unmarshal(f.services[i].Spec, &spec); err != nil {
			t.Fatal(err)
		}
		spec["Mode"] = map[string]any{"Replicated": map[string]any{"Replicas": replicas}}
		raw, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		f.services[i].Spec = raw
		return
	}
	t.Fatalf("no service %s", name)
}

func TestConnectorScaledOutsideBerthyIsRolledBack(t *testing.T) {
	f := newIngressFixture(t)
	seedDeployed(t, f.store, "api", apiBinding())
	m := ingressManifest(t, "")
	f.applyIngress("apply-1", m)
	f.bound.scaleService(t, "demo-test-ingress-connector", 2)

	f.calls.reset()
	f.applyIngress("apply-2", m)
	if n := f.calls.count("service update demo-test-ingress-connector"); n != 1 {
		t.Fatalf("connector updates %d, want 1: %v", n, f.calls.all())
	}
	var spec struct {
		Mode struct{ Replicated *struct{ Replicas int } }
	}
	if err := json.Unmarshal(f.connectorService().Spec, &spec); err != nil || spec.Mode.Replicated == nil || spec.Mode.Replicated.Replicas != 1 {
		t.Fatalf("connector replicas after the apply %+v: %v", spec, err)
	}
}

func TestDryRunReportsAnUnchangedConnector(t *testing.T) {
	f := newIngressFixture(t)
	ctx := context.Background()
	seedDeployed(t, f.store, "api", apiBinding())
	m := ingressManifest(t, "")
	admin := Actor{ID: "admin", Admin: true}
	dryRun := func() IngressPlan {
		t.Helper()
		p, err := f.ctrl.Plan(ctx, admin, Request{Kind: "ingress", Manifest: m})
		if err != nil || p.Ingress == nil {
			t.Fatalf("dry run: %v", err)
		}
		return *p.Ingress
	}
	if dryRun().ConnectorUnchanged {
		t.Fatal("no connector yet, but the dry run reports it unchanged")
	}

	f.applyIngress("apply-1", m)
	f.calls.reset()
	puts, writes := f.cf.ConfigPuts, f.cf.RecordWrites
	plan := dryRun()
	if !plan.ConnectorUnchanged {
		t.Fatalf("plan %+v", plan)
	}
	raw, err := json.Marshal(plan)
	if err != nil || !strings.Contains(string(raw), `"connector_unchanged":true`) {
		t.Fatalf("plan JSON %s: %v", raw, err)
	}
	if calls := f.calls.all(); len(calls) != 0 || f.cf.ConfigPuts != puts || f.cf.RecordWrites != writes {
		t.Fatalf("dry run wrote: %v", calls)
	}

	// A token that differs from the stored one is a change. The dry run reports it and prints neither token.
	f.cf.DeleteTunnel(f.tunnel().ID)
	f.cf.AddTunnel(ingressTunnel, true)
	if dryRun().ConnectorUnchanged {
		t.Fatal("a new tunnel token reported as unchanged")
	}
	if calls := f.calls.all(); len(calls) != 0 {
		t.Fatalf("dry run wrote: %v", calls)
	}
}

func TestIngressConflictStopsBeforeAnyChange(t *testing.T) {
	f := newIngressFixture(t)
	seedDeployed(t, f.store, "api", apiBinding())
	f.cf.AddRecord(f.zone, cloudflare.DNSRecord{Type: "A", Name: "api.example.com", Content: "203.0.113.9"})
	op := f.run(Request{RequestID: "conflict", Kind: "ingress", Manifest: ingressManifest(t, "")})
	if op.State != "failed" || op.Err != "ingress hostname conflict" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	if f.tunnel() != nil || !f.connectorMissing() {
		t.Fatal("conflict changed Cloudflare or Docker")
	}
	if routes, err := f.store.IngressRoutes(context.Background(), ""); err != nil || len(routes) != 0 {
		t.Fatalf("routes %+v %v", routes, err)
	}
	if _, err := f.store.Ingress(context.Background()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ingress recorded: %v", err)
	}
	events, err := f.store.EventsAfter(context.Background(), op.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Kind == "conflict" && strings.Contains(string(ev.Data), "DNS name api.example.com is used by another record") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no conflict event in %+v", events)
	}
}

func TestIngressApplyKeepsForeignRulesOfAnExistingTunnel(t *testing.T) {
	f := newIngressFixture(t)
	seedDeployed(t, f.store, "api", apiBinding())
	id := f.cf.AddTunnel(ingressTunnel, true)
	f.cf.SetConfig(id, json.RawMessage(`{"ingress":[{"hostname":"other.example.com","service":"http://other:80","originRequest":{"connectTimeout":"5s"}},{"service":"http_status:503"}],"warp-routing":{"enabled":false}}`))
	op := f.run(Request{RequestID: "adopt-tunnel", Kind: "ingress", Manifest: ingressManifest(t, "")})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	cfg := f.tunnelConfig(id)
	want := []string{"api.example.com||" + apiTarget, "other.example.com||http://other:80", "||http_status:503"}
	if got := describeRules(cfg); !reflect.DeepEqual(got, want) {
		t.Fatalf("rules %v, want %v", got, want)
	}
	if string(cfg.Ingress[1].Extra["originRequest"]) != `{"connectTimeout":"5s"}` {
		t.Fatalf("foreign origin request lost: %+v", cfg.Ingress[1].Extra)
	}
	if _, ok := cfg.Extra["warp-routing"]; !ok {
		t.Fatal("unrelated tunnel configuration lost")
	}
	if f.cf.ConfigPuts != 1 {
		t.Fatalf("config puts %d", f.cf.ConfigPuts)
	}
}

func TestIngressRefusesANonRemoteTunnel(t *testing.T) {
	f := newIngressFixture(t)
	seedDeployed(t, f.store, "api", apiBinding())
	f.cf.AddTunnel(ingressTunnel, false)
	op := f.run(Request{RequestID: "local", Kind: "ingress", Manifest: ingressManifest(t, "")})
	if op.State != "failed" || op.Err != "tunnel is not remotely managed" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	if f.cf.ConfigPuts != 0 || f.cf.RecordWrites != 0 || !f.connectorMissing() {
		t.Fatal("a local tunnel was changed")
	}
}

func TestIngressDryRunPlansWithoutWriting(t *testing.T) {
	f := newIngressFixture(t)
	seedDeployed(t, f.store, "api", apiBinding())
	p, err := f.ctrl.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{Kind: "ingress", Manifest: ingressManifest(t, "")})
	if err != nil {
		t.Fatal(err)
	}
	if p.Ingress == nil || !p.Ingress.CreateTunnel || p.Ingress.Connector == nil || !p.Ingress.DNS {
		t.Fatalf("plan %+v", p.Ingress)
	}
	if len(p.Ingress.Changes) != 1 || p.Ingress.Changes[0].Route != "add" || p.Ingress.Changes[0].DNS != "add" || p.Ingress.Changes[0].Target != apiTarget {
		t.Fatalf("changes %+v", p.Ingress.Changes)
	}
	if f.cf.ConfigPuts != 0 || f.cf.RecordWrites != 0 || f.tunnel() != nil || !f.connectorMissing() {
		t.Fatal("dry run wrote something")
	}
	if _, err = f.store.Ingress(context.Background()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("dry run recorded ingress: %v", err)
	}
}

func TestIngressRemovalRemovesOnlyBerthyState(t *testing.T) {
	f := newIngressFixture(t)
	ctx := context.Background()
	seedDeployed(t, f.store, "api", apiBinding())
	if op := f.run(Request{RequestID: "apply", Kind: "ingress", Manifest: ingressManifest(t, "")}); op.State != "succeeded" {
		t.Fatalf("apply %s: %s", op.State, op.Err)
	}
	id := f.tunnel().ID
	cfg := f.tunnelConfig(id)
	cfg.Ingress = []cloudflare.Rule{cfg.Ingress[0], rule("other.example.com", "http://other:80"), cfg.Ingress[1]}
	if err := f.api.PutConfig(ctx, ingressAccount, id, cfg); err != nil {
		t.Fatal(err)
	}

	op := f.run(Request{RequestID: "remove", Kind: "ingress", Manifest: plainManifest(t)})
	if op.State != "succeeded" {
		t.Fatalf("removal %s: %s", op.State, op.Err)
	}
	if got := describeRules(f.tunnelConfig(id)); !reflect.DeepEqual(got, []string{"other.example.com||http://other:80", "||http_status:404"}) {
		t.Fatalf("rules after removal %v", got)
	}
	for _, r := range f.cf.Records(f.zone) {
		if cloudflare.IsManaged(r, ingressID) {
			t.Fatalf("managed record left behind: %+v", r)
		}
	}
	if !f.connectorMissing() {
		t.Fatal("connector still runs")
	}
	if bindings, err := f.store.Bindings(ctx, "ingress"); err != nil || len(bindings) != 0 {
		t.Fatalf("connector bindings %+v %v", bindings, err)
	}
	if _, err := f.store.Applied(ctx, "ingress"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("connector still applied: %v", err)
	}
	if history, err := f.store.History(ctx, "ingress"); err != nil || len(history) == 0 {
		t.Fatalf("connector history dropped: %d %v", len(history), err)
	}
	if _, err := f.store.Ingress(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ingress state kept: %v", err)
	}
	if routes, err := f.store.IngressRoutes(ctx, ""); err != nil || len(routes) != 0 {
		t.Fatalf("routes kept %+v %v", routes, err)
	}
	if f.tunnel() == nil {
		t.Fatal("removal deleted the tunnel")
	}
}

func TestIngressRemovalLeavesForeignRecordsAlone(t *testing.T) {
	f := newIngressFixture(t)
	ctx := context.Background()
	seedDeployed(t, f.store, "api", apiBinding())
	if op := f.run(Request{RequestID: "apply", Kind: "ingress", Manifest: ingressManifest(t, "")}); op.State != "succeeded" {
		t.Fatalf("apply %s: %s", op.State, op.Err)
	}
	ours := f.cf.Records(f.zone)
	if len(ours) != 1 {
		t.Fatalf("records %+v", ours)
	}
	if err := f.api.DeleteDNSRecord(ctx, f.zone, ours[0].ID); err != nil {
		t.Fatal(err)
	}
	foreign := f.cf.AddRecord(f.zone, cloudflare.DNSRecord{Type: "A", Name: "api.example.com", Content: "203.0.113.9"})

	if op := f.run(Request{RequestID: "remove", Kind: "ingress", Manifest: plainManifest(t)}); op.State != "succeeded" {
		t.Fatalf("removal %s: %s", op.State, op.Err)
	}
	records := f.cf.Records(f.zone)
	if len(records) != 1 || records[0].ID != foreign || records[0].Type != "A" {
		t.Fatalf("foreign record changed: %+v", records)
	}
}

func TestReplaceExistingNeedsAdministrator(t *testing.T) {
	c := Controller{}
	req := Request{RequestID: "replace", Kind: "ingress", ReplaceExisting: true, Manifest: ingressManifest(t, "")}
	if _, err := c.Plan(context.Background(), Actor{ID: "deploy"}, req); !errors.Is(err, errReplaceAdmin) {
		t.Fatalf("plan: %v", err)
	}
	if _, _, err := admit(t, &c, context.Background(), Actor{ID: "deploy"}, req); !errors.Is(err, errReplaceAdmin) {
		t.Fatalf("admit: %v", err)
	}
	deploy := Request{RequestID: "deploy", App: "api", Version: "v1", ReplaceExisting: true, Manifest: ingressManifest(t, "")}
	if _, err := c.Plan(context.Background(), Actor{ID: "deploy"}, deploy); !errors.Is(err, errReplaceAdmin) {
		t.Fatalf("deploy plan: %v", err)
	}
}

func TestIngressStatusAndTunnelHealth(t *testing.T) {
	f := newIngressFixture(t)
	ctx := context.Background()
	empty, err := f.ctrl.IngressStatus(ctx)
	if err != nil || empty.Configured || empty.Routes == nil || len(empty.Routes) != 0 {
		t.Fatalf("empty status %+v %v", empty, err)
	}
	if configured, _ := f.ctrl.TunnelHealth(ctx); configured {
		t.Fatal("health reports ingress that is not configured")
	}

	seedDeployed(t, f.store, "api", apiBinding())
	if op := f.run(Request{RequestID: "apply", Kind: "ingress", Manifest: ingressManifest(t, "")}); op.State != "succeeded" {
		t.Fatalf("apply %s: %s", op.State, op.Err)
	}
	id := f.tunnel().ID
	f.cf.SetTunnelStatus(id, "healthy", 1)
	status, err := f.ctrl.IngressStatus(ctx)
	if err != nil || !status.Configured || status.Cloudflare != "available" || status.Tunnel == nil || status.Tunnel.Status != "healthy" || status.Tunnel.Connections != 1 {
		t.Fatalf("status %+v %v", status, err)
	}
	if len(status.Connector) != 1 || status.Connector[0].Service != "demo-test-ingress-connector" || status.Connector[0].Running != 1 || status.Connector[0].Desired != 1 {
		t.Fatalf("connector %+v", status.Connector)
	}
	if len(status.Routes) != 1 || status.Routes[0].Target != apiTarget || status.Routes[0].DNS != "managed" {
		t.Fatalf("routes %+v", status.Routes)
	}
	configured, health := f.ctrl.TunnelHealth(ctx)
	if !configured || health != "healthy" {
		t.Fatalf("health %v %q", configured, health)
	}

	f.cf.SetTunnelStatus(id, "degraded", 0)
	if _, health = f.ctrl.TunnelHealth(ctx); health != "degraded" {
		t.Fatalf("degraded health %q", health)
	}
	if err = f.store.DeleteSecret(ctx, state.Actor{ID: "admin"}, ingressSecret); err != nil {
		t.Fatal(err)
	}
	if _, health = f.ctrl.TunnelHealth(ctx); health != "unavailable" {
		t.Fatalf("health without the token %q", health)
	}
	unavailable, err := f.ctrl.IngressStatus(ctx)
	if err != nil || unavailable.Cloudflare != "unavailable" || len(unavailable.Routes) != 1 || unavailable.Routes[0].DNS != "" {
		t.Fatalf("status without the token %+v %v", unavailable, err)
	}
}

func TestIngressStatusReportsAConnectorWithoutARelease(t *testing.T) {
	f := newIngressFixture(t)
	ctx := context.Background()
	down := failedConnectorApply(t, f)
	status, err := f.ctrl.IngressStatus(ctx)
	if err != nil || !status.Configured || len(status.Connector) != 0 || !reflect.DeepEqual(status.Problems, []string{"connector not deployed; run ingress apply"}) {
		t.Fatalf("status %+v: %v", status, err)
	}
	raw, err := json.Marshal(status)
	if err != nil || !strings.Contains(string(raw), `"connector":[]`) {
		t.Fatalf("status JSON %s: %v", raw, err)
	}

	down.Store(false)
	f.applyIngress("apply-2", ingressManifest(t, ""))
	status, err = f.ctrl.IngressStatus(ctx)
	if err != nil || len(status.Problems) != 0 || len(status.Connector) != 1 {
		t.Fatalf("status after a successful apply %+v: %v", status, err)
	}
}

func TestRemoveConnectorReleasesClaimAfterConfirmedAbsence(t *testing.T) {
	for _, alreadyGone := range []bool{false, true} {
		t.Run(strconv.FormatBool(alreadyGone), func(t *testing.T) {
			f := newIngressFixture(t)
			seedDeployed(t, f.store, "api", apiBinding())
			f.applyIngress("apply", ingressManifest(t, ""))
			ctx := context.Background()
			claim := state.Claim{Kind: state.ClaimService, Name: "demo-test-ingress-connector"}
			if err := f.store.AddClaims(ctx, []state.Claim{claim}); err != nil {
				t.Fatal(err)
			}
			if alreadyGone {
				if err := f.docker.DeleteService(ctx, f.connectorService().ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.ctrl.removeConnector(ctx); err != nil {
				t.Fatal(err)
			}
			claims, err := f.store.Claims(ctx)
			if err != nil || slices.Contains(claims, claim) {
				t.Fatalf("claim retained: %v, %v", claims, err)
			}
		})
	}
}

func TestIngressRefusesPeerTunnel(t *testing.T) {
	f := newIngressFixture(t)
	id := f.cf.AddTunnel(ingressTunnel, true)
	f.ctrl.Foreign = func(context.Context) (Foreign, error) {
		return Foreign{Ingress: []state.IngressState{{AccountID: ingressAccount, TunnelID: id}}}, nil
	}
	m := ingressManifest(t, "")
	_, err := f.ctrl.ingressTargetFor(context.Background(), m)
	if err == nil || err.Error() != "tunnel is managed by another hosted environment" {
		t.Fatalf("peer tunnel: %v", err)
	}
}
