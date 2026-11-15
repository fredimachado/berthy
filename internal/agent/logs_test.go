package agent

import (
	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/logs"
	"berthy/internal/metrics"
	"berthy/internal/state"
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
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
func TestFollowSourceDeadlineCode(t *testing.T) {
	if got := logsErrorCode(fmt.Errorf("source: %w", context.DeadlineExceeded)); got != "docker_deadline" {
		t.Fatalf("got %q", got)
	}
	if got := logsErrorCode(errors.New("source unavailable")); got != "logs_unavailable" {
		t.Fatalf("got %q", got)
	}
}
func TestParseSince(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Time
	}{
		{"10m", now.Add(-10 * time.Minute)},
		{"2025-12-31T23:00:00Z", time.Date(2025, 12, 31, 23, 0, 0, 0, time.UTC)},
		{"2025-12-31T23:00:00.5Z", time.Date(2025, 12, 31, 23, 0, 0, 500000000, time.UTC)},
	} {
		got, err := parseSince(tc.value, now)
		if err != nil || !got.Equal(tc.want) {
			t.Errorf("%q: got %v, %v", tc.value, got, err)
		}
	}
	for _, value := range []string{"0s", "-5m", "abc", ""} {
		if _, err := parseSince(value, now); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
}
func TestLogSourcesSkipsNonLocalReplicas(t *testing.T) {
	restarts := func(n int) *int { return &n }
	app := api.App{Roles: []api.Role{{Name: "web", Replicas: []api.Replica{
		{TaskID: "task-local", ContainerID: "c-local", RestartCount: restarts(0), Local: true},
		{TaskID: "task-remote", ContainerID: "c-remote"},
		{ContainerID: "c-compose", RestartCount: restarts(0), Local: true},
		{ContainerID: "c-compose-remote"},
		// RestartCount alone does not make a replica local: only Local decides.
		{TaskID: "task-stale", ContainerID: "c-stale", RestartCount: restarts(3)},
	}}}}
	ids, status, message := logSources(app, "", "")
	if status != 200 || len(ids) != 2 || ids[0].ContainerID != "c-local" || ids[1].ContainerID != "c-compose" {
		t.Fatalf("all replicas: %d %q %+v", status, message, ids)
	}
	ids, status, message = logSources(app, "", "task-stale")
	if status != 404 || message != "replica not on this node" || len(ids) != 0 {
		t.Fatalf("stale replica with restart count: %d %q %+v", status, message, ids)
	}
	ids, status, message = logSources(app, "", "task-remote")
	if status != 404 || message != "replica not on this node" || len(ids) != 0 {
		t.Fatalf("remote replica: %d %q %+v", status, message, ids)
	}
	ids, status, message = logSources(app, "web", "task-local")
	if status != 200 || len(ids) != 1 || ids[0].Replica != "task-local" {
		t.Fatalf("local replica: %d %q %+v", status, message, ids)
	}
	ids, status, message = logSources(app, "", "c-compose-remote")
	if status != 404 || message != "replica not on this node" || len(ids) != 0 {
		t.Fatalf("remote compose replica: %d %q %+v", status, message, ids)
	}
	remoteOnly := api.App{Roles: []api.Role{{Name: "web", Replicas: []api.Replica{{TaskID: "task-x", ContainerID: "c-x"}}}}}
	if ids, status, message = logSources(remoteOnly, "", ""); status != 200 || len(ids) != 0 || message != "" {
		t.Fatalf("role-only all remote: %d %q %+v", status, message, ids)
	}
	if _, status, message = logSources(app, "missing", ""); status != 404 || message != "role or replica not found" {
		t.Fatalf("missing role: %d %q", status, message)
	}
}

func TestRedactionBeforeFiltering(t *testing.T) {
	rows := []logs.Record{{Message: "old-value and new-value"}, {Message: "public"}}
	got := redactRecords(rows, []string{"old-value", "new-value"}, regexp.MustCompile("old-value"))
	if len(got) != 0 {
		t.Fatal("matched secret before redaction")
	}
	got = redactRecords(rows, []string{"old-value", "new-value"}, regexp.MustCompile("REDACTED"))
	if len(got) != 1 || strings.Contains(got[0].Message, "value") {
		t.Fatalf("leaked value: %+v", got)
	}
}

type fakeManagedLogs struct {
	service engine.ServiceSnapshot
	env     []string
	envErr  error
}

func (f fakeManagedLogs) InspectService(context.Context, string) (engine.ServiceSnapshot, error) {
	return f.service, nil
}
func (f fakeManagedLogs) ContainerEnv(context.Context, string) ([]string, error) {
	return f.env, f.envErr
}
func TestManagedLogSecretsIncludeRotatedContainerValues(t *testing.T) {
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
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "r", ActorID: "a", Kind: "deploy", App: "api", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	m := []byte(`{"project":"demo","network":"demo","apps":{"api":{"kind":"image","image":"alpine","roles":{"web":{}},"env":{"secret":["PASSWORD"]}}}}`)
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "api", Manifest: m, Bindings: []state.Binding{{App: "api", Role: "web", Name: "demo-api-web", ServiceID: "service1"}}}}); err != nil {
		t.Fatal(err)
	}
	if err = s.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: "PASSWORD"}, []byte("new-value")); err != nil {
		t.Fatal(err)
	}
	d := &deploy.Controller{Store: s}
	app := api.App{Ref: api.AppRef{Kind: "swarm", Name: "demo-api-web"}}
	source := fakeManagedLogs{service: engine.ServiceSnapshot{ID: "service1", Spec: []byte(`{}`)}, env: []string{"PASSWORD=old-value"}}
	ids := []logs.Identity{{ContainerID: "c1"}}
	secret, err := managedLogSecrets(ctx, d, source, app, ids)
	if err != nil {
		t.Fatal(err)
	}
	redacted := redactRecords([]logs.Record{{Message: "old-value new-value"}}, secret, nil)
	if strings.Contains(redacted[0].Message, "value") {
		t.Fatal("rotated secret leaked")
	}
	source.envErr = errors.New("private failure")
	if _, err = managedLogSecrets(ctx, d, source, app, ids); err == nil {
		t.Fatal("unreadable managed container accepted")
	}
	source.envErr = nil
	source.service.ID = "wrong"
	if _, err = managedLogSecrets(ctx, d, source, app, ids); err == nil {
		t.Fatal("mismatched managed identity accepted")
	}
}
