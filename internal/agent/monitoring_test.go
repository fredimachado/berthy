package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/manifest"
	"berthy/internal/metrics"
	"berthy/internal/monitor"
	"berthy/internal/state"
)

// monitoringToken returns a bearer token of the test environment with the given fill byte.
func monitoringToken(fill string) string { return "bty.lab." + strings.Repeat(fill, 64) }

// monitoringConfig is the test environment: read, deploy and admin tokens, and a Compose command that cannot run, so
// the doctor's Compose check fails quickly.
func monitoringConfig(telegramURL string) Config {
	return Config{
		Environment:    "lab",
		Tokens:         []Token{{Digest: Digest(monitoringToken("a")), Scope: "read"}, {Digest: Digest(monitoringToken("b")), Scope: "deploy"}},
		TelegramAPIURL: telegramURL,
		ComposeCommand: []string{"/nonexistent/berthy-test-compose"},
	}
}

// recordRelease stores a successful deploy of every app the manifest names, as the agent does after deploys of them.
func recordRelease(t *testing.T, s *state.Store, id, manifestJSON string) {
	t.Helper()
	recordBoundRelease(t, s, id, manifestJSON, nil)
}

// recordAppRelease stores a successful deploy of one app whose manifest is manifestJSON.
func recordAppRelease(t *testing.T, s *state.Store, id, app, manifestJSON string) {
	t.Helper()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: id + "-" + app, ActorID: "ci", Kind: "deploy", App: app, PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: app, Manifest: []byte(manifestJSON)}, Settings: releaseSettings(t, manifestJSON)}); err != nil {
		t.Fatal(err)
	}
}

// monitoredManifest is a valid manifest with one image app, and the given monitoring members.
func monitoredManifest(members string) string {
	return `{"project":"demo","network":"demo","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}}` + members + `}`
}

// closedLoopbackAddress returns a loopback host:port with nothing listening on it.
func closedLoopbackAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func serveRequest(t *testing.T, h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHealthzIsUnauthenticatedAndSaysNothingElse(t *testing.T) {
	cfg := monitoringConfig("")
	for name, h := range map[string]http.Handler{
		"agent handler":      Handler(cfg, nil),
		"deployment handler": NewDeploymentHandler(cfg, nil, nil, nil),
	} {
		for _, token := range []string{"", "not-a-token", monitoringToken("a")} {
			w := serveRequest(t, h, http.MethodGet, "/healthz", token)
			if w.Code != http.StatusOK || w.Body.String() != `{"ok":true}` {
				t.Errorf("%s with token %q: %d %q", name, token, w.Code, w.Body.String())
			}
		}
		if w := serveRequest(t, h, http.MethodPost, "/healthz", ""); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: POST /healthz %d", name, w.Code)
		}
	}
}

