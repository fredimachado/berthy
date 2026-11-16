package agent

import (
	"berthy/internal/metrics"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
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

func TestAgentInfoSaysWhetherTheEnvironmentIsProtected(t *testing.T) {
	token := "bty.prod." + strings.Repeat("a", 64)
	for _, protected := range []bool{true, false} {
		h := Handler(Config{Environment: "prod", Protected: protected, Tokens: []Token{{Digest: Digest(token), Scope: "read"}}}, nil)
		r := httptest.NewRequest(http.MethodGet, "/v1/environments/prod/agent", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var info struct {
			Environment string `json:"environment"`
			Protected   bool   `json:"protected"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil || info.Environment != "prod" || info.Protected != protected {
			t.Errorf("protected=%v: %d %s", protected, w.Code, w.Body.String())
		}
	}
}

func TestStatsRouteRequiresAuthentication(t *testing.T) {
	c := Config{Environment: "lab", Tokens: []Token{{Digest: Digest("bty.lab." + strings.Repeat("a", 64)), Scope: "read"}}}
	req := httptest.NewRequest("GET", "/v1/environments/lab/stats", nil)
	w := httptest.NewRecorder()
	Handler(c, nil).ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("got %d", w.Code)
	}
	req = httptest.NewRequest("GET", "/v1/environments/lab/stats", nil)
	req.Header.Set("Authorization", "Bearer bty.lab."+strings.Repeat("a", 64))
	w = httptest.NewRecorder()
	Handler(c, nil).ServeHTTP(w, req)
	if w.Code != 503 {
		t.Fatalf("stats: %d %s", w.Code, w.Body.String())
	}
}
func TestStatsWatchStreamsInitialSample(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := metrics.New(metrics.NewHostReader(), nilDocker{})
	go c.Run(ctx)
	deadline := time.Now().Add(time.Second)
	for {
		if _, ok := c.Latest(); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no sample")
		}
		runtime.Gosched()
	}
	cfg := Config{Environment: "lab", Tokens: []Token{{Digest: Digest("bty.lab." + strings.Repeat("a", 64)), Scope: "read"}}}
	req := httptest.NewRequest("GET", "/v1/environments/lab/stats?watch=1", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer bty.lab."+strings.Repeat("a", 64))
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { HandlerWithMetrics(cfg, nil, c).ServeHTTP(w, req); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if !strings.Contains(w.Body.String(), "data: ") {
		t.Fatalf("missing event: %s", w.Body.String())
	}
}

type nilDocker struct{}

func (nilDocker) Containers(context.Context) ([]string, error) { return nil, nil }
func (nilDocker) Stats(context.Context, string) (metrics.RawContainer, error) {
	return metrics.RawContainer{}, nil
}
