package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/api"
	"berthy/internal/cloudflare/cftest"
	"berthy/internal/deploy"
	"berthy/internal/dialtest"
	"berthy/internal/monitor"
	"berthy/internal/state"
)

// credentialRegistry is a registry that lets the user "puller" pull with one password, which a test can change.
type credentialRegistry struct {
	*httptest.Server
	mu       sync.Mutex
	password string
}

func newCredentialRegistry(t *testing.T, password string) *credentialRegistry {
	t.Helper()
	r := &credentialRegistry{password: password}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		want := r.password
		r.mu.Unlock()
		user, pass, ok := req.BasicAuth()
		if !ok || user != "puller" || pass != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"name":"x","tags":["v1"]}`))
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *credentialRegistry) host() string { return strings.TrimPrefix(r.URL, "http://") }

func (r *credentialRegistry) accept(password string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.password = password
}

// registryManifest is a manifest whose one image app pulls from host with the secret REGISTRY_TOKEN.
func registryManifest(host string) string {
	return `{"project":"demo","network":"demo","registries":{"` + host + `":{"username":"puller","password_secret":"REGISTRY_TOKEN"}},"apps":{"api":{"kind":"image","image":"` + host + `/acme/api","roles":{"web":{}}}}}`
}

func putRegistryPassword(t *testing.T, s *state.Store, host, value string) {
	t.Helper()
	if err := s.PutSecret(context.Background(), state.Actor{ID: "admin"}, state.SecretInfo{Name: "REGISTRY_TOKEN", Registry: host}, []byte(value)); err != nil {
		t.Fatal(err)
	}
}

func putPlainSecret(t *testing.T, s *state.Store, name, value string) {
	t.Helper()
	if err := s.PutSecret(context.Background(), state.Actor{ID: "admin"}, state.SecretInfo{Name: name}, []byte(value)); err != nil {
		t.Fatal(err)
	}
}

func setIngress(t *testing.T, s *state.Store) {
	t.Helper()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "ingress-credential", ActorID: "ci", Kind: "ingress", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetIngress(ctx, op.ID, state.IngressState{AccountID: "acct", APITokenSecret: "CF_API_TOKEN", TunnelID: "t", TunnelName: "demo-lab"}); err != nil {
		t.Fatal(err)
	}
	// Finishing the operation frees the environment lock, which storing a secret needs.
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
}

func credentialDoctor(t *testing.T, cfg Config, s *state.Store) (map[string]api.Check, string) {
	t.Helper()
	h := NewDeploymentHandler(cfg, nil, nil, &deploy.Controller{Store: s, Environment: "lab"})
	w := serveRequest(t, h, http.MethodGet, "/v1/projects/demo/environments/lab/doctor", monitoringToken("a"))
	checks := doctorChecks(t, h)
	found := map[string]api.Check{}
	for _, c := range checks {
		found[c.Name] = c
	}
	return found, w.Body.String()
}

func TestCredentialResultSaysWhatToDo(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	plain := credentialProbe{check: "cloudflare_token", secret: "CF_API_TOKEN"}
	registry := credentialProbe{check: "registry:ghcr.io", secret: "REGISTRY_TOKEN", registry: "ghcr.io"}
	plainCmd := "berthy secret set CF_API_TOKEN -e lab"
	registryCmd := "berthy secret set REGISTRY_TOKEN --registry ghcr.io -e lab"
	for _, tc := range []struct {
		name   string
		probe  credentialProbe
		answer credentialAnswer
		state  string
		detail string
	}{
		{"accepted, no expiry", plain, credentialAnswer{verdict: credentialAccepted}, monitor.OK, "accepted"},
		{"accepted, far expiry", plain, credentialAnswer{verdict: credentialAccepted, expires: time.Date(2027, 3, 1, 8, 0, 0, 0, time.UTC)}, monitor.OK, "accepted, expires 2027-03-01"},
		{"accepted, expiry exactly 14 days away", plain, credentialAnswer{verdict: credentialAccepted, expires: now.Add(14 * 24 * time.Hour)}, monitor.Warning, "expires in 14d (2026-12-02): replace it with " + plainCmd},
		{"accepted, expiry soon", plain, credentialAnswer{verdict: credentialAccepted, expires: time.Date(2026, 11, 23, 13, 0, 0, 0, time.UTC)}, monitor.Warning, "expires in 5d (2026-11-23): replace it with " + plainCmd},
		{"accepted, expiry soon, registry", registry, credentialAnswer{verdict: credentialAccepted, expires: time.Date(2026, 11, 23, 13, 0, 0, 0, time.UTC)}, monitor.Warning, "expires in 5d (2026-11-23): replace it with " + registryCmd},
		{"accepted, expiry past", plain, credentialAnswer{verdict: credentialAccepted, expires: now.Add(-time.Hour)}, monitor.Warning, "expires in 0s (2026-11-18): replace it with " + plainCmd},
		{"refused", plain, credentialAnswer{verdict: credentialRefused, reason: "Cloudflare reports the token as expired"}, monitor.Critical, "refused: Cloudflare reports the token as expired: run " + plainCmd},
		{"refused, registry", registry, credentialAnswer{verdict: credentialRefused, reason: "ghcr.io does not let it pull acme/api"}, monitor.Critical, "refused: ghcr.io does not let it pull acme/api: run " + registryCmd},
		{"missing", plain, credentialAnswer{verdict: credentialMissing}, monitor.Critical, "secret CF_API_TOKEN is not stored: run " + plainCmd},
		{"missing, registry", registry, credentialAnswer{verdict: credentialMissing}, monitor.Critical, "secret REGISTRY_TOKEN is not stored: run " + registryCmd},
		{"unasked", plain, credentialAnswer{verdict: credentialUnasked, reason: "timeout"}, monitor.Unknown, "could not be asked: timeout"},
		{"unasked, registry", registry, credentialAnswer{verdict: credentialUnasked, reason: "unreachable"}, monitor.Unknown, "could not be asked: unreachable"},
		{"zero answer", plain, credentialAnswer{}, monitor.Unknown, "could not be asked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := credentialResult(tc.probe, tc.answer, "lab", now)
			if got.State != tc.state || got.Detail != tc.detail {
				t.Errorf("got %q %q, want %q %q", got.State, got.Detail, tc.state, tc.detail)
			}
		})
	}
}

func TestDoctorAsksTheRegistryAboutItsPassword(t *testing.T) {
	reg := newCredentialRegistry(t, "pw-one")
	host := reg.host()
	s := openIngressStore(t)
	recordRelease(t, s, "release-registry", registryManifest(host))
	putRegistryPassword(t, s, host, "pw-one")
	cfg := monitoringConfig("")

	checks, body := credentialDoctor(t, cfg, s)
	if c := checks["registry:"+host]; !c.OK || c.Critical || c.Detail != "accepted" {
		t.Fatalf("right password: %+v", c)
	}

	reg.accept("pw-two")
	checks, body = credentialDoctor(t, cfg, s)
	c := checks["registry:"+host]
	if c.OK || c.Critical || !strings.HasPrefix(c.Detail, "refused: ") || !strings.HasSuffix(c.Detail, "run berthy secret set REGISTRY_TOKEN --registry "+host+" -e lab") {
		t.Fatalf("changed password: %+v", c)
	}
	if strings.Contains(body, "pw-one") || strings.Contains(body, "pw-two") {
		t.Error("doctor printed a registry password")
	}

	putRegistryPassword(t, s, host, "pw-two")
	checks, body = credentialDoctor(t, cfg, s)
	if c := checks["registry:"+host]; !c.OK || c.Critical || c.Detail != "accepted" {
		t.Fatalf("replaced password: %+v", c)
	}
	if strings.Contains(body, "pw-one") || strings.Contains(body, "pw-two") {
		t.Error("doctor printed a registry password")
	}
}

func TestRegistryOutageIsNotARefusal(t *testing.T) {
	cfg := monitoringConfig("")
	t.Run("registry answers 503", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
		defer srv.Close()
		host := strings.TrimPrefix(srv.URL, "http://")
		s := openIngressStore(t)
		recordRelease(t, s, "release-503", registryManifest(host))
		putRegistryPassword(t, s, host, "pw-one")
		checks, _ := credentialDoctor(t, cfg, s)
		c := checks["registry:"+host]
		if c.OK || c.Critical || !strings.HasPrefix(c.Detail, "could not be asked: ") || strings.Contains(c.Detail, "refused:") {
			t.Fatalf("503: %+v", c)
		}
	})
	t.Run("registry is down", func(t *testing.T) {
		host := dialtest.Refused
		cfg.dial = dialtest.Dial
		s := openIngressStore(t)
		recordRelease(t, s, "release-down", registryManifest(host))
		putRegistryPassword(t, s, host, "pw-one")
		checks, _ := credentialDoctor(t, cfg, s)
		c := checks["registry:"+host]
		if c.OK || c.Critical || c.Detail != "could not be asked: connection refused" {
			t.Fatalf("down: %+v", c)
		}
	})
}

func TestRegistryWithoutADeployedImageAppIsNotChecked(t *testing.T) {
	cfg := monitoringConfig("")
	s := openIngressStore(t)
	// The manifest names a registry that no image app pulls from.
	recordRelease(t, s, "release-unused", `{"project":"demo","network":"demo","registries":{"127.0.0.1:1":{"username":"puller","password_secret":"REGISTRY_TOKEN"}},"apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}}}`)
	putRegistryPassword(t, s, "127.0.0.1:1", "pw-one")
	checks, _ := credentialDoctor(t, cfg, s)
	for name := range checks {
		if strings.HasPrefix(name, "registry:") {
			t.Errorf("unused registry checked: %s", name)
		}
	}

	s = openIngressStore(t)
	recordRelease(t, s, "release-none", monitoredManifest(""))
	checks, _ = credentialDoctor(t, cfg, s)
	for name := range checks {
		if strings.HasPrefix(name, "registry:") {
			t.Errorf("registry checked without registries: %s", name)
		}
	}
}

