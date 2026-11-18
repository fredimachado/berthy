package agent

import (
	"berthy/internal/deploy"
	"berthy/internal/state"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAuthorizationBeforeDecode(t *testing.T) {
	read := "bty.lab." + strings.Repeat("a", 64)
	deployToken := "bty.lab." + strings.Repeat("b", 64)
	cfg := Config{Environment: "lab", Tokens: []Token{{Digest: Digest(read), Scope: "read"}, {Digest: Digest(deployToken), Scope: "deploy"}}}
	h := HandlerWithRuntime(cfg, nil, nil, nil)
	for _, tc := range []struct {
		token, env string
		want       int
	}{{read, "lab", 403}, {deployToken, "other", 401}, {deployToken, "lab", 503}} {
		r := httptest.NewRequest(http.MethodPost, "/v1/environments/"+tc.env+"/deploys", strings.NewReader("not json"))
		r.Header.Set("Authorization", "Bearer "+tc.token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s: %d %s", tc.env, w.Code, w.Body.String())
		}
	}
}
func TestOperationProjectionAndEventReplay(t *testing.T) {
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
	defer s.Close()
	op, _, err := s.Admit(context.Background(), state.Admission{RequestID: "private-request", ActorID: "private-fingerprint", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(context.Background(), op.ID, state.Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	token := "bty.lab." + strings.Repeat("a", 64)
	cfg := Config{Environment: "lab", Tokens: []Token{{Digest: Digest(token), Scope: "read"}}}
	h := NewDeploymentHandler(cfg, nil, nil, &deploy.Controller{Store: s})
	for _, path := range []string{"/operations/" + op.ID, "/operations/" + op.ID + "/events"} {
		r := httptest.NewRequest("GET", "/v1/environments/lab"+path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 || strings.Contains(w.Body.String(), "private-") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if strings.HasSuffix(path, "events") && !strings.Contains(w.Body.String(), "event: succeeded") {
			t.Fatal(w.Body.String())
		}
	}
}
func TestHistoryDoesNotExposeManifest(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	key := filepath.Join(dir, "key")
	os.WriteFile(key, make([]byte, 32), 0600)
	s, err := state.Open(dir, key, "lab")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "r", ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "app", Manifest: []byte(`{"private":"canary"}`)}}); err != nil {
		t.Fatal(err)
	}
	token := "bty.lab." + strings.Repeat("a", 64)
	cfg := Config{Environment: "lab", Tokens: []Token{{Digest: Digest(token), Scope: "read"}}}
	r := httptest.NewRequest("GET", "/v1/environments/lab/history/app", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	NewDeploymentHandler(cfg, nil, nil, &deploy.Controller{Store: s}).ServeHTTP(w, r)
	if strings.Contains(w.Body.String(), "canary") {
		t.Fatal("manifest leaked")
	}
}
func TestTokenIdentityDoesNotRevealVerifier(t *testing.T) {
	token := "bty.lab." + strings.Repeat("a", 64)
	c := Config{Environment: "lab", Tokens: []Token{{Digest: Digest(token), Scope: "read"}}}
	r := httptest.NewRequest("GET", "/v1/environments/lab/audit", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	actor, _, status := authenticate(newTokenSource(c, nil), r)
	if status != http.StatusOK || actor.ID == Digest(token) {
		t.Fatal("token verifier exposed as actor identity")
	}
}
func TestTerminalStreamCarriesStatus(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	key := filepath.Join(dir, "key")
	os.WriteFile(key, make([]byte, 32), 0600)
	s, err := state.Open(dir, key, "lab")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	op, _, _ := s.Admit(ctx, state.Admission{RequestID: "r", ActorID: "a", Kind: "deploy", PublicRequest: []byte(`{}`)})
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "failed"}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	streamOperation(w, r, s, op.ID)
	if !strings.Contains(w.Body.String(), "event: terminal\ndata: {\"status\":\"failed\"") {
		t.Fatal(w.Body.String())
	}
}
func TestWaitRejectsUnboundedTimeout(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	key := filepath.Join(dir, "key")
	os.WriteFile(key, make([]byte, 32), 0600)
	s, err := state.Open(dir, key, "lab")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	token := "bty.lab." + strings.Repeat("a", 64)
	cfg := Config{Environment: "lab", Tokens: []Token{{Digest: Digest(token), Scope: "read"}}}
	r := httptest.NewRequest("GET", "/v1/environments/lab/wait/app?timeout=24h", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	NewDeploymentHandler(cfg, nil, nil, &deploy.Controller{Store: s}).ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}
func TestExecOutputIsAdminOnly(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	key := filepath.Join(dir, "key")
	os.WriteFile(key, make([]byte, 32), 0600)
	s, err := state.Open(dir, key, "lab")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "r", ActorID: "a", Kind: "exec", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordIntent(ctx, op.ID, state.Intent{Ordinal: 30000, Kind: "exec", Desired: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkStep(ctx, op.ID, 30000, "finished", []byte(`{"output":"safe","exit_code":3}`)); err != nil {
		t.Fatal(err)
	}
	read := "bty.lab." + strings.Repeat("a", 64)
	admin := "bty.lab." + strings.Repeat("b", 64)
	cfg := Config{Environment: "lab", Tokens: []Token{{Digest: Digest(read), Scope: "read"}, {Digest: Digest(admin), Scope: "admin"}}}
	h := NewDeploymentHandler(cfg, nil, nil, &deploy.Controller{Store: s})
	for _, tc := range []struct {
		token string
		code  int
	}{{read, 403}, {admin, 200}} {
		r := httptest.NewRequest("GET", "/v1/environments/lab/operations/"+op.ID+"/output", nil)
		r.Header.Set("Authorization", "Bearer "+tc.token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("%d: %s", w.Code, w.Body.String())
		}
		if tc.code == 200 && !strings.Contains(w.Body.String(), `"exit_code":3`) {
			t.Fatal(w.Body.String())
		}
	}
}
func TestHistoryIncludesFailuresWithoutPrivateErrors(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	key := filepath.Join(dir, "key")
	os.WriteFile(key, make([]byte, 32), 0600)
	s, err := state.Open(dir, key, "lab")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "r", ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "failed", Err: "private-canary"}); err != nil {
		t.Fatal(err)
	}
	token := "bty.lab." + strings.Repeat("a", 64)
	cfg := Config{Environment: "lab", Tokens: []Token{{Digest: Digest(token), Scope: "read"}}}
	r := httptest.NewRequest("GET", "/v1/environments/lab/history/app", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	NewDeploymentHandler(cfg, nil, nil, &deploy.Controller{Store: s}).ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"state":"failed"`) || strings.Contains(w.Body.String(), "private-canary") {
		t.Fatal(w.Body.String())
	}
}
func TestOperationErrorProjectionAllowsKnownTextOnly(t *testing.T) {
	if got := safeOperationError(state.Operation{Err: "pre-deploy command failed"}); got != "pre-deploy command failed" {
		t.Fatal(got)
	}
	if got := safeOperationError(state.Operation{Err: "private secret"}); got != "operation failed" {
		t.Fatal(got)
	}
}
