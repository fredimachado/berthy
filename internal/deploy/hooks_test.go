package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

type hookEngine struct {
	Engine
	calls       []string
	specs       []json.RawMessage
	exits       []int
	logs        [][]byte
	store       *state.Store
	op          string
	failStartAt int
	names       []string        // container name of every command, in order
	limits      []time.Duration // time each command had left when it was created
	waitErrAt   map[int]error   // the n-th wait (0-based) fails with this error
	waits       int
}

func (f *hookEngine) CreateContainer(ctx context.Context, name string, spec json.RawMessage) (string, error) {
	steps, err := f.store.Intents(ctx, f.op)
	if err != nil || len(steps) == 0 {
		return "", errors.New("missing durable intent")
	}
	f.calls = append(f.calls, "create")
	f.specs = append(f.specs, spec)
	f.names = append(f.names, name)
	if d, ok := ctx.Deadline(); ok {
		f.limits = append(f.limits, time.Until(d))
	}
	return name, nil
}
func (f *hookEngine) StartContainer(ctx context.Context, id string) error {
	steps, _ := f.store.Intents(ctx, f.op)
	if steps[len(steps)-1].Phase != "start_pending" {
		return errors.New("missing durable start")
	}
	f.calls = append(f.calls, "start")
	if f.failStartAt == len(f.specs) {
		return errors.New("lost response")
	}
	return nil
}
func (f *hookEngine) WaitContainer(ctx context.Context, id string) (int, error) {
	f.calls = append(f.calls, "wait")
	i := f.waits
	f.waits++
	if err := f.waitErrAt[i]; err != nil {
		return 0, err
	}
	n := f.exits[0]
	f.exits = f.exits[1:]
	return n, nil
}
func (f *hookEngine) ContainerLogs(ctx context.Context, id string) ([]byte, error) {
	f.calls = append(f.calls, "logs")
	v := f.logs[0]
	f.logs = f.logs[1:]
	return v, nil
}
func (f *hookEngine) InspectContainer(context.Context, string) (engine.ContainerSnapshot, error) {
	return engine.ContainerSnapshot{}, errors.New("missing")
}
func (f *hookEngine) StopContainer(context.Context, string, int) error { return nil }
func hookFixture(t *testing.T) (*Controller, *hookEngine, string, preparedPlan) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir, key, "stage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	op, _, err := s.Admit(context.Background(), state.Admission{RequestID: "req", ActorID: "actor", Kind: "deploy", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	f := &hookEngine{store: s, op: op.ID}
	c := &Controller{Engine: f, Store: s, Environment: "stage"}
	p := preparedPlan{Image: "registry/app@sha256:pinned", NetworkID: "overlay", Env: []string{"TOKEN=secret"}, Request: Request{App: "app", Manifest: manifest.Manifest{Project: "proj", Apps: map[string]manifest.App{"app": {}}}}}
	return c, f, op.ID, p
}
func TestHooksAndFailedMigrationSampleAfter(t *testing.T) {
	c, f, id, p := hookFixture(t)
	a := p.Request.Manifest.Apps["app"]
	a.Hooks.PreDeploy = []manifest.Hook{{Run: []string{"pre", "one arg"}}}
	a.Migrations = &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}}
	p.Request.Manifest.Apps["app"] = a
	f.exits = []int{0, 0, 2, 0}
	f.logs = [][]byte{[]byte("v1\n"), []byte("v2\n")}
	before, after, err := c.runPre(context.Background(), id, p)
	if err == nil || before != "v1" || after != "v2" {
		t.Fatalf("versions %q %q error %v", before, after, err)
	}
	if len(f.specs) != 4 || !strings.Contains(string(f.specs[0]), `"one arg"`) || !strings.Contains(string(f.specs[0]), `"overlay"`) {
		t.Fatalf("specs %s", f.specs)
	}
}
func TestMigrationVersionRejectsSecret(t *testing.T) {
	c, f, id, p := hookFixture(t)
	a := p.Request.Manifest.Apps["app"]
	a.Migrations = &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}}
	a.Env.Secret = []manifest.SecretVar{{Var: "TOKEN", Secret: "TOKEN"}}
	p.Request.Manifest.Apps["app"] = a
	f.exits = []int{0}
	f.logs = [][]byte{[]byte("secret\n")}
	_, _, err := c.runPre(context.Background(), id, p)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("secret output was accepted or disclosed")
	}
	if len(f.specs) != 1 {
		t.Fatal("apply ran after invalid version")
	}
}
func TestUncertainMigrationDoesNotRunVersionWhileUnconfirmed(t *testing.T) {
	c, f, id, p := hookFixture(t)
	a := p.Request.Manifest.Apps["app"]
	a.Migrations = &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}}
	p.Request.Manifest.Apps["app"] = a
	f.exits = []int{0}
	f.logs = [][]byte{[]byte("v1\n")}
	f.failStartAt = 2
	_, _, err := c.runPre(context.Background(), id, p)
	if err == nil || len(f.specs) != 2 {
		t.Fatalf("uncertain migration ran further commands: %v %d", err, len(f.specs))
	}
}
func TestOneOffRunsExactArgv(t *testing.T) {
	c, f, id, p := hookFixture(t)
	f.exits = []int{0}
	if _, err := c.runOneOff(context.Background(), id, p, 1, "pre_hook", []string{"/tool", "two words"}, false, time.Minute); err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Entrypoint []string
		Cmd        []string
	}
	if err := json.Unmarshal(f.specs[0], &spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Entrypoint) != 1 || spec.Entrypoint[0] != "/tool" || len(spec.Cmd) != 1 || spec.Cmd[0] != "two words" {
		t.Fatalf("entrypoint and args: %+v", spec)
	}
}
func TestSchemaVersionIsJournaledWithOneOff(t *testing.T) {
	c, f, id, p := hookFixture(t)
	f.exits = []int{0}
	f.logs = [][]byte{[]byte("v7\n")}
	v, err := c.runOneOff(context.Background(), id, p, 10000, "schema_before", []string{"version"}, true, time.Minute)
	if err != nil || v != "v7" {
		t.Fatalf("version %q: %v", v, err)
	}
	steps, err := c.Store.Intents(context.Background(), id)
	if err != nil || len(steps) != 1 || !strings.Contains(string(steps[0].Receipt), `"version":"v7"`) {
		t.Fatalf("receipt %v: %v", steps, err)
	}
}