func TestRegistryPasswordThatIsNotStoredIsReported(t *testing.T) {
	reg := newCredentialRegistry(t, "pw-one")
	host := reg.host()
	cfg := monitoringConfig("")
	want := "secret REGISTRY_TOKEN is not stored: run berthy secret set REGISTRY_TOKEN --registry " + host + " -e lab"

	s := openIngressStore(t)
	recordRelease(t, s, "release-missing", registryManifest(host))
	checks, _ := credentialDoctor(t, cfg, s)
	if c := checks["registry:"+host]; c.OK || c.Critical || c.Detail != want {
		t.Fatalf("not stored: %+v", c)
	}

	putPlainSecret(t, s, "REGISTRY_TOKEN", "pw-one")
	checks, _ = credentialDoctor(t, cfg, s)
	if c := checks["registry:"+host]; c.OK || c.Critical || c.Detail != want {
		t.Fatalf("stored as a plain secret: %+v", c)
	}
}

// cloudflareCredentialConfig serves the fake Cloudflare and returns the agent's config that points at it.
func cloudflareCredentialConfig(t *testing.T, f *cftest.Fake) Config {
	t.Helper()
	srv := httptest.NewServer(f.Handler())
	t.Cleanup(srv.Close)
	cfg := monitoringConfig("")
	cfg.CloudflareAPIURL = srv.URL
	return cfg
}

