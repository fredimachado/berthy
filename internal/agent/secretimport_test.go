package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"berthy/internal/api"
	"berthy/internal/state"
)

// importBody is the body of a secrets/import request with the entries given, and dry run when dry is true.
func importBody(t *testing.T, dry bool, entries ...api.SecretEntry) string {
	t.Helper()
	b, err := json.Marshal(api.SecretImport{Secrets: entries, DryRun: dry})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// storedValue reads a secret of the fixture's store, or returns ok false when there is none.
func storedValue(t *testing.T, f *approvalFixture, name string) (string, bool) {
	t.Helper()
	_, v, err := f.s.Secret(context.Background(), name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(v), true
}

// TestSecretImportNeedsAnAdminToken checks that only an admin token imports, as it sets one secret.
func TestSecretImportNeedsAnAdminToken(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	body := importBody(t, false, api.SecretEntry{Name: "A", Value: "v"})
	for _, token := range []string{apReadToken, apDeployToken} {
		if w := f.call("POST", "/secrets/import", token, body); w.Code != http.StatusForbidden {
			t.Fatalf("token %s: %d %s", token[:12], w.Code, w.Body.String())
		}
	}
	if _, ok := storedValue(t, f, "A"); ok {
		t.Fatal("a refused import stored a secret")
	}
	if w := f.call("POST", "/secrets/import", apAdminToken, body); w.Code != http.StatusOK {
		t.Fatalf("admin token: %d %s", w.Code, w.Body.String())
	}
}

// TestSecretImportDryRunListsNamesSortedAndNoValue checks the dry run: the names it would create and replace, sorted, and
// nothing written and no value in the answer.
func TestSecretImportDryRunListsNamesSortedAndNoValue(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	const canary = "dry-run-import-canary"
	if err := f.s.PutSecret(context.Background(), state.Actor{ID: "admin"}, state.SecretInfo{Name: "OLD"}, []byte("old-value")); err != nil {
		t.Fatal(err)
	}
	w := f.call("POST", "/secrets/import", apAdminToken, importBody(t, true,
		api.SecretEntry{Name: "NEW_B", Value: canary}, api.SecretEntry{Name: "OLD", Value: canary}, api.SecretEntry{Name: "NEW_A", Value: canary}))
	if w.Code != http.StatusOK {
		t.Fatalf("dry run: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), canary) {
		t.Fatalf("the dry run shows a value: %s", w.Body.String())
	}
	if !sameJSON(t, w.Body.Bytes(), []byte(`{"action":"secret import","create":["NEW_A","NEW_B"],"replace":["OLD"]}`)) {
		t.Fatalf("plan: %s", w.Body.String())
	}
	if _, ok := storedValue(t, f, "NEW_A"); ok {
		t.Fatal("a dry run stored a secret")
	}
	if v, _ := storedValue(t, f, "OLD"); v != "old-value" {
		t.Fatalf("a dry run replaced OLD: %q", v)
	}
}

// TestSecretImportStoresEverySecretAndAuditsEachOne checks the import itself: every entry is stored with its value, the answer
// names what was created and what was replaced, and each secret has its own audit row.
func TestSecretImportStoresEverySecretAndAuditsEachOne(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	ctx := context.Background()
	if err := f.s.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: "C"}, []byte("old")); err != nil {
		t.Fatal(err)
	}
	w := f.call("POST", "/secrets/import", apAdminToken, importBody(t, false,
		api.SecretEntry{Name: "A", Value: "value-of-a"}, api.SecretEntry{Name: "B", Value: "value=with=equals"}, api.SecretEntry{Name: "C", Value: "value-of-c"}))
	if w.Code != http.StatusOK {
		t.Fatalf("import: %d %s", w.Code, w.Body.String())
	}
	if !sameJSON(t, w.Body.Bytes(), []byte(`{"created":["A","B"],"replaced":["C"]}`)) {
		t.Fatalf("answer: %s", w.Body.String())
	}
	for name, want := range map[string]string{"A": "value-of-a", "B": "value=with=equals", "C": "value-of-c"} {
		if v, ok := storedValue(t, f, name); !ok || v != want {
			t.Fatalf("%s: %q %v, want %q", name, v, ok, want)
		}
	}
	audit, err := f.s.AuditLog(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, a := range audit {
		if a.ActorName == "alice" && a.Outcome == "succeeded" {
			seen[a.Action]++
		}
	}
	for _, name := range []string{"A", "B", "C"} {
		if seen["secret_set:"+name] != 1 {
			t.Fatalf("audit rows %v, want one secret_set:%s by alice", seen, name)
		}
	}
}

// TestSecretImportRefusesABadBodyAndStoresNothing checks that every rule of the import is enforced by the agent, which
// stores nothing when one entry is bad.
func TestSecretImportRefusesABadBodyAndStoresNothing(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	tooMany := make([]api.SecretEntry, api.MaxSecretImport+1)
	for i := range tooMany {
		tooMany[i] = api.SecretEntry{Name: "S" + strconv.Itoa(i), Value: "v"}
	}
	for _, tc := range []struct {
		name, body string
	}{
		{"bad name", importBody(t, false, api.SecretEntry{Name: "GOOD", Value: "v"}, api.SecretEntry{Name: "1BAD", Value: "v"})},
		{"tunnel token", importBody(t, false, api.SecretEntry{Name: "A", Value: "v"}, api.SecretEntry{Name: "TUNNEL_TOKEN", Value: "v"})},
		{"duplicate", importBody(t, false, api.SecretEntry{Name: "A", Value: "v"}, api.SecretEntry{Name: "A", Value: "w"})},
		{"no entries", importBody(t, false)},
		{"too many", importBody(t, false, tooMany...)},
		{"unknown field", `{"secrets":[{"name":"A","value":"v"}],"extra":true}`},
		{"not a list", `{"secrets":"A=v"}`},
	} {
		if w := f.call("POST", "/secrets/import", apAdminToken, tc.body); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
	}
	if got, err := f.s.Secrets(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("stored %v after refused imports (%v)", got, err)
	}
}

// TestSecretImportRefusesARegistryPasswordNameAndStoresNothing checks that a name stored as a registry password is refused,
// in the dry run and in the import, and that the other entries are not stored.
func TestSecretImportRefusesARegistryPasswordNameAndStoresNothing(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	ctx := context.Background()
	if err := f.s.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: "GHCR_TOKEN", Registry: "ghcr.io"}, []byte("pull")); err != nil {
		t.Fatal(err)
	}
	const message = "GHCR_TOKEN is a registry password: use secret set --registry"
	for _, dry := range []bool{true, false} {
		w := f.call("POST", "/secrets/import", apAdminToken, importBody(t, dry, api.SecretEntry{Name: "A", Value: "v"}, api.SecretEntry{Name: "GHCR_TOKEN", Value: "plain"}))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), message) {
			t.Fatalf("dry %v: %d %s", dry, w.Code, w.Body.String())
		}
	}
	if _, ok := storedValue(t, f, "A"); ok {
		t.Fatal("A was stored with a refused registry name")
	}
	if v, _ := storedValue(t, f, "GHCR_TOKEN"); v != "pull" {
		t.Fatalf("the registry password changed: %q", v)
	}
}