func TestChecksEndpointListsStoredStatesByName(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	since := time.UnixMilli(1_700_000_000_000).UTC()
	if err := s.PutCheck(ctx, state.CheckState{Name: "probe:site", State: "critical", Detail: "HTTP 503", Since: since}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutCheck(ctx, state.CheckState{Name: "disk:/", State: "ok", Detail: "40.0% used", Since: since}); err != nil {
		t.Fatal(err)
	}
	h := NewDeploymentHandler(monitoringConfig(""), nil, nil, &deploy.Controller{Store: s, Environment: "lab"})
	w := serveRequest(t, h, http.MethodGet, "/v1/environments/lab/checks", monitoringToken("a"))
	if w.Code != http.StatusOK {
		t.Fatalf("read token: %d %s", w.Code, w.Body.String())
	}
	var rows []struct {
		Name   string    `json:"name"`
		State  string    `json:"state"`
		Detail string    `json:"detail"`
		Since  time.Time `json:"since"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Name != "disk:/" || rows[1].Name != "probe:site" || rows[1].State != "critical" || rows[1].Detail != "HTTP 503" || !rows[1].Since.Equal(since) {
		t.Fatalf("rows: %+v", rows)
	}
	if w := serveRequest(t, h, http.MethodGet, "/v1/environments/lab/checks", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
	if w := serveRequest(t, h, http.MethodGet, "/v1/environments/dev/checks", monitoringToken("a")); w.Code != http.StatusUnauthorized {
		t.Fatalf("other environment: %d", w.Code)
	}
	if w := serveRequest(t, h, http.MethodPost, "/v1/environments/lab/checks", monitoringToken("b")); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST checks: %d", w.Code)
	}
	empty := NewDeploymentHandler(monitoringConfig(""), nil, nil, &deploy.Controller{Store: openIngressStore(t), Environment: "lab"})
	if w := serveRequest(t, empty, http.MethodGet, "/v1/environments/lab/checks", monitoringToken("a")); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("empty checks: %q", w.Body.String())
	}
	if w := serveRequest(t, NewDeploymentHandler(monitoringConfig(""), nil, nil, nil), http.MethodGet, "/v1/environments/lab/checks", monitoringToken("a")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("checks without state: %d", w.Code)
	}
}

func TestAlertTestNeedsDeployScopeAndReportsMissingConfiguration(t *testing.T) {
	s := openIngressStore(t)
	h := NewDeploymentHandler(monitoringConfig(""), nil, nil, &deploy.Controller{Store: s, Environment: "lab"})
	if w := serveRequest(t, h, http.MethodPost, "/v1/environments/lab/alerts/test", monitoringToken("a")); w.Code != http.StatusForbidden {
		t.Fatalf("read token sent a test alert: %d %s", w.Code, w.Body.String())
	}
	if w := serveRequest(t, h, http.MethodPost, "/v1/environments/lab/alerts/test", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
	if w := serveRequest(t, h, http.MethodGet, "/v1/environments/lab/alerts/test", monitoringToken("b")); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET alerts/test: %d", w.Code)
	}
	w := serveRequest(t, h, http.MethodPost, "/v1/environments/lab/alerts/test", monitoringToken("b"))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "alerts are not configured") {
		t.Fatalf("no manifest: %d %s", w.Code, w.Body.String())
	}
	rows, err := alertAudits(s)
	if err != nil || len(rows) != 1 || rows[0].Outcome != "failed" {
		t.Fatalf("audit: %+v %v", rows, err)
	}
}

func TestAlertTestSendsThroughTelegramAndAuditsEveryAttempt(t *testing.T) {
	const token = "123456789:TEST-bot-token-not-real"
	var mu sync.Mutex
	var sent []url.Values
	var status atomic.Int32
	status.Store(http.StatusOK)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		mu.Lock()
		sent = append(sent, r.PostForm)
		mu.Unlock()
		if code := int(status.Load()); code != http.StatusOK {
			w.WriteHeader(code)
			w.Write([]byte(`{"ok":false,"description":"Unauthorized"}`))
			return
		}
		w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer fake.Close()
	s := openIngressStore(t)
	ctx := context.Background()
	if err := s.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: "TELEGRAM_BOT_TOKEN"}, []byte(token)); err != nil {
		t.Fatal(err)
	}
	recordRelease(t, s, "release-alerts", monitoredManifest(`,"alerts":{"telegram":{"bot_token_secret":"TELEGRAM_BOT_TOKEN","chat_id":"-1001234567890"}}`))
	h := NewDeploymentHandler(monitoringConfig(fake.URL), nil, nil, &deploy.Controller{Store: s, Environment: "lab"})

	w := serveRequest(t, h, http.MethodPost, "/v1/environments/lab/alerts/test", monitoringToken("b"))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"sent":true}` {
		t.Fatalf("send: %d %s", w.Code, w.Body.String())
	}
	mu.Lock()
	if len(sent) != 1 || sent[0].Get("chat_id") != "-1001234567890" || sent[0].Get("text") != "🧪 lab test alert from berthy" {
		t.Fatalf("sent: %v", sent)
	}
	mu.Unlock()

	status.Store(http.StatusUnauthorized)
	w = serveRequest(t, h, http.MethodPost, "/v1/environments/lab/alerts/test", monitoringToken("b"))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "telegram returned HTTP 401") {
		t.Fatalf("rejected: %d %s", w.Code, w.Body.String())
	}

	// A second handler points at an address with nothing listening on it.
	down := NewDeploymentHandler(monitoringConfig("http://"+closedLoopbackAddress(t)), nil, nil, &deploy.Controller{Store: s, Environment: "lab"})
	w = serveRequest(t, down, http.MethodPost, "/v1/environments/lab/alerts/test", monitoringToken("b"))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "telegram unavailable") {
		t.Fatalf("unreachable: %d %s", w.Code, w.Body.String())
	}

	rows, err := alertAudits(s)
	if err != nil || len(rows) != 3 {
		t.Fatalf("audit rows: %+v %v", rows, err)
	}
	outcomes := []string{rows[0].Outcome, rows[1].Outcome, rows[2].Outcome}
	if strings.Join(outcomes, ",") != "failed,failed,succeeded" {
		t.Fatalf("audit outcomes: %v", outcomes)
	}
	for _, row := range rows {
		if strings.Contains(fmt.Sprint(row), token) {
			t.Fatalf("audit row holds the token: %+v", row)
		}
	}
}

