package agent

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"berthy/internal/scope"
)

func TestNewTokenHasTheIssuedFormatAndItsDigest(t *testing.T) {
	id := scope.ID{Project: "shop", Environment: "p5"}
	token, digest, err := NewToken(id)
	if err != nil {
		t.Fatal(err)
	}
	if !tokenPattern(id).MatchString(token) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(digest) || digest != Digest(token) {
		t.Fatalf("token %q, digest %q", token, digest)
	}
	other, _, err := NewToken(id)
	if err != nil || other == token {
		t.Fatalf("two tokens are equal: %v", err)
	}
}

func TestATokenNamesItsProjectAndEnvironment(t *testing.T) {
	id := scope.ID{Project: "shop", Environment: "prod"}
	token, _, err := NewToken(id)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := "bty.shop.prod."
	if !strings.HasPrefix(token, wantPrefix) || len(token) != len(wantPrefix)+64 || !tokenPattern(id).MatchString(token) {
		t.Fatalf("token %q", token)
	}
}

func TestATokenOfTheOldShapeIs401(t *testing.T) {
	old := "bty.prod." + strings.Repeat("a", 64)
	h := Handler(Config{Project: "demo", Environment: "prod", Tokens: []Token{{Digest: Digest(old), Scope: "read"}}}, nil)
	r := httptest.NewRequest(http.MethodGet, "/v1/projects/demo/environments/prod/agent", nil)
	r.Header.Set("Authorization", "Bearer "+old)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("old shape: got %d want 401", w.Code)
	}
}