// TestSecretImportOnAProtectedEnvironmentIsHeldUntilApproved checks the approval flow: an agent token's import is held, the
// stored plan and the approval list name the secrets but show no value, and approving stores them.
func TestSecretImportOnAProtectedEnvironmentIsHeldUntilApproved(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	const canary = "held-import-canary-value"
	h := f.hold("POST", "/secrets/import", apAgentAdmin, importBody(t, false,
		api.SecretEntry{Name: "DB_URL", Value: canary}, api.SecretEntry{Name: "API_KEY", Value: canary}))
	if !sameJSON(t, h.Plan, []byte(`{"action":"secret import","create":["API_KEY","DB_URL"],"replace":[]}`)) {
		t.Fatalf("plan: %s", h.Plan)
	}
	if strings.Contains(string(h.Plan), canary) {
		t.Fatalf("the plan shows a value: %s", h.Plan)
	}
	if _, ok := storedValue(t, f, "DB_URL"); ok {
		t.Fatal("the held import stored a secret")
	}
	list := f.call("GET", "/approvals", apReadToken, "")
	if !strings.Contains(list.Body.String(), `"summary":"secret import 2 secrets"`) || !strings.Contains(list.Body.String(), `"kind":"secret import"`) || strings.Contains(list.Body.String(), canary) {
		t.Fatalf("list: %s", list.Body.String())
	}
	if w := f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	if v, ok := storedValue(t, f, "DB_URL"); !ok || v != canary {
		t.Fatalf("approved DB_URL: %q %v", v, ok)
	}
	if v, ok := storedValue(t, f, "API_KEY"); !ok || v != canary {
		t.Fatalf("approved API_KEY: %q %v", v, ok)
	}
}

// TestHeldSecretImportIsRefusedWhenASecretAppearedSinceTheRequest checks that an approval is refused, and nothing is stored,
// when one of the names has been set since the request was held: the plan has changed.
func TestHeldSecretImportIsRefusedWhenASecretAppearedSinceTheRequest(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	h := f.hold("POST", "/secrets/import", apAgentAdmin, importBody(t, false, api.SecretEntry{Name: "A", Value: "v1"}, api.SecretEntry{Name: "B", Value: "v2"}))
	if err := f.s.PutSecret(context.Background(), state.Actor{ID: "admin"}, state.SecretInfo{Name: "B"}, []byte("someone-else")); err != nil {
		t.Fatal(err)
	}
	w := f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "the plan changed") {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	if _, ok := storedValue(t, f, "A"); ok {
		t.Fatal("A was stored by a refused approval")
	}
	if v, _ := storedValue(t, f, "B"); v != "someone-else" {
		t.Fatalf("B was overwritten: %q", v)
	}
}

// TestDeletingASecretNamedImportIsStillADelete checks that the route for a secret named import is told apart by its method:
// a delete of that secret is still a delete, in the approvals and in the store.
func TestDeletingASecretNamedImportIsStillADelete(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	ctx := context.Background()
	if err := f.s.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: "import"}, []byte("v")); err != nil {
		t.Fatal(err)
	}
	h := f.hold("DELETE", "/secrets/import", apAgentAdmin, `{}`)
	if !sameJSON(t, h.Plan, []byte(`{"action":"secret delete","name":"import","exists":true}`)) {
		t.Fatalf("plan: %s", h.Plan)
	}
	list := f.call("GET", "/approvals", apReadToken, "")
	if !strings.Contains(list.Body.String(), `"kind":"secret delete"`) || !strings.Contains(list.Body.String(), `"summary":"secret delete import"`) {
		t.Fatalf("list: %s", list.Body.String())
	}
	if w := f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	if _, _, err := f.s.Secret(ctx, "import"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("the secret named import was not deleted: %v", err)
	}
}

// TestSecretImportRouteIsPostOnly checks that the import route takes POST and only POST, so that a GET of the path is not
// taken for an import.
func TestSecretImportRouteIsPostOnly(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	r := httptest.NewRequest("GET", "/v1/projects/demo/environments/lab/secrets/import", nil)
	r.Header.Set("Authorization", "Bearer "+apAdminToken)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET secrets/import: %d %s", w.Code, w.Body.String())
	}
}