// fastRetries shortens the wait between attempts of a retried command.
func fastRetries(t *testing.T) {
	t.Helper()
	old := oneOffRetryDelay
	oneOffRetryDelay = time.Millisecond
	t.Cleanup(func() { oneOffRetryDelay = old })
}

func intPtr(n int) *int { return &n }

func oneOffSteps(t *testing.T, c *Controller, id string) []state.Intent {
	t.Helper()
	steps, err := c.Store.Intents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return steps
}

func TestMigrationApplyRetriesUntilItSucceeds(t *testing.T) {
	fastRetries(t)
	c, f, id, p := hookFixture(t)
	a := p.Request.Manifest.Apps["app"]
	a.Migrations = &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}, Retries: intPtr(2)}
	p.Request.Manifest.Apps["app"] = a
	f.exits = []int{0, 1, 1, 0, 0}
	f.logs = [][]byte{[]byte("v1\n"), []byte("v2\n")}
	before, after, err := c.runPre(context.Background(), id, p)
	if err != nil || before != "v1" || after != "v2" {
		t.Fatalf("versions %q %q error %v", before, after, err)
	}
	if len(f.specs) != 5 {
		t.Fatalf("%d containers, want version, three applies, version", len(f.specs))
	}
	// Every attempt has its own container and its own journal entry.
	names := map[string]bool{}
	ordinals := map[int]bool{}
	applies := 0
	for _, s := range oneOffSteps(t, c, id) {
		names[s.Name], ordinals[s.Ordinal] = true, true
		if s.Role == "migration_apply" {
			applies++
		}
	}
	if len(names) != 5 || len(ordinals) != 5 || applies != 3 {
		t.Fatalf("names %v ordinals %v applies %d", names, ordinals, applies)
	}
}

