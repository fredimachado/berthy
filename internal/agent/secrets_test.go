package agent

import (
	"context"
	"database/sql"
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

func TestTunnelTokenSecretIsReservedForIngress(t *testing.T) {
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
	admin := "bty.lab." + strings.Repeat("d", 64)
	cfg := Config{Project: "demo", Environment: "lab", Tokens: []Token{{Digest: Digest(admin), Scope: "admin"}}}
	h := NewDeploymentHandler(cfg, nil, nil, &deploy.Controller{Store: s})
	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+admin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	// A positive control shows the refusals below come from the reserved name, not from the request shape.
	if w := call("POST", "/v1/projects/demo/environments/lab/secrets", `{"name":"APP_KEY","value":"v"}`); w.Code != http.StatusOK {
		t.Fatalf("ordinary secret refused: %d %s", w.Code, w.Body.String())
	}
	for _, tc := range []struct{ name, method, path, body string }{
		{"set", "POST", "/v1/projects/demo/environments/lab/secrets", `{"name":"TUNNEL_TOKEN","value":"v"}`},
		{"dry run set", "POST", "/v1/projects/demo/environments/lab/secrets", `{"name":"TUNNEL_TOKEN","value":"v","dry_run":true}`},
		{"delete", "DELETE", "/v1/projects/demo/environments/lab/secrets/TUNNEL_TOKEN", `{}`},
	} {
		w := call(tc.method, tc.path, tc.body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "TUNNEL_TOKEN is reserved for managed ingress") {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
	}
	if _, _, err := s.Secret(context.Background(), "TUNNEL_TOKEN"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("reserved secret stored: %v", err)
	}
}