func TestDoctorAsksCloudflareAboutItsToken(t *testing.T) {
	f := cftest.New("cf-token-1", "acct")
	cfg := cloudflareCredentialConfig(t, f)
	s := openIngressStore(t)
	setIngress(t, s)
	putPlainSecret(t, s, "CF_API_TOKEN", "cf-token-1")

	checks, body := credentialDoctor(t, cfg, s)
	if c := checks["cloudflare_token"]; !c.OK || c.Critical || c.Detail != "accepted" {
		t.Fatalf("active token: %+v", c)
	}

	far := time.Now().Add(40 * 24 * time.Hour)
	f.SetTokenExpiry(far)
	checks, body = credentialDoctor(t, cfg, s)
	if c := checks["cloudflare_token"]; !c.OK || c.Critical || c.Detail != "accepted, expires "+far.UTC().Format("2006-01-02") {
		t.Fatalf("far expiry: %+v", c)
	}

	f.SetTokenExpiry(time.Now().Add(5 * 24 * time.Hour))
	checks, body = credentialDoctor(t, cfg, s)
	if c := checks["cloudflare_token"]; c.OK || c.Critical || !strings.HasPrefix(c.Detail, "expires in ") || !strings.Contains(c.Detail, "berthy secret set CF_API_TOKEN -e lab") {
		t.Fatalf("near expiry: %+v", c)
	}

	putPlainSecret(t, s, "CF_API_TOKEN", "cf-token-2")
	checks, body = credentialDoctor(t, cfg, s)
	if c := checks["cloudflare_token"]; c.OK || c.Critical || c.Detail != "refused: Cloudflare does not accept the token: it is wrong, expired or disabled: run berthy secret set CF_API_TOKEN -e lab" {
		t.Fatalf("wrong token: %+v", c)
	}
	if strings.Contains(body, "cf-token-1") || strings.Contains(body, "cf-token-2") {
		t.Error("doctor printed a Cloudflare token")
	}
}

func TestCloudflareOutageIsNotARefusal(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		f := cftest.New("cf-token-1", "acct")
		f.FailRequests(http.MethodGet, "/user/tokens/verify", status)
		f.FailRequests(http.MethodGet, "/accounts/", status)
		cfg := cloudflareCredentialConfig(t, f)
		s := openIngressStore(t)
		setIngress(t, s)
		putPlainSecret(t, s, "CF_API_TOKEN", "cf-token-1")
		checks, _ := credentialDoctor(t, cfg, s)
		c := checks["cloudflare_token"]
		if c.OK || c.Critical || !strings.HasPrefix(c.Detail, "could not be asked: ") || strings.Contains(c.Detail, "refused:") {
			t.Errorf("status %d: %+v", status, c)
		}
	}

	cfg := monitoringConfig("")
	cfg.CloudflareAPIURL = "http://" + dialtest.Refused
	cfg.dial = dialtest.Dial
	s := openIngressStore(t)
	setIngress(t, s)
	putPlainSecret(t, s, "CF_API_TOKEN", "cf-token-1")
	checks, _ := credentialDoctor(t, cfg, s)
	if c := checks["cloudflare_token"]; c.OK || c.Critical || c.Detail != "could not be asked: connection refused" {
		t.Errorf("connection refused: %+v", c)
	}
}