func TestMigrationApplyWithoutRetriesRunsOnce(t *testing.T) {
	fastRetries(t)
	c, f, id, p := hookFixture(t)
	a := p.Request.Manifest.Apps["app"]
	a.Migrations = &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}}
	p.Request.Manifest.Apps["app"] = a
	f.exits = []int{0, 2, 0}
	f.logs = [][]byte{[]byte("v1\n"), []byte("v1\n")}
	_, _, err := c.runPre(context.Background(), id, p)
	d := failureDiagnostics(err)
	if err == nil || len(f.specs) != 3 || len(d) != 1 || d[0].Attempts != 0 {
		t.Fatalf("error %v, %d containers, diagnostics %+v", err, len(f.specs), d)
	}
	if got := failureReason(d, "x"); got != "migration apply exited with code 2" {
		t.Fatalf("reason %q", got)
	}
}

func TestMigrationApplyRetriesAreBoundedAndTheReasonCountsAttempts(t *testing.T) {
	fastRetries(t)
	c, f, id, p := hookFixture(t)
	a := p.Request.Manifest.Apps["app"]
	a.Migrations = &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}, Retries: intPtr(1)}
	p.Request.Manifest.Apps["app"] = a
	f.exits = []int{0, 2, 3, 0}
	f.logs = [][]byte{[]byte("v1\n"), []byte("v1\n")}
	before, after, err := c.runPre(context.Background(), id, p)
	if err == nil || before != "v1" || after != "v1" {
		t.Fatalf("versions %q %q error %v", before, after, err)
	}
	if len(f.specs) != 4 {
		t.Fatalf("%d containers, want version, two applies, version", len(f.specs))
	}
	d := failureDiagnostics(err)
	if len(d) != 1 || d[0].Attempts != 2 || d[0].ExitCode == nil || *d[0].ExitCode != 3 {
		t.Fatalf("the last attempt evidence was not kept: %+v", d)
	}
	if got := failureReason(d, "x"); got != "migration apply exited with code 3 (after 2 attempts)" {
		t.Fatalf("reason %q", got)
	}
}

func TestTimedOutCommandIsNeverRetried(t *testing.T) {
	fastRetries(t)
	c, f, id, p := hookFixture(t)
	a := p.Request.Manifest.Apps["app"]
	a.Migrations = &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}, Retries: intPtr(3), Timeout: "10s"}
	p.Request.Manifest.Apps["app"] = a
	f.exits = []int{0}
	f.logs = [][]byte{[]byte("v1\n")}
	f.waitErrAt = map[int]error{1: context.DeadlineExceeded}
	_, _, err := c.runPre(context.Background(), id, p)
	if !errors.Is(err, ErrOneOffUncertain) || len(f.specs) != 2 {
		t.Fatalf("error %v after %d containers: a command that may still run was started again", err, len(f.specs))
	}
	if got := failureReason(failureDiagnostics(err), "x"); got != "migration apply timed out" {
		t.Fatalf("reason %q", got)
	}
}

func TestTimeoutAfterARetriedFailureIsNotRetriedAgain(t *testing.T) {
	fastRetries(t)
	c, f, id, p := hookFixture(t)
	a := p.Request.Manifest.Apps["app"]
	a.Migrations = &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}, Retries: intPtr(3)}
	p.Request.Manifest.Apps["app"] = a
	f.exits = []int{0, 1}
	f.logs = [][]byte{[]byte("v1\n")}
	f.waitErrAt = map[int]error{2: context.DeadlineExceeded}
	_, _, err := c.runPre(context.Background(), id, p)
	if !errors.Is(err, ErrOneOffUncertain) || len(f.specs) != 3 {
		t.Fatalf("error %v after %d containers", err, len(f.specs))
	}
	if got := failureReason(failureDiagnostics(err), "x"); got != "migration apply timed out (after 2 attempts)" {
		t.Fatalf("reason %q", got)
	}
}