func TestAlertTestWithoutSecretIsNotConfigured(t *testing.T) {
	s := openIngressStore(t)
	recordRelease(t, s, "release-no-secret", monitoredManifest(`,"alerts":{"telegram":{"bot_token_secret":"TELEGRAM_BOT_TOKEN","chat_id":"1"}}`))
	h := NewDeploymentHandler(monitoringConfig("https://api.telegram.org"), nil, nil, &deploy.Controller{Store: s, Environment: "lab"})
	w := serveRequest(t, h, http.MethodPost, "/v1/environments/lab/alerts/test", monitoringToken("b"))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "alerts are not configured") {
		t.Fatalf("secret missing: %d %s", w.Code, w.Body.String())
	}
}

func TestMonitoringChecksFollowTheLastAppliedManifest(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	recordRelease(t, s, "release-monitoring", monitoredManifest(`,
		"probes":{
			"site":{"http":"http://127.0.0.1:1/health","interval":"10s","timeout":"2s","failures":2},
			"pg":{"tcp":"127.0.0.1:5432"},
			"web":{"service":{"app":"api","port":8080}}
		},
		"peers":{"prod":"http://10.0.0.5:8080"},
		"alerts":{"telegram":{"bot_token_secret":"TELEGRAM_BOT_TOKEN","chat_id":"1"}}`))
	w := NewMonitoring(monitoringConfig(""), nil, nil, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {})
	checks := map[string]monitor.Check{}
	for _, c := range w.checks(ctx) {
		checks[c.Name()] = c
	}
	for _, name := range []string{"probe:site", "probe:pg", "probe:web", "peer:prod", "telegram", "memory", "docker_disk"} {
		if checks[name] == nil {
			t.Errorf("missing check %s; have %d checks", name, len(checks))
		}
	}
	if c := checks["probe:site"]; c != nil && (c.Interval() != 10*time.Second || c.Failures() != 2) {
		t.Errorf("probe settings: %v %d", c.Interval(), c.Failures())
	}
	if c := checks["probe:web"]; c != nil {
		// Without Docker the helper cannot run, and off Linux it never can; the reason is the probe's detail.
		want := "probe helper unavailable: Docker unavailable"
		if runtime.GOOS != "linux" {
			want = "service probes need a Linux agent"
		}
		if got := c.Run(ctx); got.State != monitor.Unknown || got.Detail != want {
			t.Errorf("service probe: %+v, want %q", got, want)
		}
	}
	if c := checks["telegram"]; c != nil {
		if got := c.Run(ctx); got.State != monitor.Warning || got.Detail != "telegram bot token secret missing" {
			t.Errorf("missing secret: %+v", got)
		}
	}
	if checks["tunnel"] != nil {
		t.Error("tunnel check without managed ingress")
	}
	if err := s.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: "TELEGRAM_BOT_TOKEN"}, []byte("token-value")); err != nil {
		t.Fatal(err)
	}
	if c := (checks["telegram"]); c != nil {
		if got := c.Run(ctx); got.State != monitor.OK {
			t.Errorf("stored secret: %+v", got)
		}
	}
}

func TestMonitoringWaitsForTheFirstHostSample(t *testing.T) {
	s := openIngressStore(t)
	col := metrics.New(metrics.NewHostReader(), nilDocker{})
	w := NewMonitoring(monitoringConfig(""), nil, col, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {})
	if got := w.checks(context.Background()); got != nil {
		t.Fatalf("checks named before the first sample: %d", len(got))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go col.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := col.Latest(); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no sample")
		}
		time.Sleep(10 * time.Millisecond)
	}
	found := false
	for _, c := range w.checks(context.Background()) {
		found = found || c.Name() == "memory"
	}
	if !found {
		t.Fatal("memory check missing after the first sample")
	}
}

func TestMonitoringWithoutStateIsNil(t *testing.T) {
	if NewMonitoring(monitoringConfig(""), nil, nil, nil, func(string) {}) != nil {
		t.Fatal("monitoring without state")
	}
	if NewMonitoring(monitoringConfig(""), nil, nil, &deploy.Controller{}, func(string) {}) != nil {
		t.Fatal("monitoring with a controller that has no store")
	}
	var none *Monitoring
	none.Run(context.Background()) // a nil monitoring does nothing
}

func TestManagedIngressAddsTheTunnelCheck(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "ingress-check", ActorID: "ci", Kind: "ingress", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetIngress(ctx, op.ID, state.IngressState{AccountID: "acct", APITokenSecret: "CF_TOKEN", TunnelID: "t", TunnelName: "demo-lab"}); err != nil {
		t.Fatal(err)
	}
	w := NewMonitoring(monitoringConfig(""), nil, nil, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {})
	for _, c := range w.checks(ctx) {
		if c.Name() == "tunnel" {
			if c.Interval() != 2*time.Minute || c.Failures() != 2 {
				t.Fatalf("tunnel settings: %v %d", c.Interval(), c.Failures())
			}
			return
		}
	}
	t.Fatal("no tunnel check with managed ingress")
}

