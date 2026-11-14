package agent

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScopeGuard(t *testing.T) {
	token := "bty.lab." + strings.Repeat("a", 64)
	c := Config{Environment: "lab", Tokens: []Token{{Digest: strings.ToUpper(Digest(token)), Scope: "read"}}}
	called := false
	h := authorize(c, "deploy", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	r := httptest.NewRequest("GET", "/v1/environments/lab/apps", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 || called {
		t.Fatalf("status %d called %v", w.Code, called)
	}
}
func TestStrictConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.json")
	for _, s := range []string{`{"environment":"lab","extra":1}`, `{} {}`} {
		if err := os.WriteFile(p, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Errorf("accepted %s", s)
		}
	}
}
