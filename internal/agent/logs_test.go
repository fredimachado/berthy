package agent

import (
	"berthy/internal/metrics"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLogsValidationBeforeDocker(t *testing.T) {
	c := Config{Environment: "lab", Tokens: []Token{{Digest: Digest("bty.lab." + strings.Repeat("a", 64)), Scope: "read"}}}
	r := httptest.NewRequest("GET", "/v1/environments/lab/apps/demo/logs?grep=%5B", nil)
	r.Header.Set("Authorization", "Bearer bty.lab."+strings.Repeat("a", 64))
	w := httptest.NewRecorder()
	Handler(c, nil).ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
}
func TestStatsBeforeFirstSample(t *testing.T) {
	c := Config{Environment: "lab", Tokens: []Token{{Digest: Digest("bty.lab." + strings.Repeat("a", 64)), Scope: "read"}}}
	r := httptest.NewRequest("GET", "/v1/environments/lab/stats", nil)
	r.Header.Set("Authorization", "Bearer bty.lab."+strings.Repeat("a", 64))
	w := httptest.NewRecorder()
	Handler(c, nil).ServeHTTP(w, r)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "sample_not_ready") {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
}
func TestPartialMountsKeepRootHealthy(t *testing.T) {
	mounts := []metrics.Mount{{Path: "/", Total: 100, Available: 50}}
	if diskStatus(mounts, errors.New("optional mount denied")) != "partial" {
		t.Fatal("expected partial")
	}
	check := rootDiskCheck(mounts)
	if !check.OK {
		t.Fatalf("root incorrectly failed: %+v", check)
	}
	if rootDiskCheck([]metrics.Mount{{Path: "/", Total: 100, Available: 5}}).OK {
		t.Fatal("full root must fail")
	}
}