func TestVersionCommandIsNeverRetried(t *testing.T) {
	fastRetries(t)
	c, f, id, p := hookFixture(t)
	a := p.Request.Manifest.Apps["app"]
	a.Migrations = &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}, Retries: intPtr(3)}
	p.Request.Manifest.Apps["app"] = a
	f.exits = []int{1}
	if _, _, err := c.runPre(context.Background(), id, p); err == nil || len(f.specs) != 1 {
		t.Fatalf("error %v after %d containers", err, len(f.specs))
	}
}

func TestRevertIsNeverRetried(t *testing.T) {
	fastRetries(t)
	c, f, id, p := hookFixture(t)
	a := p.Request.Manifest.Apps["app"]
	a.Migrations = &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}, Revert: []string{"down", "{target}"}, Retries: intPtr(3)}
	p.Request.Manifest.Apps["app"] = a
	f.exits = []int{0, 1, 0}
	f.logs = [][]byte{[]byte("v2\n"), []byte("v2\n")}
	_, _, err := c.runRevert(context.Background(), id, schemaRevert{Prepared: p, Target: "v1", Revert: []string{"down", "v1"}, Version: []string{"version"}, Timeout: time.Minute})
	if err == nil || len(f.specs) != 3 {
		t.Fatalf("error %v after %d containers: revert once, and the version commands around it", err, len(f.specs))
	}
}

func TestHooksRetryInTheirOwnContainersAndKeepTheirOrder(t *testing.T) {
	fastRetries(t)
	c, f, id, p := hookFixture(t)
	a := p.Request.Manifest.Apps["app"]
	a.Hooks.PreDeploy = []manifest.Hook{{Run: []string{"warm"}, Retries: intPtr(2)}, {Run: []string{"second"}}}
	p.Request.Manifest.Apps["app"] = a
	f.exits = []int{1, 0, 0}
	if _, _, err := c.runPre(context.Background(), id, p); err != nil {
		t.Fatal(err)
	}
	want := []string{"berthy-" + id + "-10000", "berthy-" + id + "-10001", "berthy-" + id + "-10010"}
	if strings.Join(f.names, ",") != strings.Join(want, ",") {
		t.Fatalf("containers %v, want %v", f.names, want)
	}
}

func TestPostHookRetriesAndReasonNamesTheCommand(t *testing.T) {
	fastRetries(t)
	c, f, id, p := hookFixture(t)
	a := p.Request.Manifest.Apps["app"]
	a.Hooks.PostDeploy = []manifest.Hook{{Run: []string{"ok"}}, {Run: []string{"flaky"}, Retries: intPtr(1)}}
	p.Request.Manifest.Apps["app"] = a
	f.exits = []int{0, 4, 5}
	err := c.runPost(context.Background(), id, p)
	d := failureDiagnostics(err)
	if err == nil || len(d) != 1 {
		t.Fatalf("error %v diagnostics %+v", err, d)
	}
	if got := failureReason(d, "x"); got != "post-deploy command 2 exited with code 5 (after 2 attempts)" {
		t.Fatalf("reason %q", got)
	}
	if want := "berthy-" + id + "-20011"; f.names[len(f.names)-1] != want {
		t.Fatalf("containers %v", f.names)
	}
}

