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
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestLogsValidationBeforeDocker(t *testing.T) {
	c := Config{Project: "demo", Environment: "lab", Tokens: []Token{{Digest: Digest("bty.demo.lab." + strings.Repeat("a", 64)), Scope: "read"}}}
	r := httptest.NewRequest("GET", "/v1/projects/demo/environments/lab/apps/demo/logs?grep=%5B", nil)
	r.Header.Set("Authorization", "Bearer bty.demo.lab."+strings.Repeat("a", 64))
	w := httptest.NewRecorder()
	Handler(c, nil).ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
}
func TestStatsBeforeFirstSample(t *testing.T) {
	c := Config{Project: "demo", Environment: "lab", Tokens: []Token{{Digest: Digest("bty.demo.lab." + strings.Repeat("a", 64)), Scope: "read"}}}
	r := httptest.NewRequest("GET", "/v1/projects/demo/environments/lab/stats", nil)
	r.Header.Set("Authorization", "Bearer bty.demo.lab."+strings.Repeat("a", 64))
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

// TestLogSourcesMatchesReplicaByPrefix: the container id a response shows (its 12 characters) names a replica, and so does
// a prefix of 6 or more characters. A shorter one matches only exactly.
func TestLogSourcesMatchesReplicaByPrefix(t *testing.T) {
	pad := func(id string) string { return id + strings.Repeat("0", 64-len(id)) }
	web1 := api.Replica{TaskID: "task-web-1", ContainerID: pad("f77449eeec32"), Local: true}
	web2 := api.Replica{TaskID: "task-web-2", ContainerID: pad("a1b2c3d4e5f6"), Local: true}
	worker := api.Replica{TaskID: "task-worker-1", ContainerID: pad("9f8e7d6c5b4a"), Local: true}
	app := api.App{Roles: []api.Role{
		{Name: "web", Replicas: []api.Replica{web1, web2}},
		{Name: "worker", Replicas: []api.Replica{worker}},
	}}
	for _, tc := range []struct {
		name, role, replica string
		want                []string
		status              int
	}{
		{"task id", "", "task-web-1", []string{web1.ContainerID}, 200},
		{"full container id", "", web1.ContainerID, []string{web1.ContainerID}, 200},
		{"12-character container prefix", "", "f77449eeec32", []string{web1.ContainerID}, 200},
		{"6-character prefix", "", "a1b2c3", []string{web2.ContainerID}, 200},
		{"5-character prefix matches nothing", "", "f7744", nil, 404},
		{"prefix of a replica in another role", "worker", "a1b2c3", nil, 404},
		{"role filter and prefix", "web", "a1b2c3", []string{web2.ContainerID}, 200},
		{"prefix of a worker", "", "9f8e7d6c5b4a", []string{worker.ContainerID}, 200},
	} {
		ids, status, message := logSources(app, tc.role, tc.replica)
		if status != tc.status {
			t.Fatalf("%s: status %d %q, want %d", tc.name, status, message, tc.status)
		}
		if len(ids) != len(tc.want) {
			t.Fatalf("%s: got %+v, want containers %v", tc.name, ids, tc.want)
		}
		for i, id := range ids {
			if id.ContainerID != tc.want[i] {
				t.Fatalf("%s: got container %s, want %s", tc.name, id.ContainerID, tc.want[i])
			}
		}
	}
}

// TestLogSourcesRefusesAmbiguousPrefix: a prefix that names more than one replica of the app is refused, and a replica on
// another node counts, since the prefix would name it too. The role filter comes first, so it can tell them apart.
func TestLogSourcesRefusesAmbiguousPrefix(t *testing.T) {
	pad := func(id string) string { return id + strings.Repeat("0", 64-len(id)) }
	web := api.Replica{TaskID: "task-web", ContainerID: pad("f77449eeec32"), Local: true}
	worker := api.Replica{TaskID: "task-worker", ContainerID: pad("f77449ffff00"), Local: true}
	app := api.App{Roles: []api.Role{
		{Name: "web", Replicas: []api.Replica{web}},
		{Name: "worker", Replicas: []api.Replica{worker}},
	}}
	ids, status, message := logSources(app, "", "f77449")
	if status != 400 || message != `replica "f77449" matches several replicas; give more characters` || len(ids) != 0 {
		t.Fatalf("ambiguous prefix: %d %q %+v", status, message, ids)
	}
	if ids, status, message = logSources(app, "web", "f77449"); status != 200 || len(ids) != 1 || ids[0].ContainerID != web.ContainerID {
		t.Fatalf("role filter did not tell them apart: %d %q %+v", status, message, ids)
	}
	if ids, status, message = logSources(app, "", "f77449eeec"); status != 200 || len(ids) != 1 || ids[0].ContainerID != web.ContainerID {
		t.Fatalf("longer prefix: %d %q %+v", status, message, ids)
	}
	remote := api.App{Roles: []api.Role{
		{Name: "web", Replicas: []api.Replica{web}},
		{Name: "worker", Replicas: []api.Replica{{TaskID: "task-remote", ContainerID: pad("f77449aaaa00")}}},
	}}
	if _, status, message = logSources(remote, "", "f77449"); status != 400 {
		t.Fatalf("replica on another node not counted: %d %q", status, message)
	}
}

// TestLogSourcesWholeIDBeatsPrefix: a whole id names its replica even when it is also a prefix of another replica's id. A
// prefix counts as a prefix only when no whole id matches, so the prefix of both stays ambiguous.
func TestLogSourcesWholeIDBeatsPrefix(t *testing.T) {
	a := api.Replica{TaskID: "task-a", ContainerID: "f77449eeec32abcd", Local: true}
	b := api.Replica{TaskID: "task-b", ContainerID: "f77449eeec32abcd0123", Local: true}
	app := api.App{Roles: []api.Role{{Name: "web", Replicas: []api.Replica{a, b}}}}
	ids, status, message := logSources(app, "", a.ContainerID)
	if status != 200 || len(ids) != 1 || ids[0].ContainerID != a.ContainerID {
		t.Fatalf("whole id of a: %d %q %+v", status, message, ids)
	}
	if ids, status, message = logSources(app, "", "f77449eeec32"); status != 400 || len(ids) != 0 {
		t.Fatalf("prefix of both: %d %q %+v", status, message, ids)
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

func TestRedactRecordsProtectsTheCutBeforeGrep(t *testing.T) {
	secrets := []string{"complete-secret", "partial-secret"}
	rows := []logs.Record{
		{Message: "complete-secret useful partial" + logs.TruncationMarker, Truncated: true},
		{Message: "ordinary partial" + logs.TruncationMarker},
	}
	got := redactRecords(rows, secrets, nil)
	if len(got) != 2 || got[0].Message != "[REDACTED] useful [REDACTED]"+logs.TruncationMarker || got[1].Message != rows[1].Message {
		t.Fatalf("unexpected messages: %+v", got)
	}
	got = redactRecords(rows[:1], secrets, regexp.MustCompile("partial"))
	if len(got) != 0 {
		t.Fatalf("grep matched a secret fragment: %+v", got)
	}
	got = redactRecords(rows[:1], secrets, regexp.MustCompile(`\[truncated\]`))
	if len(got) != 1 || got[0].Message != "[REDACTED] useful [REDACTED]"+logs.TruncationMarker {
		t.Fatalf("grep missed the safe marked record: %+v", got)
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
	logical := api.App{Ref: api.AppRef{Kind: "image", Name: "api"}}
	ids[0].Role = "web"
	logicalSecrets, err := managedLogSecrets(ctx, d, source, logical, ids)
	if err != nil || len(logicalSecrets) < 2 {
		t.Fatalf("logical app did not redact secrets: %v", err)
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

func TestLogsOfAnotherEnvironmentsServiceAre404(t *testing.T) {
	d := &deploy.Controller{Foreign: func(context.Context) (deploy.Foreign, error) {
		return deploy.Foreign{Claims: map[state.Claim]bool{{Kind: state.ClaimService, Name: "foreign"}: true}}, nil
	}}
	w := httptest.NewRecorder()
	serveLogs(w, httptest.NewRequest("GET", "/logs", nil), inventoryDocker(t), "foreign", d, newLimits())
	if w.Code != 404 {
		t.Fatalf("logs: %d %s", w.Code, w.Body.String())
	}
}

type framedLogSource []byte

func (f framedLogSource) Open(context.Context, string, logs.OpenOptions) (io.ReadCloser, bool, error) {
	return io.NopCloser(strings.NewReader(string(f))), false, nil
}

func TestRedactRecordsJoinsSecretAcrossTimestampedFrames(t *testing.T) {
	stamp := "2026-01-01T00:00:01Z "
	secret := "distinct-first-distinct-second"
	half := len(secret) / 2
	filler := strings.Repeat("x", logs.MaxLineBytes)
	wire := append(dockerFrame(1, []byte(stamp+"safe "+secret[:half])), dockerFrame(1, []byte(stamp+secret[half:]+filler+"\n"))...)
	rows, err := logs.Tail(context.Background(), framedLogSource(wire), []logs.Identity{{ContainerID: "a"}}, logs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := redactRecords(rows, []string{secret}, nil)
	want := "safe [REDACTED]" + filler[:logs.MaxLineBytes-len(stamp)-len("safe ")-len(secret)] + logs.TruncationMarker
	if len(got) != 1 || got[0].Message != want || strings.Contains(got[0].Message, secret[:half]) || strings.Contains(got[0].Message, secret[half:]) {
		t.Fatal("secret or filler differs")
	}
}
