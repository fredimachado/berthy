package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// secretDocker is a Docker with one service, api-web, that runs an image of ghcr.io.
type secretDocker struct {
	deploy.Engine
	spec    json.RawMessage
	updates []engine.RegistryAuth
}

func (d *secretDocker) InspectService(context.Context, string) (engine.ServiceSnapshot, error) {
	return engine.ServiceSnapshot{ID: "id-api", Version: 5, Spec: d.spec}, nil
}

func (d *secretDocker) UpdateService(_ context.Context, _ string, _ uint64, _ json.RawMessage, auth *engine.RegistryAuth) error {
	d.updates = append(d.updates, *auth)
	return nil
}

type secretFixture struct {
	t      *testing.T
	s      *state.Store
	docker *secretDocker
	h      http.Handler
}

func newSecretFixture(t *testing.T) *secretFixture {
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
	f := &secretFixture{t: t, s: s, docker: &secretDocker{spec: json.RawMessage(`{"Name":"api-web","TaskTemplate":{"ContainerSpec":{"Image":"ghcr.io/acme/api:1"}}}`)}}
	cfg := Config{Project: "demo", Environment: "lab", Tokens: []Token{{Name: "alice", Digest: Digest(apAdminToken), Scope: "admin"}}}
	f.h = NewDeploymentHandler(cfg, nil, nil, &deploy.Controller{Engine: f.docker, Store: s, Environment: "lab"})
	return f
}

// bindAPI applies the app api, which pulls from ghcr.io with the secret GHCR_TOKEN.
func (f *secretFixture) bindAPI() {
	f.t.Helper()
	ctx := context.Background()
	m, err := json.Marshal(manifest.Manifest{Project: "p", Network: "n",
		Registries: map[string]manifest.Registry{"ghcr.io": {Username: "robot", PasswordSecret: "GHCR_TOKEN"}},
		Apps:       map[string]manifest.App{"api": {Kind: "image", Image: "ghcr.io/acme/api"}}})
	if err != nil {
		f.t.Fatal(err)
	}
	op, _, err := f.s.Admit(ctx, state.Admission{RequestID: "deploy", ActorID: "a", Kind: "deploy", App: "api", PublicRequest: []byte(`{}`)})
	if err != nil {
		f.t.Fatal(err)
	}
	if err = f.s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "api", Manifest: m, Bindings: []state.Binding{{App: "api", Role: "web", Name: "api-web", ServiceID: "id-api"}}}}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *secretFixture) call(method, path, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, "/v1/projects/demo/environments/lab"+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+apAdminToken)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

// set posts a secret and returns the answer, which must be a success.
func (f *secretFixture) set(body string) string {
	f.t.Helper()
	w := f.call("POST", "/secrets", body)
	if w.Code != http.StatusOK {
		f.t.Fatalf("set %s: %d %s", body, w.Code, w.Body.String())
	}
	return w.Body.String()
}

func wantJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if json.Unmarshal([]byte(got), &g) != nil || json.Unmarshal([]byte(want), &w) != nil || !reflect.DeepEqual(g, w) {
		t.Fatalf("answer %s, want %s", got, want)
	}
}

func (f *secretFixture) registryOf(name string) string {
	f.t.Helper()
	w := f.call("GET", "/secrets", "")
	var rows []map[string]string
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rows) != nil {
		f.t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	for _, r := range rows {
		if r["name"] == name {
			return r["registry"]
		}
	}
	f.t.Fatalf("%s not listed: %v", name, rows)
	return ""
}

func TestSecretSetKeepsTheRegistryWhenItIsOmitted(t *testing.T) {
	f := newSecretFixture(t)
	f.set(`{"name":"GHCR_TOKEN","value":"a","registry":"ghcr.io"}`)
	f.set(`{"name":"GHCR_TOKEN","value":"b"}`)
	if got := f.registryOf("GHCR_TOKEN"); got != "ghcr.io" {
		t.Fatalf("registry %q", got)
	}
	info, value, err := f.s.Secret(context.Background(), "GHCR_TOKEN")
	if err != nil || string(value) != "b" || info.Registry != "ghcr.io" {
		t.Fatalf("stored %+v %q %v", info, value, err)
	}
}

func TestSecretSetWithAnotherRegistryReplacesIt(t *testing.T) {
	f := newSecretFixture(t)
	f.set(`{"name":"GHCR_TOKEN","value":"a","registry":"ghcr.io"}`)
	f.set(`{"name":"GHCR_TOKEN","value":"b","registry":"other.example"}`)
	if got := f.registryOf("GHCR_TOKEN"); got != "other.example" {
		t.Fatalf("registry %q", got)
	}
}

func TestSecretSetOfARegistryPasswordReportsTheServices(t *testing.T) {
	f := newSecretFixture(t)
	wantJSON(t, f.set(`{"name":"GHCR_TOKEN","value":"a","registry":"ghcr.io"}`), `{"ok":true,"registry":"ghcr.io","services":[]}`)
	if rows, _ := f.s.AuditLog(context.Background(), 100); strings.Contains(auditText(rows), "registry_auth_refresh") {
		t.Fatalf("audit for no service: %s", auditText(rows))
	}

	f.bindAPI()
	wantJSON(t, f.set(`{"name":"GHCR_TOKEN","value":"fresh"}`), `{"ok":true,"registry":"ghcr.io","services":[{"name":"api-web","updated":true}]}`)
	if want := []engine.RegistryAuth{{Username: "robot", Password: "fresh", ServerAddress: "ghcr.io"}}; !reflect.DeepEqual(f.docker.updates, want) {
		t.Fatalf("Docker got %+v", f.docker.updates)
	}
	rows, err := f.s.AuditLog(context.Background(), 100)
	if err != nil || !strings.Contains(auditText(rows), "registry_auth_refresh:ghcr.io succeeded") {
		t.Fatalf("audit %v: %s", err, auditText(rows))
	}
}

func auditText(rows []state.Audit) string {
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(r.Action + " " + r.Outcome + "\n")
	}
	return b.String()
}

func TestSecretSetOfAPlainSecretAnswersAsBefore(t *testing.T) {
	f := newSecretFixture(t)
	f.bindAPI()
	wantJSON(t, f.set(`{"name":"APP_KEY","value":"v"}`), `{"ok":true}`)
	if len(f.docker.updates) != 0 {
		t.Fatalf("Docker got %+v", f.docker.updates)
	}
}

func TestSecretSetDryRunShowsTheKeptRegistryAndTheServices(t *testing.T) {
	f := newSecretFixture(t)
	f.set(`{"name":"GHCR_TOKEN","value":"a","registry":"ghcr.io"}`)
	f.bindAPI()
	w := f.call("POST", "/secrets", `{"name":"GHCR_TOKEN","value":"b","dry_run":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	wantJSON(t, w.Body.String(), `{"action":"secret set","name":"GHCR_TOKEN","exists":true,"registry":"ghcr.io","services":["api-web"]}`)
	if _, value, err := f.s.Secret(context.Background(), "GHCR_TOKEN"); err != nil || string(value) != "a" {
		t.Fatalf("a dry run stored %q %v", value, err)
	}
	if len(f.docker.updates) != 0 {
		t.Fatalf("a dry run changed Docker: %+v", f.docker.updates)
	}
	// A plain secret keeps the answer it always had.
	w = f.call("POST", "/secrets", `{"name":"APP_KEY","value":"v","dry_run":true}`)
	wantJSON(t, w.Body.String(), `{"action":"secret set","name":"APP_KEY","exists":false,"registry":""}`)
}
