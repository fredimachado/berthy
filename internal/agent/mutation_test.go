package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/deploy"
	"berthy/internal/state"
)

func TestOperationErrorShowsOnlyRefusals(t *testing.T) {
	refused := httptest.NewRecorder()
	operationError(refused, deploy.Refusal("a newer release followed the migration; use --to"))
	if refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), "a newer release followed the migration; use --to") {
		t.Fatalf("refusal: %d %s", refused.Code, refused.Body.String())
	}
	plain := httptest.NewRecorder()
	operationError(plain, errors.New("workload secret unavailable"))
	if plain.Code != http.StatusBadRequest || strings.Contains(plain.Body.String(), "workload secret") || !strings.Contains(plain.Body.String(), "operation unavailable or invalid") {
		t.Fatalf("plain error: %d %s", plain.Code, plain.Body.String())
	}
}

func TestSafeOperationErrorPublishesKnownTextOnly(t *testing.T) {
	for _, text := range []string{
		"a newer release followed the migration; use --to",
		"operation interrupted and cleaned up",
		"post-deploy completion was interrupted",
		"unsupported recovery step",
		"one-off recovery required",
		"verified release requires inspection",
		"service recovery required",
		"bound service unavailable",
		"invalid bound service",
		"bound service ownership changed",
		"prior service health cannot be verified",
		"prior image identity unavailable",
		"prior image is not pinned",
		"prior image credential unavailable",
		"removing managed roles unavailable",
		"role preflight failed",
		"image inspection failed",
		"image provenance recording failed",
		"adoption preflight failed",
		"migration source journal failed",
	} {
		if got := safeOperationError(state.Operation{Err: text}); got != text {
			t.Fatalf("%q hidden: %q", text, got)
		}
	}
	if got := safeOperationError(state.Operation{Err: "workload secret unavailable"}); got != "operation failed" {
		t.Fatalf("generic error shown: %q", got)
	}
}