func TestDoctorReportsPeersAndTheAlertSecret(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer healthy.Close()
	s := openIngressStore(t)
	ctx := context.Background()
	recordRelease(t, s, "release-doctor", monitoredManifest(`,
		"peers":{"up":"`+healthy.URL+`","down":"http://`+closedLoopbackAddress(t)+`"},
		"alerts":{"telegram":{"bot_token_secret":"TELEGRAM_BOT_TOKEN","chat_id":"1"}}`))
	// The monitor has a stored state for one peer, which the doctor uses without asking again.
	if err := s.PutCheck(ctx, state.CheckState{Name: "peer:up", State: "ok", Detail: "reachable", Since: time.Now()}); err != nil {
		t.Fatal(err)
	}
	h := NewDeploymentHandler(monitoringConfig(""), nil, nil, &deploy.Controller{Store: s, Environment: "lab"})
	w := serveRequest(t, h, http.MethodGet, "/v1/environments/lab/doctor", monitoringToken("a"))
	if w.Code != http.StatusOK {
		t.Fatalf("doctor: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Checks []api.Check `json:"checks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	found := map[string]api.Check{}
	for _, c := range body.Checks {
		found[c.Name] = c
	}
	if c := found["peer:up"]; !c.OK || c.Critical || c.Detail != "reachable" {
		t.Errorf("stored peer state: %+v", c)
	}
	if c := found["peer:down"]; c.OK || c.Critical || c.Detail != "silent: connection refused" {
		t.Errorf("live peer probe: %+v", c)
	}
	if c := found["telegram"]; c.OK || c.Critical || c.Detail != "telegram bot token secret missing" {
		t.Errorf("alert secret: %+v", c)
	}
	if strings.Contains(w.Body.String(), healthy.URL) {
		t.Error("doctor echoed a peer address")
	}
}

func TestTelegramAPIURLFollowsTheCloudflareRule(t *testing.T) {
	for raw, valid := range map[string]bool{
		"https://api.telegram.org":             true,
		"http://127.0.0.1:18080":               true,
		"http://localhost:18080":               true,
		"http://api.telegram.org":              false,
		"https://user:secret@api.telegram.org": false,
		"https://api.telegram.org/?x=1":        false,
		"ftp://api.telegram.org":               false,
	} {
		c := monitoringConfig(raw)
		c.Listen = "127.0.0.1:9999"
		c.Environment = "lab"
		err := c.Validate()
		if (err == nil) != valid {
			t.Errorf("%s: valid=%v err=%v", raw, valid, err)
		}
		if err != nil && !strings.Contains(err.Error(), "telegram_api_url") {
			t.Errorf("%s: error does not name the setting: %v", raw, err)
		}
	}
}

// alertAudits returns the audit rows of test alerts, newest first.
func alertAudits(s *state.Store) ([]state.Audit, error) {
	rows, err := s.AuditLog(context.Background(), 100)
	if err != nil {
		return nil, err
	}
	var out []state.Audit
	for _, row := range rows {
		if row.Action == "alerts_test" {
			out = append(out, row)
		}
	}
	return out, nil
}

func TestRestartChecksCoverOnlyTheAppsBerthyManages(t *testing.T) {
	role := func(name string) []api.Role { return []api.Role{{Name: name}} }
	inventory := []api.App{
		{Ref: api.AppRef{Kind: "swarm", Name: "p2test-api-web"}, Roles: role("p2test-api-web")},
		{Ref: api.AppRef{Kind: "compose", Name: "old-stack"}, Roles: role("web")},
		{Ref: api.AppRef{Kind: "compose", Name: "demo-db"}, Roles: role("db")},
		{Ref: api.AppRef{Kind: "compose", Name: "p3alegacy"}, Roles: role("store")},
		{Ref: api.AppRef{Kind: "image", Name: "ingress"}, Roles: role("connector")},
		{Ref: api.AppRef{Kind: "image", Name: "api"}, Roles: role("web")},
	}
	m := &manifest.Manifest{Project: "demo", Apps: map[string]manifest.App{
		"db":     {Kind: "compose"},
		"legacy": {Kind: "compose", ProjectName: "p3alegacy"},
		"api":    {Kind: "image"},
	}}
	names := func(apps []api.App) string {
		var out []string
		for _, app := range apps {
			out = append(out, app.Ref.Kind+":"+app.Ref.Name)
		}
		return strings.Join(out, ",")
	}
	if got, want := names(managedRestartApps(inventory, m)), "compose:demo-db,compose:p3alegacy,image:ingress,image:api"; got != want {
		t.Fatalf("managed apps %s, want %s", got, want)
	}
	// Without a manifest no Compose project is named, so none is managed; image apps still are.
	if got, want := names(managedRestartApps(inventory, nil)), "image:ingress,image:api"; got != want {
		t.Fatalf("managed apps without a manifest %s, want %s", got, want)
	}
}