func TestConfiguredTimeoutLimitsEveryAttempt(t *testing.T) {
	c, f, id, p := hookFixture(t)
	a := p.Request.Manifest.Apps["app"]
	a.Hooks.PreDeploy = []manifest.Hook{{Run: []string{"short"}, Timeout: "90s"}, {Run: []string{"default"}}}
	a.Migrations = &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}, Timeout: "30m"}
	p.Request.Manifest.Apps["app"] = a
	f.exits = []int{0, 0, 0, 0, 0}
	f.logs = [][]byte{[]byte("v1\n"), []byte("v1\n")}
	if _, _, err := c.runPre(context.Background(), id, p); err != nil {
		t.Fatal(err)
	}
	within := func(got, limit time.Duration) bool { return got > limit-5*time.Second && got <= limit }
	if len(f.limits) != 5 || !within(f.limits[0], 90*time.Second) || !within(f.limits[1], 5*time.Minute) {
		t.Fatalf("hook limits %v", f.limits)
	}
	for _, got := range f.limits[2:] {
		if !within(got, 30*time.Minute) {
			t.Fatalf("migration limits %v", f.limits[2:])
		}
	}
}

// recoveringEngine reports every command container as still running until it is stopped.
type recoveringEngine struct {
	*hookEngine
	stopped map[string]bool
}

func (r *recoveringEngine) InspectContainer(_ context.Context, name string) (engine.ContainerSnapshot, error) {
	for _, n := range r.names {
		if n == name {
			return engine.ContainerSnapshot{ID: name, Running: !r.stopped[name], Labels: map[string]string{"berthy.environment": "stage", "berthy.operation": r.op, "berthy.oneoff": r.roleOf(name)}}, nil
		}
	}
	return engine.ContainerSnapshot{}, &engine.StatusError{Code: 404}
}
func (r *recoveringEngine) roleOf(name string) string {
	steps, _ := r.store.Intents(context.Background(), r.op)
	for _, s := range steps {
		if s.Name == name {
			return s.Role
		}
	}
	return ""
}
func (r *recoveringEngine) StopContainer(_ context.Context, name string, _ int) error {
	r.stopped[name] = true
	return nil
}

func TestRecoveryStopsTheContainerOfEveryAttempt(t *testing.T) {
	fastRetries(t)
	c, f, id, p := hookFixture(t)
	a := p.Request.Manifest.Apps["app"]
	a.Migrations = &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}, Retries: intPtr(2)}
	p.Request.Manifest.Apps["app"] = a
	f.exits = []int{0, 1, 1, 1, 0}
	f.logs = [][]byte{[]byte("v1\n"), []byte("v1\n")}
	if _, _, err := c.runPre(context.Background(), id, p); err == nil {
		t.Fatal("exhausted retries succeeded")
	}
	r := &recoveringEngine{hookEngine: f, stopped: map[string]bool{}}
	c.Engine = r
	if err := c.recoverHooks(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if len(r.stopped) != 5 {
		t.Fatalf("recovery stopped %v, want both version commands and all three apply attempts", r.stopped)
	}
}

// TestOneOffContainersGetTheIdentityWithAnEmptyRole: a hook's container belongs to the app, not a role, so BERTHY_ROLE is
// set and empty. It keeps the app's own variables too.
func TestOneOffContainersGetTheIdentityWithAnEmptyRole(t *testing.T) {
	c, f, id, p := hookFixture(t)
	p.Identity = identity{Environment: "stage", Project: "proj", App: "app", Digest: "sha256:pinned"}
	a := p.Request.Manifest.Apps["app"]
	a.Hooks.PreDeploy = []manifest.Hook{{Run: []string{"pre"}}}
	p.Request.Manifest.Apps["app"] = a
	f.exits = []int{0}
	if _, _, err := c.runPre(context.Background(), id, p); err != nil {
		t.Fatal(err)
	}
	if len(f.specs) != 1 {
		t.Fatalf("%d one-off containers", len(f.specs))
	}
	var spec struct{ Env []string }
	if err := json.Unmarshal(f.specs[0], &spec); err != nil {
		t.Fatal(err)
	}
	want := []string{"BERTHY_APP=app", "BERTHY_ENVIRONMENT=stage", "BERTHY_IMAGE_DIGEST=sha256:pinned", "BERTHY_PROJECT=proj", "BERTHY_ROLE=", "BERTHY_VERSION=", "TOKEN=secret"}
	if !reflect.DeepEqual(spec.Env, want) {
		t.Fatalf("one-off env %q, want %q", spec.Env, want)
	}
}
