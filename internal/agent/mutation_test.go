package agent

import (
	"context"
	"encoding/json"
	"errors"
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
	cfg := Config{Environment: "lab", Tokens: []Token{{Digest: Digest(token), Scope: "read"}}}
	r := httptest.NewRequest("GET", "/v1/environments/lab/history/app", nil)
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
