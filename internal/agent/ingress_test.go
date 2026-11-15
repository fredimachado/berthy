package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/cloudflare/cftest"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// The Cloudflare token is a test value.
const ingressTestCFToken = "cf-test-token-not-real"

// planEngine answers the one Docker call a dry run makes, the overlay network check.
type planEngine struct{ deploy.Engine }

func (planEngine) InspectNetwork(context.Context, string) (engine.Network, error) {
	return engine.Network{ID: "net", Driver: "overlay", Scope: "swarm", Attachable: true}, nil
}

// openIngressStore returns an empty encrypted state store for environment lab.
func openIngressStore(t *testing.T) *state.Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir, key, "lab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// ingressTestRequest is a dry-run request for a manifest with managed ingress and one hostname.
func ingressTestRequest(t *testing.T, replace bool) []byte {
	t.Helper()
	input := `{"project":"demo","network":"overlay","ingress":{"cloudflare":{"account_id":"acct","api_token_secret":"CF_TOKEN","image":"cloudflare/cloudflared@sha256:` + strings.Repeat("c", 64) + `"}},"apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{"port":8080,"hostnames":["api.example.com"]}}}}}`
	m, err := manifest.DecodeJSON([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(deploy.Request{Manifest: *m, RequestID: "ingress-test", DryRun: true, ReplaceExisting: replace})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestIngressRoutesEnforceScopeAndAdministratorForReplace(t *testing.T) {
	s := openIngressStore(t)
	readToken := "bty.lab." + strings.Repeat("a", 64)
	deployToken := "bty.lab." + strings.Repeat("b", 64)
	adminToken := "bty.lab." + strings.Repeat("c", 64)
	cfg := Config{Environment: "lab", Tokens: []Token{{Digest: Digest(readToken), Scope: "read"}, {Digest: Digest(deployToken), Scope: "deploy"}, {Digest: Digest(adminToken), Scope: "admin"}}}
	h := NewDeploymentHandler(cfg, nil, nil, &deploy.Controller{Store: s, Environment: "lab"})
	call := func(method, path, token string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/v1/environments/lab"+path, strings.NewReader(string(body)))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	plain := ingressTestRequest(t, false)
	replace := ingressTestRequest(t, true)
	for _, tc := range []struct {
		name, method, path, token string
		body                      []byte
		want                      int
	}{
		{"read scope cannot apply", "POST", "/ingress/apply", readToken, plain, http.StatusForbidden},
		{"deploy scope cannot replace existing", "POST", "/ingress/apply", deployToken, replace, http.StatusForbidden},
		{"apply needs POST", "GET", "/ingress/apply", deployToken, nil, http.StatusMethodNotAllowed},
		{"admin dry run without the token secret is refused, not a server error", "POST", "/ingress/apply", adminToken, plain, http.StatusBadRequest},
		{"status needs read scope", "GET", "/ingress/status", "bad", nil, http.StatusUnauthorized},
	} {
		if w := call(tc.method, tc.path, tc.token, tc.body); w.Code != tc.want {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
	}
	w := call("GET", "/ingress/status", readToken, nil)
	var status struct {
		Configured bool  `json:"configured"`
		Routes     []any `json:"routes"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &status) != nil || status.Configured || status.Routes == nil {
		t.Fatalf("status on an empty store: %d %s", w.Code, w.Body.String())
	}
}

func TestIngressAdminDryRunReturnsThePlanWithoutWriting(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	if err := s.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: "CF_TOKEN"}, []byte(ingressTestCFToken)); err != nil {
		t.Fatal(err)
	}
	fake := cftest.New(ingressTestCFToken, "acct")
	zone := fake.AddZone("example.com")
	srv := httptest.NewServer(fake.Handler())
	t.Cleanup(srv.Close)
	adminToken := "bty.lab." + strings.Repeat("c", 64)
	cfg := Config{Environment: "lab", Tokens: []Token{{Digest: Digest(adminToken), Scope: "admin"}}}
	ctrl := &deploy.Controller{Engine: planEngine{}, Store: s, Environment: "lab", CloudflareURL: srv.URL + "/client/v4"}
	h := NewDeploymentHandler(cfg, nil, nil, ctrl)
	r := httptest.NewRequest("POST", "/v1/environments/lab/ingress/apply", strings.NewReader(string(ingressTestRequest(t, false))))
	r.Header.Set("Authorization", "Bearer "+adminToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"create_tunnel":true`) || !strings.Contains(w.Body.String(), `"tunnel":"demo-lab"`) {
		t.Fatalf("dry run: %d %s", w.Code, w.Body.String())
	}
	if fake.ConfigPuts != 0 || fake.RecordWrites != 0 || len(fake.Records(zone)) != 0 {
		t.Fatal("dry run wrote to Cloudflare")
	}
	if _, err := s.Ingress(ctx); err == nil {
		t.Fatal("dry run recorded ingress")
	}
}

func TestDoctorReportsTunnelHealthWhenIngressIsConfigured(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	if err := s.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: "CF_TOKEN"}, []byte(ingressTestCFToken)); err != nil {
		t.Fatal(err)
	}
	fake := cftest.New(ingressTestCFToken, "acct")
	srv := httptest.NewServer(fake.Handler())
	t.Cleanup(srv.Close)
	tunnelID := fake.AddTunnel("demo-lab", true)
	fake.SetTunnelStatus(tunnelID, "healthy", 1)
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "ingress-state", ActorID: "admin", Kind: "ingress", App: "ingress", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetIngress(ctx, op.ID, state.IngressState{AccountID: "acct", APITokenSecret: "CF_TOKEN", TunnelID: tunnelID, TunnelName: "demo-lab", DNS: true}); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	token := "bty.lab." + strings.Repeat("a", 64)
	cfg := Config{Environment: "lab", Tokens: []Token{{Digest: Digest(token), Scope: "read"}}}
	h := NewDeploymentHandler(cfg, nil, nil, &deploy.Controller{Store: s, Environment: "lab", CloudflareURL: srv.URL + "/client/v4"})
	r := httptest.NewRequest("GET", "/v1/environments/lab/doctor", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `{"name":"ingress_tunnel","ok":true,"critical":false,"detail":"healthy"}`) {
		t.Fatalf("doctor: %d %s", w.Code, w.Body.String())
	}
}

func TestCloudflareURLMustBeHTTPSOrLoopback(t *testing.T) {
	base := Config{Environment: "lab", Listen: "127.0.0.1:9999", Tokens: []Token{{Digest: Digest("bty.lab." + strings.Repeat("a", 64)), Scope: "read"}}}
	for raw, ok := range map[string]bool{
		"https://api.cloudflare.com/client/v4": true,
		"http://127.0.0.1:9999":                true,
		"http://localhost:8080/client/v4":      true,
		"http://[::1]:8080":                    true,
		"http://example.com":                   false,
		"https://user:secret@api.example.com":  false,
		"https://api.cloudflare.com/?x=1":      false,
		"https://api.cloudflare.com/#frag":     false,
		"ftp://api.cloudflare.com":             false,
		"not a url":                            false,
	} {
		c := base
		c.CloudflareAPIURL = raw
		if err := c.Validate(); (err == nil) != ok {
			t.Errorf("%s: valid=%v err=%v", raw, ok, err)
		}
	}
}

func TestLoadRejectsPlainHTTPCloudflareURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	body := func(url string) []byte {
		return []byte(`{"environment":"lab","listen":"127.0.0.1:9999","docker_socket":"/var/run/docker.sock","cloudflare_api_url":"` + url + `","tokens":[{"digest":"` + Digest("bty.lab."+strings.Repeat("a", 64)) + `","scope":"read"}]}`)
	}
	if err := os.WriteFile(path, body("http://example.com"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("plain HTTP to a public host accepted")
	}
	if err := os.WriteFile(path, body("http://127.0.0.1:9999"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("loopback fake refused: %v", err)
	}
}

func TestIngressFailureMessagesReachOperators(t *testing.T) {
	for _, text := range []string{"ingress hostname conflict", "ingress publish failed", "tunnel is not remotely managed", "Cloudflare unavailable", "ingress state recording failed", "connector removal failed"} {
		if got := safeOperationError(state.Operation{Err: text}); got != text {
			t.Fatalf("%q hidden: %q", text, got)
		}
	}
}
