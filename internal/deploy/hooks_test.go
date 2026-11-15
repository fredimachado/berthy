package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
}

func (f *hookEngine) CreateContainer(ctx context.Context, name string, spec json.RawMessage) (string, error) {
	steps, err := f.store.Intents(ctx, f.op)
	if err != nil || len(steps) == 0 {
		return "", errors.New("missing durable intent")
	}
	f.calls = append(f.calls, "create")
	f.specs = append(f.specs, spec)
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
	a.Env.Secret = []string{"TOKEN"}
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
func TestBackupHookBlocksEarlierCommands(t *testing.T) {
	c, f, id, p := hookFixture(t)
	a := p.Request.Manifest.Apps["app"]
	a.Hooks.PreDeploy = []manifest.Hook{{Run: []string{"first"}}, {Backup: "db"}}
	p.Request.Manifest.Apps["app"] = a
	f.exits = []int{0}
	_, _, err := c.runPre(context.Background(), id, p)
	if err == nil || len(f.specs) != 0 {
		t.Fatalf("backup hook did not block: %v, %d commands", err, len(f.specs))
	}
}

func TestOneOffRunsExactArgv(t *testing.T) {
	c, f, id, p := hookFixture(t)
	f.exits = []int{0}
	if _, err := c.runOneOff(context.Background(), id, p, 1, "pre_hook", []string{"/tool", "two words"}, false); err != nil {
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
	v, err := c.runOneOff(context.Background(), id, p, 10000, "schema_before", []string{"version"}, true)
	if err != nil || v != "v7" {
		t.Fatalf("version %q: %v", v, err)
	}
	steps, err := c.Store.Intents(context.Background(), id)
	if err != nil || len(steps) != 1 || !strings.Contains(string(steps[0].Receipt), `"version":"v7"`) {
		t.Fatalf("receipt %v: %v", steps, err)
	}
}
