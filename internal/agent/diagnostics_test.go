package agent

import (
	"berthy/internal/deploy"
	"berthy/internal/state"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type diagnosticsFixture struct {
	h              http.Handler
	store          *state.Store
	read, deployer string
	failed, clean  string
}

func newDiagnosticsFixture(t *testing.T) diagnosticsFixture {
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
	ctx := context.Background()
	failed, _, err := s.Admit(ctx, state.Admission{RequestID: "r1", ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.PutDiagnostics(ctx, failed.ID, []json.RawMessage{json.RawMessage(`{"kind":"task","name":"proj-app-web","role":"web","state":"failed","exit_code":1,"logs":"config: REQUIRED_SETTING is missing\n","captured":"2026-11-16T10:00:00Z"}`)}); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, failed.ID, state.Outcome{State: "failed", Err: "rollout failed and was cleaned up", Reason: "role web: task exited with code 1"}); err != nil {
		t.Fatal(err)
	}
	clean, _, err := s.Admit(ctx, state.Admission{RequestID: "r2", ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, clean.ID, state.Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	read := "bty.lab." + strings.Repeat("a", 64)
	deployer := "bty.lab." + strings.Repeat("b", 64)
	cfg := Config{Environment: "lab", Tokens: []Token{{Digest: Digest(read), Scope: "read"}, {Digest: Digest(deployer), Scope: "deploy"}}}
	return diagnosticsFixture{h: NewDeploymentHandler(cfg, nil, nil, &deploy.Controller{Store: s}), store: s, read: read, deployer: deployer, failed: failed.ID, clean: clean.ID}
}

func (f diagnosticsFixture) get(path, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/v1/environments/lab"+path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func TestDiagnosticsNeedDeployScope(t *testing.T) {
	f := newDiagnosticsFixture(t)
	path := "/operations/" + f.failed + "/diagnostics"
	if w := f.get(path, f.read); w.Code != 403 || strings.Contains(w.Body.String(), "REQUIRED_SETTING") {
		t.Fatalf("read token: %d %s", w.Code, w.Body.String())
	}
	if w := f.get(path, ""); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
	w := f.get(path, f.deployer)
	if w.Code != 200 {
		t.Fatalf("deploy token: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		OperationID string `json:"operation_id"`
		Reason      string `json:"reason"`
		Diagnostics []struct {
			Kind     string `json:"kind"`
			ExitCode int    `json:"exit_code"`
			Logs     string `json:"logs"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.OperationID != f.failed || got.Reason != "role web: task exited with code 1" || len(got.Diagnostics) != 1 || got.Diagnostics[0].ExitCode != 1 || !strings.Contains(got.Diagnostics[0].Logs, "REQUIRED_SETTING is missing") {
		t.Fatalf("response %s", w.Body.String())
	}
}

func TestDiagnosticsAreNotFoundWithoutEvidence(t *testing.T) {
	f := newDiagnosticsFixture(t)
	for _, id := range []string{f.clean, "unknown"} {
		if w := f.get("/operations/"+id+"/diagnostics", f.deployer); w.Code != 404 {
			t.Fatalf("%s: %d %s", id, w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest("POST", "/v1/environments/lab/operations/"+f.failed+"/diagnostics", nil)
	r.Header.Set("Authorization", "Bearer "+f.deployer)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != 405 {
		t.Fatalf("POST: %d", w.Code)
	}
}

func TestOperationHistoryAndEventsCarryTheReasonButNotTheLogs(t *testing.T) {
	f := newDiagnosticsFixture(t)
	w := f.get("/operations/"+f.failed, f.read)
	var op map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &op); err != nil || op["reason"] != "role web: task exited with code 1" || op["error"] != "rollout failed and was cleaned up" {
		t.Fatalf("operation %s", w.Body.String())
	}
	if w = f.get("/operations/"+f.clean, f.read); strings.Contains(w.Body.String(), `"reason"`) {
		t.Fatalf("a successful operation has no reason: %s", w.Body.String())
	}
	w = f.get("/history/app", f.read)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"reason":"role web: task exited with code 1"`) || strings.Contains(w.Body.String(), "REQUIRED_SETTING") {
		t.Fatalf("history %s", w.Body.String())
	}
	w = f.get("/operations/"+f.failed+"/events", f.read)
	if !strings.Contains(w.Body.String(), `"reason":"role web: task exited with code 1"`) || strings.Contains(w.Body.String(), "REQUIRED_SETTING") {
		t.Fatalf("events %s", w.Body.String())
	}
}

func TestOperationReasonIsDroppedWhenItIsNotPublic(t *testing.T) {
	for _, reason := range []string{"role web: password=hunter2\nsecond line", strings.Repeat("x", 301), "café"} {
		if got := safeReason(reason); got != "" {
			t.Fatalf("reason %q was kept", reason)
		}
	}
	if got := safeReason("role web: task exited with code 1"); got == "" {
		t.Fatal("public reason dropped")
	}
}