func TestHistoryShowsRecordedSchemaStates(t *testing.T) {
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
	ctx := context.Background()
	finish := func(id string, out state.Outcome) state.Operation {
		t.Helper()
		op, _, err := s.Admit(ctx, state.Admission{RequestID: id, ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Finish(ctx, op.ID, out); err != nil {
			t.Fatal(err)
		}
		return op
	}
	finish("migrated", state.Outcome{State: "succeeded", SchemaBefore: "v0", SchemaAfter: "v1", Release: &state.Release{App: "app", Digest: "d1", Manifest: []byte(`{}`)}})
	finish("plain", state.Outcome{State: "succeeded", Release: &state.Release{App: "app", Digest: "d2", Manifest: []byte(`{}`)}})
	failed := finish("failed", state.Outcome{State: "failed", SchemaBefore: "v1", SchemaAfter: "v2"})
	token := "bty.lab." + strings.Repeat("a", 64)
	cfg := Config{Project: "demo", Environment: "lab", Tokens: []Token{{Digest: Digest(token), Scope: "read"}}}
	r := httptest.NewRequest("GET", "/v1/projects/demo/environments/lab/history/app", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	NewDeploymentHandler(cfg, nil, nil, &deploy.Controller{Store: s}).ServeHTTP(w, r)
	var rows []map[string]any
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rows) != nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	seen := map[string]bool{}
	for _, row := range rows {
		switch {
		case row["digest"] == "d1":
			seen["migrated"] = true
			if row["schema_before"] != "v0" || row["schema_after"] != "v1" {
				t.Fatalf("release row %v", row)
			}
		case row["digest"] == "d2":
			seen["plain"] = true
			if _, ok := row["schema_before"]; ok {
				t.Fatalf("release without a schema shows one: %v", row)
			}
		case row["operation_id"] == failed.ID:
			seen["failed"] = true
			if row["schema_before"] != "v1" || row["schema_after"] != "v2" {
				t.Fatalf("failed row %v", row)
			}
		}
	}
	if len(seen) != 3 {
		t.Fatalf("rows missing %v in %s", seen, w.Body.String())
	}
}

func TestRecoveryResolveScopesValidationAndResult(t *testing.T) {
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
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "stuck", ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "recovery_required", Err: "service recovery required"}); err != nil {
		t.Fatal(err)
	}
	readToken := "bty.lab." + strings.Repeat("a", 64)
	deployToken := "bty.lab." + strings.Repeat("b", 64)
	adminToken := "bty.lab." + strings.Repeat("c", 64)
	cfg := Config{Project: "demo", Environment: "lab", Tokens: []Token{{Digest: Digest(readToken), Scope: "read"}, {Digest: Digest(deployToken), Scope: "deploy"}, {Digest: Digest(adminToken), Scope: "admin"}}}
	h := NewDeploymentHandler(cfg, nil, nil, &deploy.Controller{Store: s})
	call := func(method, path, token, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/v1/projects/demo/environments/lab"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	resolve := "/operations/" + op.ID + "/resolve"
	for _, tc := range []struct {
		name, method, token, body string
		want                      int
	}{
		{"read scope", "POST", readToken, `{"reason":"service removed"}`, http.StatusForbidden},
		{"deploy scope", "POST", deployToken, `{"reason":"service removed"}`, http.StatusForbidden},
		{"wrong method", "GET", adminToken, ``, http.StatusMethodNotAllowed},
		{"empty reason", "POST", adminToken, `{"reason":""}`, http.StatusBadRequest},
		{"long reason", "POST", adminToken, `{"reason":"` + strings.Repeat("x", 201) + `"}`, http.StatusBadRequest},
		{"unknown field", "POST", adminToken, `{"reason":"service removed","force":true}`, http.StatusBadRequest},
	} {
		if w := call(tc.method, resolve, tc.token, tc.body); w.Code != tc.want {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
	}
	dry := call("POST", resolve, adminToken, `{"reason":"service removed","dry_run":true}`)
	var preview struct {
		Resolved bool `json:"resolved"`
	}
	if dry.Code != http.StatusOK || json.Unmarshal(dry.Body.Bytes(), &preview) != nil || preview.Resolved {
		t.Fatalf("dry run: %d %s", dry.Code, dry.Body.String())
	}
	if _, err = s.Lock(ctx); err != nil {
		t.Fatalf("dry run released the lock: %v", err)
	}
	done := call("POST", resolve, adminToken, `{"reason":"service deleted outside berthy"}`)
	if err = json.Unmarshal(done.Body.Bytes(), &preview); done.Code != http.StatusOK || err != nil || !preview.Resolved {
		t.Fatalf("resolve: %d %s", done.Code, done.Body.String())
	}
	status := call("GET", "/operations/"+op.ID, readToken, "")
	var shown struct {
		State string `json:"state"`
		Error string `json:"error"`
	}
	if status.Code != http.StatusOK || json.Unmarshal(status.Body.Bytes(), &shown) != nil || shown.State != "failed" || shown.Error != "recovery resolved by admin" {
		t.Fatalf("operation after resolve: %d %s", status.Code, status.Body.String())
	}
	again := call("POST", resolve, adminToken, `{"reason":"again"}`)
	if again.Code != http.StatusBadRequest || !strings.Contains(again.Body.String(), "operation is not waiting for recovery") {
		t.Fatalf("second resolve: %d %s", again.Code, again.Body.String())
	}
}

func TestValidReasonBounds(t *testing.T) {
	for _, tc := range []struct {
		name, s string
		want    bool
	}{
		{"empty", "", false},
		{"short", "service removed", true},
		{"200 bytes", strings.Repeat("x", 200), true},
		{"201 bytes", strings.Repeat("x", 201), false},
		{"multibyte at limit", strings.Repeat("é", 100), true},
		{"multibyte over limit", strings.Repeat("é", 101), false},
		{"newline", "a\nb", false},
		{"tab", "a\tb", false},
		{"invalid utf-8", "bad\xffbyte", false},
	} {
		if got := validReason(tc.s); got != tc.want {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
}

func TestHistoryShowsTheTagOfAReleaseOnlyWhenThereIsOne(t *testing.T) {
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
	ctx := context.Background()
	finish := func(id string, out state.Outcome) {
		t.Helper()
		op, _, err := s.Admit(ctx, state.Admission{RequestID: id, ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Finish(ctx, op.ID, out); err != nil {
			t.Fatal(err)
		}
	}
	finish("tagged", state.Outcome{State: "succeeded", Release: &state.Release{App: "app", Digest: "tagged-digest", Tag: "v1.0.2", Manifest: []byte(`{}`)}})
	finish("untagged", state.Outcome{State: "succeeded", Release: &state.Release{App: "app", Digest: "untagged-digest", Manifest: []byte(`{}`)}})
	token := "bty.lab." + strings.Repeat("a", 64)
	cfg := Config{Project: "demo", Environment: "lab", Tokens: []Token{{Digest: Digest(token), Scope: "read"}}}
	r := httptest.NewRequest("GET", "/v1/projects/demo/environments/lab/history/app", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	NewDeploymentHandler(cfg, nil, nil, &deploy.Controller{Store: s}).ServeHTTP(w, r)
	var rows []map[string]any
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rows) != nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	seen := map[string]bool{}
	for _, row := range rows {
		switch row["digest"] {
		case "tagged-digest":
			seen["tagged"] = true
			if row["tag"] != "v1.0.2" {
				t.Fatalf("tagged release row %v", row)
			}
		case "untagged-digest":
			seen["untagged"] = true
			if _, ok := row["tag"]; ok {
				t.Fatalf("untagged release shows a tag: %v", row)
			}
		}
	}
	if !seen["tagged"] || !seen["untagged"] {
		t.Fatalf("history %s", w.Body.String())
	}
}

// TestAppliedShowsTheReleaseAnAppRunsNow: the read that tells berthy deploy whether an app can keep its version. A removed or
// never deployed app has no applied release, even when it has a history.
func TestAppliedShowsTheReleaseAnAppRunsNow(t *testing.T) {
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
	ctx := context.Background()
	for i, release := range []state.Release{
		{App: "api", Digest: "repo@sha256:old", Tag: "v1.0.1", Manifest: []byte(`{}`)},
		{App: "api", Digest: "repo@sha256:new", Tag: "v1.0.2", Manifest: []byte(`{}`)},
		{App: "worker", Digest: "repo@sha256:w", Manifest: []byte(`{}`)},
	} {
		op, _, err := s.Admit(ctx, state.Admission{RequestID: fmt.Sprint("r", i), ActorID: "a", Kind: "deploy", App: release.App, PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &release}); err != nil {
			t.Fatal(err)
		}
	}
	token := "bty.lab." + strings.Repeat("a", 64)
	cfg := Config{Project: "demo", Environment: "lab", Tokens: []Token{{Digest: Digest(token), Scope: "read"}}}
	get := func(app string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/v1/projects/demo/environments/lab/applied/"+app, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		NewDeploymentHandler(cfg, nil, nil, &deploy.Controller{Store: s}).ServeHTTP(w, r)
		return w
	}
	var got map[string]any
	if w := get("api"); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got["digest"] != "repo@sha256:new" || got["tag"] != "v1.0.2" || got["id"] == nil {
		t.Fatalf("api: %d %s", w.Code, w.Body.String())
	}
	got = nil
	if w := get("worker"); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got["digest"] != "repo@sha256:w" {
		t.Fatalf("worker: %d %s", w.Code, w.Body.String())
	} else if _, ok := got["tag"]; ok {
		t.Fatalf("an untagged release shows a tag: %s", w.Body.String())
	}
	if w := get("missing"); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"not_deployed"`) {
		t.Fatalf("an app with no applied release: %d %s", w.Code, w.Body.String())
	}
}

// TestKeepVersionDeployOfAnAppNeverDeployedIsRefusedWithItsMessage: berthy deploy prints the agent's refusal as it is.
func TestKeepVersionDeployOfAnAppNeverDeployedIsRefusedWithItsMessage(t *testing.T) {
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
	token := "bty.lab." + strings.Repeat("b", 64)
	cfg := Config{Project: "demo", Environment: "lab", Tokens: []Token{{Digest: Digest(token), Scope: "deploy"}}}
	body := `{"app":"api","request_id":"r1","keep_version":true,"manifest":{"project":"demo","network":"demo","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}}}}`
	r := httptest.NewRequest("POST", "/v1/projects/demo/environments/lab/deploys", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	NewDeploymentHandler(cfg, nil, nil, &deploy.Controller{Store: s}).ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "app api is not deployed yet: give --version api=REF") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}