func TestCloudflareTokenThatIsNotStoredIsReported(t *testing.T) {
	f := cftest.New("cf-token-1", "acct")
	cfg := cloudflareCredentialConfig(t, f)
	s := openIngressStore(t)
	setIngress(t, s)
	checks, _ := credentialDoctor(t, cfg, s)
	if c := checks["cloudflare_token"]; c.OK || c.Critical || c.Detail != "secret CF_API_TOKEN is not stored: run berthy secret set CF_API_TOKEN -e lab" {
		t.Fatalf("not stored: %+v", c)
	}

	s = openIngressStore(t)
	checks, _ = credentialDoctor(t, cfg, s)
	if _, ok := checks["cloudflare_token"]; ok {
		t.Error("cloudflare_token checked without managed ingress")
	}
}

func TestMonitorChecksTheSameCredentials(t *testing.T) {
	reg := newCredentialRegistry(t, "pw-one")
	host := reg.host()
	f := cftest.New("cf-token-1", "acct")
	cfg := cloudflareCredentialConfig(t, f)
	s := openIngressStore(t)
	ctx := context.Background()
	recordRelease(t, s, "release-monitor", registryManifest(host))
	setIngress(t, s)
	putRegistryPassword(t, s, host, "pw-one")
	putPlainSecret(t, s, "CF_API_TOKEN", "cf-token-1")
	w := NewMonitoring(cfg, nil, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {})
	lookup := func() map[string]monitor.Check {
		checks := map[string]monitor.Check{}
		for _, c := range w.checks(ctx) {
			checks[c.Name()] = c
		}
		return checks
	}
	checks := lookup()
	for _, name := range []string{"registry:" + host, "cloudflare_token"} {
		c := checks[name]
		if c == nil {
			t.Fatalf("missing check %s", name)
		}
		if c.Interval() != 10*time.Minute || c.Failures() != 2 {
			t.Errorf("%s settings: %v %d", name, c.Interval(), c.Failures())
		}
		if got := c.Run(ctx); got.State != monitor.OK {
			t.Errorf("%s with good credentials: %+v", name, got)
		}
	}

	reg.accept("pw-two")
	got := checks["registry:"+host].Run(ctx)
	doctor, _ := credentialDoctor(t, cfg, s)
	if got.State != monitor.Critical || got.Detail != doctor["registry:"+host].Detail {
		t.Errorf("changed password: monitor %+v, doctor %+v", got, doctor["registry:"+host])
	}

	closed := dialtest.Refused
	cfg.dial = dialtest.Dial
	s2 := openIngressStore(t)
	recordRelease(t, s2, "release-closed", registryManifest(closed))
	putRegistryPassword(t, s2, closed, "pw-one")
	w2 := NewMonitoring(cfg, nil, &deploy.Controller{Store: s2, Environment: "lab"}, func(string) {})
	for _, c := range w2.checks(ctx) {
		if c.Name() == "registry:"+closed {
			if got := c.Run(ctx); got.State != monitor.Unknown {
				t.Errorf("closed registry: %+v", got)
			}
			return
		}
	}
	t.Fatal("no check for the closed registry")
}

func TestDoctorPutsCredentialChecksAfterCompose(t *testing.T) {
	reg := newCredentialRegistry(t, "pw-one")
	host := reg.host()
	e, _ := doctorDocker(t, map[string]string{"demo": attachableOverlay})
	s := openIngressStore(t)
	recordRelease(t, s, "release-credential-order", registryManifest(host))
	putRegistryPassword(t, s, host, "pw-one")
	h := NewDeploymentHandler(monitoringConfig(""), e, nil, &deploy.Controller{Store: s, Environment: "lab"})
	var names []string
	for _, c := range doctorChecks(t, h) {
		names = append(names, c.Name)
	}
	index := func(name string) int {
		for i, n := range names {
			if n == name {
				return i
			}
		}
		return -1
	}
	compose, credential, lock := index("compose"), index("registry:"+host), index("lock")
	if compose < 0 || credential < 0 || lock < 0 || !(compose < credential && credential < lock) {
		t.Fatalf("order: compose %d, credential %d, lock %d in %v", compose, credential, lock, names)
	}
}
