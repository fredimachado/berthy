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

	"berthy/internal/deploy"
	"berthy/internal/state"
)

// TestHistoryRowsCarryTheLocalChangesFlagOnlyWhenSet: a release or a failed deploy from a checkout with local changes says so,
// and a clean one shows no flag at all.
func TestHistoryRowsCarryTheLocalChangesFlagOnlyWhenSet(t *testing.T) {
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
	finish := func(id string, a state.Admission, out state.Outcome) {
		t.Helper()
		a.RequestID, a.ActorID, a.Kind, a.App, a.PublicRequest = id, "a", "deploy", "app", []byte(`{}`)
		op, _, err := s.Admit(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Finish(ctx, op.ID, out); err != nil {
			t.Fatal(err)
		}
	}
	finish("clean", state.Admission{Commit: "abc"}, state.Outcome{State: "succeeded", Release: &state.Release{App: "app", Digest: "clean-digest", Commit: "abc", Manifest: []byte(`{}`)}})
	finish("local", state.Admission{Commit: "abc", Modified: true}, state.Outcome{State: "succeeded", Release: &state.Release{App: "app", Digest: "local-digest", Commit: "abc", Modified: true, Manifest: []byte(`{}`)}})
	finish("failed-local", state.Admission{Commit: "def", Modified: true}, state.Outcome{State: "failed", Err: "private error"})
	finish("failed-clean", state.Admission{Commit: "ghi"}, state.Outcome{State: "failed", Err: "private error"})

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
		var key string
		switch {
		case row["digest"] == "clean-digest":
			key = "clean"
		case row["digest"] == "local-digest":
			key = "local"
		case row["operation_id"] != nil && row["commit"] == "def":
			key = "failed-local"
		case row["operation_id"] != nil && row["commit"] == "ghi":
			key = "failed-clean"
		}
		seen[key] = true
		// Only the rows of the flagged deploys carry the key, and it is true.
		flagged := key == "local" || key == "failed-local"
		value, has := row["modified"]
		if has != flagged || has && value != true {
			t.Fatalf("%s row %v: want flag %v", key, row, flagged)
		}
	}
	for _, want := range []string{"clean", "local", "failed-local", "failed-clean"} {
		if !seen[want] {
			t.Fatalf("no %s row in %s", want, w.Body.String())
		}
	}
}
