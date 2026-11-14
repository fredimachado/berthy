package agent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGuardRejectsWrongEnvironmentBeforeDocker(t *testing.T) {
	h := Handler(Config{Environment: "prod", Tokens: []Token{{Digest: Digest("bty.prod." + strings.Repeat("a", 64)), Scope: "read"}}}, nil)
	for _, tc := range []struct {
		path, token string
		want        int
	}{{"/v1/environments/dev/apps", "bty.prod." + strings.Repeat("a", 64), 401}, {"/v1/environments/prod/apps", "bad", 401}, {"/v1/environments/prod/agent", "bty.prod." + strings.Repeat("a", 64), 200}} {
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		r.Header.Set("Authorization", "Bearer "+tc.token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s: got %d want %d", tc.path, w.Code, tc.want)
		}
	}
}
