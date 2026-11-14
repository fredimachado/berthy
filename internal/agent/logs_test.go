package agent

import (
	"berthy/internal/metrics"
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
func TestDockerErrorCodesAndDuplicateSamples(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{{context.DeadlineExceeded, "docker_deadline"}, {errors.New("swarm_manager_unavailable"), "swarm_manager_unavailable"}} {
		if got := dockerErrorCode(tc.err); got != tc.want {
			t.Fatalf("got %s", got)
		}
	}
	t1 := time.Now()
	if shouldEmitSample(t1, t1) || shouldEmitSample(t1.Add(-time.Second), t1) || !shouldEmitSample(t1.Add(time.Second), t1) {
		t.Fatal("sample order")
	}
}
