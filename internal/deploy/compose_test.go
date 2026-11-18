package deploy

import (
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const composeFileOne = "services:\n  database:\n    image: alpine:3.20\n    environment:\n      MODE: one\n    volumes:\n      - data:/data\nvolumes:\n  data: {}\n"
const composeFileTwo = "services:\n  database:\n    image: alpine:3.20\n    environment:\n      MODE: two\n    volumes:\n      - data:/data\nvolumes:\n  data: {}\n"

// configOne is `compose config --format json` for one service with a named volume, published on a private address.
const configOne = `{"services":{"database":{"image":"alpine:3.20","ports":[{"mode":"ingress","host_ip":"127.0.0.1","target":80,"published":"18080","protocol":"tcp"}],"volumes":[{"type":"volume","source":"data","target":"/data"}]}},"volumes":{"data":{"name":"shop-db_data"}}}`

// hashOne is `compose config --hash *` for the configuration of configOne.
const hashOne = "database hash-0000000000000001\n"

// fakeCompose answers the Compose CLI's config and hash calls with canned output, records every run, and reads the
// project's files when up runs.
type fakeCompose struct {
	mu         sync.Mutex
	config     string
	hashes     string
	upErr      error
	restartErr error
	runs       []engine.ComposeRun
	upFile     string
	upAuth     string
	upArgs     []string
	upEnvFound bool
	upAuthSeen bool
	downErr    error
	onDown     func(engine.ComposeRun)
}

func (f *fakeCompose) Run(_ context.Context, r engine.ComposeRun) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, r)
	switch r.Args[0] {
	case "config":
		if len(r.Args) > 1 && r.Args[1] == "--hash" {
			return []byte(f.hashes), nil, nil
		}
		return []byte(f.config), nil, nil
	case "up":
		b, _ := os.ReadFile(filepath.Join(r.Dir, "compose.yaml"))
		f.upFile = string(b)
		f.upAuth = r.DockerConfig
		f.upArgs = append([]string(nil), r.Args...)
		_, err := os.Stat(filepath.Join(r.Dir, ".env"))
		f.upEnvFound = err == nil
		if r.DockerConfig != "" {
			_, err = os.Stat(filepath.Join(r.DockerConfig, "config.json"))
			f.upAuthSeen = err == nil
		}
		return nil, nil, f.upErr
	case "restart":
		return nil, nil, f.restartErr
	case "down":
		if f.downErr != nil {
			return nil, nil, f.downErr
		}
		if f.onDown != nil {
			f.onDown(r)
		}
		return nil, nil, nil
	}
	return nil, nil, errors.New("unexpected compose command")
}

func TestComposeRegistryAuthExistsOnlyForUp(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	c, s := composeController(t, runner, newComposeDocker())
	ctx := context.Background()
	if err := s.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: "REGISTRY_TOKEN", Registry: "registry.example"}, []byte("token-value")); err != nil {
		t.Fatal(err)
	}
	m := composeApp(false)
	m.Registries = map[string]manifest.Registry{"registry.example": {Username: "robot", PasswordSecret: "REGISTRY_TOKEN"}}
	op, _, err := admit(t, c, ctx, Actor{ID: "ci"}, composeReq("auth", composeFileOne, m))
	if err != nil {
		t.Fatal(err)
	}
	if got := waitComposeOperation(t, s, op.ID); got.State != "succeeded" {
		t.Fatalf("deploy %s: %s", got.State, got.Err)
	}
	if !runner.upAuthSeen || runner.upAuth == "" {
		t.Fatalf("up did not see the registry auth (seen %v, dir %q)", runner.upAuthSeen, runner.upAuth)
	}
	if _, err = os.Stat(runner.upAuth); !os.IsNotExist(err) {
		t.Fatalf("registry auth outlived the deploy: %v", err)
	}
	for _, r := range runner.runs {
		if r.Args[0] != "up" && r.DockerConfig != "" {
			t.Fatalf("%s received registry auth", r.Args[0])
		}
	}
}

func TestComposeMissingRegistryCredentialStopsBeforeCompose(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	c, s := composeController(t, runner, newComposeDocker())
	m := composeApp(false)
	m.Registries = map[string]manifest.Registry{"registry.example": {Username: "robot", PasswordSecret: "ABSENT"}}
	op, _, err := admit(t, c, context.Background(), Actor{ID: "ci"}, composeReq("no-cred", composeFileOne, m))
	if err != nil {
		t.Fatal(err)
	}
	if got := waitComposeOperation(t, s, op.ID); got.State != "failed" || got.Err != string(errComposeRegistry) {
		t.Fatalf("deploy %s: %q", got.State, got.Err)
	}
	if len(runner.runs) != 0 {
		t.Fatalf("compose ran without a credential: %d runs", len(runner.runs))
	}
}

func (f *fakeCompose) upRuns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.runs {
		if r.Args[0] == "up" {
			n++
		}
	}
	return n
}

// composeDocker is an Engine that also reports Compose containers and volumes.
type composeDocker struct {
	*positiveEngine
	containers []engine.ComposeContainer
	volumes    map[string]bool
	// projectVolumes are the volumes Compose created for the project, found by label.
	projectVolumes []string
}

func (d *composeDocker) ProjectVolumes(context.Context, string) ([]string, error) {
	return d.projectVolumes, nil
}

func (d *composeDocker) ComposeContainers(context.Context, string) ([]engine.ComposeContainer, error) {
	return d.containers, nil
}

func (d *composeDocker) VolumeExists(_ context.Context, name string) (bool, error) {
	return d.volumes[name], nil
}

func composeController(t *testing.T, runner *fakeCompose, docker *composeDocker) (*Controller, *state.Store) {
	t.Helper()
	s := openStore(t)
	return &Controller{Engine: docker, Store: s, Environment: "test", Compose: runner, StateDir: t.TempDir()}, s
}

func newComposeDocker(containers ...engine.ComposeContainer) *composeDocker {
	return &composeDocker{positiveEngine: &positiveEngine{}, containers: containers, volumes: map[string]bool{"shop-db_data": true}}
}

func composeApp(stateful bool) manifest.Manifest {
	return manifest.Manifest{Project: "shop", Network: "berthy", Apps: map[string]manifest.App{"db": {Kind: "compose", File: "compose.yaml", Stateful: stateful, Deploy: manifest.Deploy{DeployTimeout: "30s", DrainTimeout: "5s"}}}}
}

func composeReq(id, file string, m manifest.Manifest) Request {
	return Request{RequestID: id, App: "db", Kind: "deploy", Manifest: m, ComposeFile: file}
}

// waitComposeOperation waits until the operation leaves the running state.
func waitComposeOperation(t *testing.T, s *state.Store, id string) state.Operation {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		op, err := s.Operation(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if op.State != "running" {
			return op
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("operation did not finish")
	return state.Operation{}
}

func eventKinds(t *testing.T, s *state.Store, id string) []string {
	t.Helper()
	events, err := s.EventsAfter(context.Background(), id, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

func digestOf(file string) string {
	sum := sha256.Sum256([]byte(file))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestComposeDeploySucceedsAndRecordsRelease(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	c, s := composeController(t, runner, newComposeDocker())
	op, created, err := admit(t, c, context.Background(), Actor{ID: "ci", Name: "ci"}, composeReq("one", composeFileOne, composeApp(true)))
	if err != nil || !created {
		t.Fatalf("admit: %v %v", created, err)
	}
	got := waitComposeOperation(t, s, op.ID)
	if got.State != "succeeded" {
		t.Fatalf("state %s: %s", got.State, got.Err)
	}
	rel, err := s.Applied(context.Background(), "db")
	if err != nil || rel.Digest != digestOf(composeFileOne) || len(rel.Bindings) != 0 {
		t.Fatalf("release %+v: %v", rel, err)
	}
	if runner.upFile != composeFileOne || !runner.upEnvFound {
		t.Fatalf("up saw %q (env %v)", runner.upFile, runner.upEnvFound)
	}
	want := []string{"up", "-d", "--remove-orphans", "--wait", "--wait-timeout", "30"}
	if strings.Join(runner.upArgs, " ") != strings.Join(want, " ") {
		t.Fatalf("up args %v", runner.upArgs)
	}
	intents, err := s.Intents(context.Background(), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sourced bool
	for _, i := range intents {
		if i.Kind == "compose_source" {
			var source struct {
				ComposeFile string `json:"compose_file"`
				Project     string `json:"project"`
			}
			if json.Unmarshal(i.Desired, &source) != nil || source.ComposeFile != composeFileOne || source.Project != "shop-db" {
				t.Fatalf("source intent %s", i.Desired)
			}
			sourced = true
		}
	}
	if !sourced {
		t.Fatal("no compose source intent")
	}
	dir := filepath.Join(c.StateDir, "compose", "shop-db")
	if b, err := os.ReadFile(filepath.Join(dir, "compose.yaml")); err != nil || string(b) != composeFileOne {
		t.Fatalf("project file: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(c.StateDir, "compose")); len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("staging left behind: %v", names)
	}
}

func TestComposeUpFailureIsReported(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne, upErr: errors.New("exit status 1")}
	c, s := composeController(t, runner, newComposeDocker())
	op, _, err := admit(t, c, context.Background(), Actor{ID: "ci"}, composeReq("up-fail", composeFileOne, composeApp(false)))
	if err != nil {
		t.Fatal(err)
	}
	got := waitComposeOperation(t, s, op.ID)
	if got.State != "failed" || got.Err != "compose up failed" {
		t.Fatalf("operation %s %q", got.State, got.Err)
	}
	if _, err := s.Applied(context.Background(), "db"); err == nil {
		t.Fatal("failed deploy recorded a release")
	}
}

func TestComposeStatefulRecreateNeedsAdministratorAndFlag(t *testing.T) {
	// The stored hash differs from the file's, so database would be recreated.
	stale := newComposeDocker(engine.ComposeContainer{ID: "c1", Service: "database", ConfigHash: "hash-old-000000000000"})
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	c, s := composeController(t, runner, stale)
	op, _, err := admit(t, c, context.Background(), Actor{ID: "ci"}, composeReq("no-flag", composeFileOne, composeApp(true)))
	if err != nil {
		t.Fatal(err)
	}
	got := waitComposeOperation(t, s, op.ID)
	want := "stateful app db would recreate database; deploy with --allow-recreate and an admin token"
	if got.State != "failed" || got.Err != want {
		t.Fatalf("operation %s %q", got.State, got.Err)
	}
	if runner.upRuns() != 0 {
		t.Fatal("blocked deploy ran up")
	}
	if kinds := eventKinds(t, s, op.ID); len(kinds) == 0 || !contains(kinds, "blocked") {
		t.Fatalf("events %v", kinds)
	}

	// A deploy token cannot ask for the recreation even with the flag.
	req := composeReq("flag-no-admin", composeFileOne, composeApp(true))
	req.AllowRecreate = true
	if _, _, err = admit(t, c, context.Background(), Actor{ID: "ci"}, req); !errors.Is(err, errAllowRecreateAdmin) {
		t.Fatalf("flag without admin: %v", err)
	}
	if _, err = c.Plan(context.Background(), Actor{ID: "ci"}, req); !errors.Is(err, errAllowRecreateAdmin) {
		t.Fatalf("plan flag without admin: %v", err)
	}

	// An administrator with the flag recreates the service.
	req = composeReq("admin-flag", composeFileOne, composeApp(true))
	req.AllowRecreate = true
	op, _, err = admit(t, c, context.Background(), Actor{ID: "admin", Admin: true}, req)
	if err != nil {
		t.Fatal(err)
	}
	if got = waitComposeOperation(t, s, op.ID); got.State != "succeeded" {
		t.Fatalf("admin recreate %s: %s", got.State, got.Err)
	}
	if runner.upRuns() != 1 {
		t.Fatalf("up runs %d", runner.upRuns())
	}
}

func TestComposeDryRunReturnsPlanWithBlockers(t *testing.T) {
	stale := newComposeDocker(engine.ComposeContainer{ID: "c1", Service: "database", ConfigHash: "hash-old-000000000000"}, engine.ComposeContainer{ID: "c2", Service: "retired", ConfigHash: "hash-x-0000000000"})
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	c, _ := composeController(t, runner, stale)
	plan, err := c.Plan(context.Background(), Actor{ID: "ci"}, composeReq("dry", composeFileOne, composeApp(true)))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Compose == nil || plan.Compose.Project != "shop-db" {
		t.Fatalf("plan %+v", plan)
	}
	if got := plan.Compose.Services; len(got) != 2 || got[0] != (ComposeService{Name: "database", Action: "recreate"}) || got[1] != (ComposeService{Name: "retired", Action: "remove"}) {
		t.Fatalf("services %+v", got)
	}
	if len(plan.Compose.Volumes) != 1 || plan.Compose.Volumes[0] != (ComposeVolume{Name: "shop-db_data", Action: "keep"}) {
		t.Fatalf("volumes %+v", plan.Compose.Volumes)
	}
	if len(plan.Compose.Blockers) != 2 || plan.Compose.Blockers[0] != "stateful app db would recreate database; deploy with --allow-recreate and an admin token" || plan.Compose.Blockers[1] != "stateful app db would remove retired; deploy with --allow-recreate and an admin token" {
		t.Fatalf("blockers %v", plan.Compose.Blockers)
	}
	if runner.upRuns() != 0 {
		t.Fatal("dry run ran up")
	}
}

func TestComposePublicPortBlocksBeforeAnyChange(t *testing.T) {
	public := `{"services":{"database":{"image":"alpine:3.20","ports":[{"mode":"ingress","host_ip":"0.0.0.0","target":80,"published":"8080","protocol":"tcp"}]}},"volumes":{}}`
	runner := &fakeCompose{config: public, hashes: hashOne}
	c, s := composeController(t, runner, newComposeDocker())
	op, _, err := admit(t, c, context.Background(), Actor{ID: "ci"}, composeReq("public", composeFileOne, composeApp(false)))
	if err != nil {
		t.Fatal(err)
	}
	got := waitComposeOperation(t, s, op.ID)
	if got.State != "failed" || got.Err != "service database publishes port 8080 on all interfaces; bind it to a private address" {
		t.Fatalf("operation %s %q", got.State, got.Err)
	}
	if runner.upRuns() != 0 {
		t.Fatal("public port deploy ran up")
	}
}

func TestComposeRefusesFileWithoutContentOrForImageApps(t *testing.T) {
	c, _ := composeController(t, &fakeCompose{}, newComposeDocker())
	ctx := context.Background()
	for _, file := range []string{"", "a\x00b", strings.Repeat("x", manifest.MaxComposeFile+1)} {
		if _, _, err := admit(t, c, ctx, Actor{ID: "ci"}, composeReq("bad", file, composeApp(false))); !errors.Is(err, errComposeFile) {
			t.Fatalf("file %q: %v", file[:min(len(file), 8)], err)
		}
	}
	image := manifest.Manifest{Project: "shop", Network: "berthy", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Roles: map[string]manifest.Role{"web": {}}}}}
	req := Request{RequestID: "img", App: "web", Kind: "deploy", Version: "sha256:" + strings.Repeat("a", 64), Manifest: image, ComposeFile: composeFileOne}
	if _, err := c.Plan(ctx, Actor{ID: "ci"}, req); !errors.Is(err, errComposeNotCompose) {
		t.Fatalf("image app with compose file: %v", err)
	}
}

func TestComposeAdoptionRecordsReleaseWithoutUp(t *testing.T) {
	matching := newComposeDocker(engine.ComposeContainer{ID: "c1", Service: "database", ConfigHash: "hash-0000000000000001"})
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	c, s := composeController(t, runner, matching)
	req := composeReq("adopt", composeFileOne, composeApp(true))
	req.Adopt = true
	plan, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, req)
	if err != nil || plan.Compose == nil || len(plan.Compose.Blockers) != 0 {
		t.Fatalf("adoption plan %+v: %v", plan.Compose, err)
	}
	op, _, err := admit(t, c, context.Background(), Actor{ID: "admin", Admin: true}, req)
	if err != nil {
		t.Fatal(err)
	}
	if got := waitComposeOperation(t, s, op.ID); got.State != "succeeded" {
		t.Fatalf("adoption %s: %s", got.State, got.Err)
	}
	if runner.upRuns() != 0 {
		t.Fatal("adoption ran up")
	}
	rel, err := s.Applied(context.Background(), "db")
	if err != nil || rel.Digest != digestOf(composeFileOne) {
		t.Fatalf("adopted release %+v: %v", rel, err)
	}
}

func TestComposeAdoptionRefusesDifferences(t *testing.T) {
	stale := newComposeDocker(engine.ComposeContainer{ID: "c1", Service: "database", ConfigHash: "hash-old-000000000000"})
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	c, s := composeController(t, runner, stale)
	req := composeReq("adopt-diff", composeFileOne, composeApp(true))
	req.Adopt = true
	op, _, err := admit(t, c, context.Background(), Actor{ID: "admin", Admin: true}, req)
	if err != nil {
		t.Fatal(err)
	}
	got := waitComposeOperation(t, s, op.ID)
	if got.State != "failed" || got.Err != "adoption would change service database; align the manifest first" {
		t.Fatalf("operation %s %q", got.State, got.Err)
	}
	if runner.upRuns() != 0 {
		t.Fatal("refused adoption ran up")
	}
	if _, err := s.Applied(context.Background(), "db"); err == nil {
		t.Fatal("refused adoption recorded a release")
	}
}

func TestComposeRollbackRedeploysStoredFile(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	c, s := composeController(t, runner, newComposeDocker())
	ctx := context.Background()
	admin := Actor{ID: "admin", Admin: true}
	for _, tc := range []struct{ id, file string }{{"first", composeFileOne}, {"second", composeFileTwo}} {
		op, _, err := admit(t, c, ctx, admin, composeReq(tc.id, tc.file, composeApp(false)))
		if err != nil {
			t.Fatal(err)
		}
		if got := waitComposeOperation(t, s, op.ID); got.State != "succeeded" {
			t.Fatalf("%s: %s %s", tc.id, got.State, got.Err)
		}
	}
	if runner.upFile != composeFileTwo {
		t.Fatalf("second deploy ran %q", runner.upFile)
	}
	op, _, err := admit(t, c, ctx, admin, Request{RequestID: "back", App: "db", Kind: "rollback", Manifest: composeApp(false)})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitComposeOperation(t, s, op.ID); got.State != "succeeded" {
		t.Fatalf("rollback %s: %s", got.State, got.Err)
	}
	if runner.upFile != composeFileOne {
		t.Fatalf("rollback ran %q, want the first file", runner.upFile)
	}
	rel, err := s.Applied(ctx, "db")
	if err != nil || rel.Digest != digestOf(composeFileOne) {
		t.Fatalf("applied after rollback %+v: %v", rel, err)
	}
}

func TestComposeRecoveryFinishesInterruptedRunAsInterrupted(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	c, s := composeController(t, runner, newComposeDocker())
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "crash", ActorID: "ci", Kind: "deploy", App: "db", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordIntent(ctx, op.ID, state.Intent{Ordinal: composeRunOrdinal, Kind: "compose", Name: "shop-db", Desired: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkStep(ctx, op.ID, composeRunOrdinal, "issued", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err = c.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.Operation(ctx, op.ID)
	if err != nil || got.State != "interrupted" || got.Err != "compose deploy interrupted" {
		t.Fatalf("operation %+v: %v", got, err)
	}
	if _, err = s.Active(ctx); err == nil {
		t.Fatal("interrupted operation kept the lock")
	}
	if runner.upRuns() != 0 {
		t.Fatal("recovery ran a Compose command")
	}
}

func TestComposeRestartRunsRestartAndRefusesScale(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	c, s := composeController(t, runner, newComposeDocker())
	ctx := context.Background()
	m := composeApp(false)
	b, _ := json.Marshal(m)
	seed, _, err := s.Admit(ctx, state.Admission{RequestID: "seed", ActorID: "ci", Kind: "deploy", App: "db", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, seed.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "db", Digest: digestOf(composeFileOne), Manifest: b}}); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(c.StateDir, "compose", "shop-db"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Plan(ctx, Actor{ID: "ci"}, Request{App: "db", Kind: "restart", DryRun: true}); err != nil {
		t.Fatalf("restart plan: %v", err)
	}
	op, _, err := admit(t, c, ctx, Actor{ID: "ci"}, Request{RequestID: "restart", App: "db", Kind: "restart"})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitComposeOperation(t, s, op.ID); got.State != "succeeded" {
		t.Fatalf("restart %s: %s", got.State, got.Err)
	}
	restarted := false
	for _, r := range runner.runs {
		if len(r.Args) == 1 && r.Args[0] == "restart" {
			restarted = true
		}
	}
	if !restarted || runner.upRuns() != 0 {
		t.Fatalf("runs %+v", runner.runs)
	}
	if _, _, err = admit(t, c, ctx, Actor{ID: "admin", Admin: true}, Request{RequestID: "scale", App: "db", Kind: "scale", Replicas: new(int)}); !errors.Is(err, errScaleCompose) {
		t.Fatalf("scale: %v", err)
	}
	if _, _, err = admit(t, c, ctx, Actor{ID: "admin", Admin: true}, Request{RequestID: "exec", App: "db", Kind: "exec", Role: "web", Command: []string{"true"}}); !errors.Is(err, errComposeRole) {
		t.Fatalf("exec with a role: %v", err)
	}
	if _, err = c.Plan(ctx, Actor{ID: "admin", Admin: true}, Request{App: "db", Kind: "scale", Replicas: new(int), DryRun: true}); !errors.Is(err, errScaleCompose) {
		t.Fatalf("scale plan: %v", err)
	}
}

func TestComposeStageWritesEnvironmentAndRegistryAuth(t *testing.T) {
	runner := &fakeCompose{}
	c, s := composeController(t, runner, newComposeDocker())
	ctx := context.Background()
	actor := state.Actor{ID: "admin", Name: "admin"}
	if err := s.PutSecret(ctx, actor, state.SecretInfo{Name: "DB_PASSWORD"}, []byte(`it's "quoted" $HOME \back`)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSecret(ctx, actor, state.SecretInfo{Name: "REGISTRY_TOKEN", Registry: "registry.example"}, []byte("token-value")); err != nil {
		t.Fatal(err)
	}
	m := composeApp(false)
	app := m.Apps["db"]
	app.Env = manifest.Env{Plain: map[string]string{"MODE": "one", "NOTE": "plain value"}, Secret: []manifest.SecretVar{{Var: "DB_PASSWORD", Secret: "DB_PASSWORD"}}}
	m.Apps["db"] = app
	m.Registries = map[string]manifest.Registry{"registry.example": {Username: "robot", PasswordSecret: "REGISTRY_TOKEN"}}
	dir := t.TempDir()
	auth, err := c.stageCompose(ctx, dir, composeReq("stage", composeFileOne, m))
	if err != nil {
		t.Fatal(err)
	}
	env, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	// The Berthy identity comes first, sorted; the app's own variables follow.
	want := "BERTHY_APP='db'\nBERTHY_ENVIRONMENT='test'\nBERTHY_PROJECT='shop'\n" + "MODE='one'\nNOTE='plain value'\n" + `DB_PASSWORD="it's \"quoted\" \$HOME \\back"` + "\n"
	if string(env) != want {
		t.Fatalf("environment file %q, want %q", env, want)
	}
	if runtime.GOOS != "windows" {
		for _, p := range []string{filepath.Join(dir, ".env"), filepath.Join(dir, "compose.yaml")} {
			if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("%s mode %v: %v", filepath.Base(p), info.Mode().Perm(), err)
			}
		}
	}
	if auth == "" {
		t.Fatal("no docker config for a listed registry")
	}
	raw, err := os.ReadFile(filepath.Join(auth, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err = json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	decoded, _ := base64.StdEncoding.DecodeString(cfg.Auths["registry.example"].Auth)
	if string(decoded) != "robot:token-value" || len(cfg.Auths) != 1 {
		t.Fatalf("auths %+v", cfg.Auths)
	}
	if err = os.RemoveAll(auth); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(auth); !os.IsNotExist(err) {
		t.Fatal("docker config outlived its command")
	}

	// A secret with a line break cannot be written to an environment file.
	if err = s.PutSecret(ctx, actor, state.SecretInfo{Name: "DB_PASSWORD"}, []byte("first\nsecond")); err != nil {
		t.Fatal(err)
	}
	if _, err = c.stageCompose(ctx, t.TempDir(), composeReq("nl", composeFileOne, m)); !errors.Is(err, errComposeSecret) {
		t.Fatalf("secret with newline: %v", err)
	}
	// A missing secret stops the deploy with the workload message.
	m.Apps["db"] = manifest.App{Kind: "compose", File: "compose.yaml", Env: manifest.Env{Secret: []manifest.SecretVar{{Var: "ABSENT", Secret: "ABSENT"}}}}
	if _, err = c.stageCompose(ctx, t.TempDir(), composeReq("missing", composeFileOne, m)); !errors.Is(err, errComposeSecretMissing) {
		t.Fatalf("missing secret: %v", err)
	}
}

func TestComposeEnvValueQuoting(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"plain", "'plain'", true},
		{"with space #hash $dollar \\back", "'with space #hash $dollar \\back'", true},
		{`it's "quoted" $x`, `"it's \"quoted\" \$x"`, true},
		{"line\nbreak", "", false},
		{"carriage\rreturn", "", false},
		{"nul\x00byte", "", false},
	} {
		got, ok := composeEnvValue(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%q: %q %v, want %q %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestBuildComposePlanActionsAndVolumes(t *testing.T) {
	cfg := composeConfigFile{
		Services: map[string]composeConfigService{
			"api":   {},
			"cache": {},
			"web":   {},
		},
		Volumes: map[string]composeConfigVolume{
			"data":     {Name: "shop-db_data"},
			"shared":   {Name: "shared-volume", External: json.RawMessage("true")},
			"uploads":  {Name: "shop-db_uploads"},
			"unlisted": {Name: "shop-db_unlisted"},
		},
	}
	plan := buildComposePlan(composeInputs{
		Project: "shop-db",
		App:     "db",
		Config:  cfg,
		Hashes:  map[string]string{"api": "hash-api-000000000", "cache": "hash-cache-00000000", "web": "hash-web-0000000000"},
		Containers: []engine.ComposeContainer{
			{ID: "1", Service: "api", ConfigHash: "hash-api-000000000"},
			{ID: "2", Service: "cache", ConfigHash: "hash-old-000000000"},
			{ID: "3", Service: "retired", ConfigHash: "hash-old-000000000"},
			{ID: "4", Service: "api", ConfigHash: "hash-api-000000000", OneOff: true},
		},
		VolumeExists: map[string]bool{"shop-db_data": true, "shop-db_uploads": false, "shop-db_unlisted": true},
	})
	wantServices := []ComposeService{{"api", "keep"}, {"cache", "recreate"}, {"web", "create"}, {"retired", "remove"}}
	if len(plan.Services) != len(wantServices) {
		t.Fatalf("services %+v", plan.Services)
	}
	for i, s := range wantServices {
		if plan.Services[i] != s {
			t.Fatalf("service %d: %+v, want %+v", i, plan.Services[i], s)
		}
	}
	wantVolumes := []ComposeVolume{{"shop-db_data", "keep"}, {"shop-db_unlisted", "keep"}, {"shop-db_uploads", "create"}}
	if len(plan.Volumes) != len(wantVolumes) {
		t.Fatalf("volumes %+v", plan.Volumes)
	}
	for i, v := range wantVolumes {
		if plan.Volumes[i] != v {
			t.Fatalf("volume %d: %+v, want %+v", i, plan.Volumes[i], v)
		}
	}
	if len(plan.Blockers) != 0 {
		t.Fatalf("blockers %v", plan.Blockers)
	}
	if got := firstDifference(plan); got != "service cache" {
		t.Fatalf("first difference %q", got)
	}
}

func TestBuildComposePlanBlockers(t *testing.T) {
	svc := func(ports ...composeConfigPort) composeConfigService { return composeConfigService{Ports: ports} }
	root := filepath.Join(t.TempDir(), "compose")
	hostPath := filepath.Join(t.TempDir(), "srv", "data")
	cfg := composeConfigFile{Services: map[string]composeConfigService{
		"open":    svc(composeConfigPort{Published: "8080"}),
		"wild":    svc(composeConfigPort{HostIP: "0.0.0.0", Published: "9090"}),
		"v6":      svc(composeConfigPort{HostIP: "::", Published: "7070"}),
		"private": svc(composeConfigPort{HostIP: "127.0.0.1", Published: "6060"}),
		"unused":  svc(composeConfigPort{}),
		"builder": {Build: json.RawMessage(`{"context":"."}`)},
		"binder":  {Volumes: []composeConfigMount{{Type: "bind", Source: filepath.Join(root, "shop-db", "data")}}},
		"host":    {Volumes: []composeConfigMount{{Type: "bind", Source: hostPath}}},
	}}
	// A bind mount inside the Compose root is refused for everyone. A host mount needs an administrator.
	admin := buildComposePlan(composeInputs{Project: "shop-db", App: "db", Root: root, Admin: true, Config: cfg})
	wantAdmin := []string{
		"service binder binds a path inside the agent's Compose directory; use a named volume or a path outside it",
		"service builder builds an image; Compose apps deploy images only",
		"service open publishes port 8080 on all interfaces; bind it to a private address",
		"service v6 publishes port 7070 on all interfaces; bind it to a private address",
		"service wild publishes port 9090 on all interfaces; bind it to a private address",
	}
	if strings.Join(admin.Blockers, "\n") != strings.Join(wantAdmin, "\n") {
		t.Fatalf("administrator blockers:\n%s", strings.Join(admin.Blockers, "\n"))
	}
	deploy := buildComposePlan(composeInputs{Project: "shop-db", App: "db", Root: root, Config: cfg})
	wantDeploy := append(append([]string(nil), wantAdmin...),
		"service binder mounts host path "+filepath.Join(root, "shop-db", "data")+"; deploy it with an admin token",
		"service host mounts host path "+hostPath+"; deploy it with an admin token")
	if strings.Join(deploy.Blockers, "\n") != strings.Join(wantDeploy, "\n") {
		t.Fatalf("deploy blockers:\n%s", strings.Join(deploy.Blockers, "\n"))
	}

	stateful := composeInputs{Project: "shop-db", App: "db", Root: root, Stateful: true, Config: composeConfigFile{Services: map[string]composeConfigService{"database": {}}}, Containers: []engine.ComposeContainer{{ID: "1", Service: "database", ConfigHash: "old"}}, Hashes: map[string]string{"database": "new"}}
	if got := buildComposePlan(stateful).Blockers; len(got) != 1 || got[0] != "stateful app db would recreate database; deploy with --allow-recreate and an admin token" {
		t.Fatalf("stateful blockers %v", got)
	}
	stateful.AllowRecreate = true
	if got := buildComposePlan(stateful).Blockers; len(got) != 0 {
		t.Fatalf("allowed recreate blocked: %v", got)
	}
	stateful.AllowRecreate = false
	stateful.Adopt = true
	if got := buildComposePlan(stateful).Blockers; len(got) != 0 {
		t.Fatalf("adoption blocked by stateful rule: %v", got)
	}
}

// hostAccessCases holds one host-reaching setting each, in the JSON that `compose config --format json
// --no-env-resolution` prints on Compose v5.5.1. want is the refusal's first words, before the admin instruction.
var hostAccessCases = []struct {
	name, config, want string
}{
	{"privileged", `{"services":{"web":{"privileged":true}}}`, "service web runs privileged"},
	{"host network", `{"services":{"web":{"network_mode":"host"}}}`, "service web uses the host network namespace"},
	{"host pid", `{"services":{"web":{"pid":"host"}}}`, "service web uses the host pid namespace"},
	{"host ipc", `{"services":{"web":{"ipc":"host"}}}`, "service web uses the host ipc namespace"},
	{"host uts", `{"services":{"web":{"uts":"host"}}}`, "service web uses the host uts namespace"},
	{"host user", `{"services":{"web":{"userns_mode":"host"}}}`, "service web uses the host user namespace"},
	{"host cgroup", `{"services":{"web":{"cgroup":"host"}}}`, "service web uses the host cgroup namespace"},
	{"added capability", `{"services":{"web":{"cap_add":["NET_BIND_SERVICE"]}}}`, "service web adds capabilities"},
	{"device", `{"services":{"web":{"devices":[{"permissions":"rwm","source":"/dev/null","target":"/dev/null"}]}}}`, "service web maps host devices"},
	{"device cgroup rule", `{"services":{"web":{"device_cgroup_rules":["c 1:3 mr"]}}}`, "service web maps host devices"},
	{"unconfined seccomp", `{"services":{"web":{"security_opt":["seccomp=unconfined"]}}}`, "service web relaxes security options"},
	{"label disabled", `{"services":{"web":{"security_opt":["label=disable"]}}}`, "service web relaxes security options"},
	{"short bind mount", `{"services":{"web":{"volumes":[{"type":"bind","source":"/var/run/docker.sock","target":"/var/run/docker.sock","bind":{}}]}}}`, "service web mounts host path /var/run/docker.sock"},
	{"long bind mount", `{"services":{"web":{"volumes":[{"type":"bind","source":"/srv/app","target":"/app","read_only":true}]}}}`, "service web mounts host path /srv/app"},
	{"env file", `{"services":{"web":{"env_file":[{"path":"/etc/app.env"}]}}}`, "service web reads an env_file from the host"},
	{"secret file", `{"services":{"web":{"secrets":[{"source":"tok","target":"/run/secrets/tok"}]}},"secrets":{"tok":{"file":"/etc/tok","name":"x_tok"}}}`, "service web reads a host file through secret tok"},
	{"config file", `{"services":{"web":{"configs":[{"source":"cfg"}]}},"configs":{"cfg":{"file":"/etc/cfg","name":"x_cfg"}}}`, "service web reads a host file through config cfg"},
}

// TestHostAccessNeedsAdministratorForEachSetting checks each setting alone: a deploy token is refused it with the admin
// instruction, and an administrator's token deploys the same file.
func TestHostAccessNeedsAdministratorForEachSetting(t *testing.T) {
	for _, tc := range hostAccessCases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg composeConfigFile
			if err := json.Unmarshal([]byte(tc.config), &cfg); err != nil {
				t.Fatal(err)
			}
			deploy := buildComposePlan(composeInputs{Project: "shop-db", App: "db", Config: cfg}).Blockers
			if want := tc.want + "; deploy it with an admin token"; len(deploy) != 1 || deploy[0] != want {
				t.Fatalf("deploy token blockers %q, want %q", deploy, want)
			}
			if got := buildComposePlan(composeInputs{Project: "shop-db", App: "db", Admin: true, Config: cfg}).Blockers; len(got) != 0 {
				t.Fatalf("administrator blocked: %q", got)
			}
		})
	}
}

// TestHostAccessAllowsOrdinaryConfiguration checks settings that do not reach the host: a named volume, a named secret
// from an environment variable, a private port, and security options that keep their protections.
func TestHostAccessAllowsOrdinaryConfiguration(t *testing.T) {
	ordinary := `{"services":{"web":{"volumes":[{"type":"volume","source":"data","target":"/data"}],"security_opt":["no-new-privileges:true"],"ports":[{"host_ip":"127.0.0.1","published":"8080","target":80}],"secrets":[{"source":"env_tok","target":"/run/secrets/env_tok"}]}},"secrets":{"env_tok":{"environment":"TOKEN_VALUE","name":"x_env_tok"}}}`
	var cfg composeConfigFile
	if err := json.Unmarshal([]byte(ordinary), &cfg); err != nil {
		t.Fatal(err)
	}
	if got := buildComposePlan(composeInputs{Project: "shop-db", App: "db", Config: cfg}).Blockers; len(got) != 0 {
		t.Fatalf("ordinary configuration blocked for a deploy token: %q", got)
	}
}

// TestHostAccessAppliesToAdoption: an adoption needs an administrator, so only the administrator's row reaches the
// plan. The same planning function applies the rule to both, and this test keeps that true.
func TestHostAccessAppliesToAdoption(t *testing.T) {
	var cfg composeConfigFile
	if err := json.Unmarshal([]byte(`{"services":{"store":{"privileged":true}}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	want := []string{"service store runs privileged; deploy it with an admin token"}
	if got := buildComposePlan(composeInputs{Project: "p3alegacy", App: "legacy", Adopt: true, Config: cfg}).Blockers; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("adoption without administrator: %q", got)
	}
	if got := buildComposePlan(composeInputs{Project: "p3alegacy", App: "legacy", Adopt: true, Admin: true, Config: cfg}).Blockers; len(got) != 0 {
		t.Fatalf("administrator adoption blocked: %q", got)
	}
}

// TestHostAccessDeployEndToEnd runs the whole deploy: a deploy token is refused before anything changes, and an
// administrator's token deploys the privileged service.
func TestHostAccessDeployEndToEnd(t *testing.T) {
	privileged := `{"services":{"database":{"image":"alpine:3.20","privileged":true}},"volumes":{}}`
	runner := &fakeCompose{config: privileged, hashes: "database hash-0000000000000003\n"}
	c, s := composeController(t, runner, newComposeDocker())
	ctx := context.Background()
	op, _, err := admit(t, c, ctx, Actor{ID: "ci"}, composeReq("priv-ci", composeFileOne, composeApp(false)))
	if err != nil {
		t.Fatal(err)
	}
	got := waitComposeOperation(t, s, op.ID)
	if got.State != "failed" || got.Err != "service database runs privileged; deploy it with an admin token" {
		t.Fatalf("deploy token: %s %q", got.State, got.Err)
	}
	if runner.upRuns() != 0 {
		t.Fatal("refused deploy ran up")
	}
	op, _, err = admit(t, c, ctx, Actor{ID: "admin", Admin: true}, composeReq("priv-admin", composeFileOne, composeApp(false)))
	if err != nil {
		t.Fatal(err)
	}
	if got = waitComposeOperation(t, s, op.ID); got.State != "succeeded" {
		t.Fatalf("administrator: %s %q", got.State, got.Err)
	}
	if runner.upRuns() != 1 {
		t.Fatalf("administrator deploy ran up %d times", runner.upRuns())
	}
	// The planning read must not resolve env files, or env_file would be invisible; the hash read must still resolve them.
	planRead := false
	hashRead := false
	for _, r := range runner.runs {
		if len(r.Args) > 1 && r.Args[1] == "--format" && contains(r.Args, "--no-env-resolution") {
			planRead = true
		}
		if len(r.Args) > 1 && r.Args[1] == "--hash" && !contains(r.Args, "--no-env-resolution") {
			hashRead = true
		}
	}
	if !planRead || !hashRead {
		t.Fatalf("planning read %v, hash read %v", planRead, hashRead)
	}
}

// TestSafePathAndSecurityRelaxation covers the text a refusal shows for a host path, and the security options that
// relax a protection.
func TestSafePathAndSecurityRelaxation(t *testing.T) {
	if got := safePath("/var/run/docker.sock"); got != "/var/run/docker.sock" {
		t.Fatalf("plain path %q", got)
	}
	for _, p := range []string{"", "/x\ny", "/tab\there", "/é", strings.Repeat("a", 201)} {
		if got := safePath(p); got != "(path not shown)" {
			t.Fatalf("%q shown as %q", p, got)
		}
	}
	for opt, relaxed := range map[string]bool{
		"seccomp=unconfined":     true,
		"apparmor=unconfined":    true,
		"label:disable":          true,
		"label=disable":          true,
		"no-new-privileges:true": false,
		"seccomp=default.json":   false,
	} {
		if relaxesSecurity(opt) != relaxed {
			t.Errorf("%s relaxed=%v", opt, !relaxed)
		}
	}
}

// sharedContainerCases: a reference to a container outside the project is refused a deploy token, and a reference to a
// service of the project is not. The JSON is what `compose config --format json --no-env-resolution` prints on v5.5.1.
// want is the container the refusal names, empty when there is no refusal.
var sharedContainerCases = []struct {
	name, config, want string
}{
	{"volumes_from container", `{"services":{"web":{"volumes_from":["container:data-store"]}}}`, "data-store"},
	{"volumes_from container read-only", `{"services":{"web":{"volumes_from":["container:data-store:ro"]}}}`, "data-store"},
	{"network_mode container", `{"services":{"web":{"network_mode":"container:data-store"}}}`, "data-store"},
	{"pid container", `{"services":{"web":{"pid":"container:data-store"}}}`, "data-store"},
	{"ipc container", `{"services":{"web":{"ipc":"container:data-store"}}}`, "data-store"},
	{"userns container", `{"services":{"web":{"userns_mode":"container:data-store"}}}`, "data-store"},
	{"container name not safe to show", `{"services":{"web":{"network_mode":"container:dätastore"}}}`, "(path not shown)"},
	{"volumes_from service", `{"services":{"web":{"volumes_from":["db"]}}}`, ""},
	{"network_mode service", `{"services":{"web":{"network_mode":"service:db"}}}`, ""},
	{"pid service", `{"services":{"web":{"pid":"service:db"}}}`, ""},
	{"ipc service", `{"services":{"web":{"ipc":"service:db"}}}`, ""},
	{"network_mode bridge", `{"services":{"web":{"network_mode":"bridge"}}}`, ""},
	{"no shared resources", `{"services":{"web":{"image":"alpine:3.20"}}}`, ""},
}

// TestSharedContainerReferences: a deploy token is refused a reference to a container outside the project, with the
// admin instruction and the container's name; an administrator is not, and a service of the project is allowed to both.
func TestSharedContainerReferences(t *testing.T) {
	for _, tc := range sharedContainerCases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg composeConfigFile
			if err := json.Unmarshal([]byte(tc.config), &cfg); err != nil {
				t.Fatal(err)
			}
			deploy := buildComposePlan(composeInputs{Project: "shop-db", App: "db", Config: cfg}).Blockers
			if tc.want == "" {
				if len(deploy) != 0 {
					t.Fatalf("deploy token blocked: %q", deploy)
				}
			} else if want := "service web shares container " + tc.want + "; deploy it with an admin token"; len(deploy) != 1 || deploy[0] != want {
				t.Fatalf("deploy token blockers %q, want %q", deploy, want)
			}
			if got := buildComposePlan(composeInputs{Project: "shop-db", App: "db", Admin: true, Config: cfg}).Blockers; len(got) != 0 {
				t.Fatalf("administrator blocked: %q", got)
			}
		})
	}
}

func TestComposeTextsAreTheOnlyDynamicPublicMessages(t *testing.T) {
	for _, text := range []string{
		portBlocker("database", "8080"),
		portBlocker("database", "8000-8010"),
		buildBlocker("worker-1"),
		bindBlocker("binder"),
		statefulBlocker("db", "database", "recreate"),
		statefulBlocker("db", "retired", "remove"),
		"adoption would change service database; align the manifest first",
		"adoption would change volume shop-db_data; align the manifest first",
		string(errAllowRecreateAdmin),
		string(errComposeUp),
		string(errComposeInterrupted),
		"service web runs privileged; deploy it with an admin token",
		"service web uses the host network namespace; deploy it with an admin token",
		"service web adds capabilities; deploy it with an admin token",
		"service web maps host devices; deploy it with an admin token",
		"service web relaxes security options; deploy it with an admin token",
		"service web mounts host path /var/run/docker.sock; deploy it with an admin token",
		"service web reads an env_file from the host; deploy it with an admin token",
		"service web reads a host file through secret tok; deploy it with an admin token",
		"service web reads a host file through config cfg; deploy it with an admin token",
		"service web shares container data-store; deploy it with an admin token",
		"service web shares container (path not shown); deploy it with an admin token",
		"no running container for db/postgres",
		"no running container for db",
		string(errExecServices),
		string(errExecServiceImage),
	} {
		if !PublicMessage(text) {
			t.Errorf("not public: %q", text)
		}
	}
	for _, text := range []string{
		"workload secret unavailable",
		"registry credential unavailable",
		"service database would delete everything",
		"adoption would change service database",
		"stateful app db would recreate database",
		"service web runs privileged",
		"service web runs privileged; deploy it",
		"service web shares container data-store",
		"no running container for db/post gres",
		"no running container for DB",
	} {
		if PublicMessage(text) {
			t.Errorf("public: %q", text)
		}
	}
	// The digest of the file is what a release records, so a changed file changes the release.
	if digestOf(composeFileOne) == digestOf(composeFileTwo) {
		t.Fatal("digests collide")
	}
}

func TestAllowRecreateAdminCheckedBeforeEngine(t *testing.T) {
	c := Controller{}
	req := composeReq("x", composeFileOne, composeApp(true))
	req.AllowRecreate = true
	if _, err := c.Plan(context.Background(), Actor{ID: "ci"}, req); !errors.Is(err, errAllowRecreateAdmin) || !strings.Contains(err.Error(), "administrator access") {
		t.Fatalf("plan: %v", err)
	}
	if _, _, err := admit(t, &c, context.Background(), Actor{ID: "ci"}, req); !errors.Is(err, errAllowRecreateAdmin) {
		t.Fatalf("admit: %v", err)
	}
}

func TestComposeFileDigestMatchesSha256(t *testing.T) {
	sum := sha256.Sum256([]byte(composeFileOne))
	if got := composeDigest(composeFileOne); got != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("digest %s", got)
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// composeExecEngine is a Compose engine that also runs exec. It records the container each command ran in.
type composeExecEngine struct {
	*composeDocker
	mu     sync.Mutex
	ran    []string
	output string
}

func (f *composeExecEngine) InspectContainer(context.Context, string) (engine.ContainerSnapshot, error) {
	return engine.ContainerSnapshot{Running: true}, nil
}

func (f *composeExecEngine) Exec(_ context.Context, id string, _ []string) (engine.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ran = append(f.ran, id)
	return engine.ExecResult{Output: []byte(f.output)}, nil
}

// execComposeController returns a controller whose app db is a Compose app with an applied release, and whose Docker lists
// the containers given. Exec runs in those containers and records which one it ran in.
func execComposeController(t *testing.T, containers ...engine.ComposeContainer) (*Controller, *state.Store, *composeExecEngine) {
	t.Helper()
	docker := &composeExecEngine{composeDocker: newComposeDocker(containers...)}
	c, s := composeController(t, &fakeCompose{}, docker.composeDocker)
	c.Engine = docker
	ctx := context.Background()
	b, _ := json.Marshal(composeApp(false))
	seed, _, err := s.Admit(ctx, state.Admission{RequestID: "seed", ActorID: "ci", Kind: "deploy", App: "db", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, seed.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "db", Digest: digestOf(composeFileOne), Manifest: b}}); err != nil {
		t.Fatal(err)
	}
	return c, s, docker
}

// TestComposeExecRunsInTheNamedServiceAndStoresTheReceipt: the exec runs in the running container of the named service, and
// its output and exit code are stored as the operation's receipt.
func TestComposeExecRunsInTheNamedServiceAndStoresTheReceipt(t *testing.T) {
	c, s, docker := execComposeController(t,
		engine.ComposeContainer{ID: "pg-old", Service: "postgres", State: "exited", Number: 1},
		engine.ComposeContainer{ID: "side-1", Service: "side", State: "running", Number: 1},
		engine.ComposeContainer{ID: "pg-2", Service: "postgres", State: "running", Number: 2},
	)
	docker.output = "42"
	ctx := context.Background()
	op, _, err := admit(t, c, ctx, Actor{ID: "admin", Admin: true}, Request{RequestID: "exec", App: "db", Kind: "exec", Service: "postgres", Command: []string{"psql", "-tAc", "select 40+2"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitComposeOperation(t, s, op.ID); got.State != "succeeded" {
		t.Fatalf("exec %s: %s", got.State, got.Err)
	}
	if len(docker.ran) != 1 || docker.ran[0] != "pg-2" {
		t.Fatalf("exec ran in %v, want [pg-2]", docker.ran)
	}
	steps, err := s.Intents(ctx, op.ID)
	if err != nil || len(steps) != 1 || !strings.Contains(string(steps[0].Receipt), `"output":"42"`) || !strings.Contains(string(steps[0].Receipt), `"exit_code":0`) {
		t.Fatalf("receipt %v %v", steps, err)
	}
}

// TestComposeExecPlanNamesTheServiceAndIsStableAcrossContainers: a dry run names the service, runs nothing, and gives the same
// plan when the container it would pick changes, so the approval of a held exec still matches.
func TestComposeExecPlanNamesTheServiceAndIsStableAcrossContainers(t *testing.T) {
	c, _, docker := execComposeController(t,
		engine.ComposeContainer{ID: "pg-2", Service: "postgres", State: "running", Number: 2},
		engine.ComposeContainer{ID: "side-1", Service: "side", State: "running", Number: 1},
	)
	ctx := context.Background()
	req := Request{App: "db", Kind: "exec", Service: "postgres", Command: []string{"psql", "-c", "select 1"}}
	first, err := c.Plan(ctx, Actor{ID: "admin", Admin: true}, req)
	if err != nil || first.Exec == nil || first.Exec.Target != "shop-db/postgres" {
		t.Fatalf("plan %+v: %v", first, err)
	}
	if !slices.Equal(first.Command, req.Command) {
		t.Fatalf("plan command %v", first.Command)
	}
	docker.containers = []engine.ComposeContainer{
		{ID: "pg-9", Service: "postgres", State: "running", Number: 9},
		{ID: "pg-8", Service: "postgres", State: "running", Number: 8},
		{ID: "side-1", Service: "side", State: "running", Number: 1},
	}
	second, err := c.Plan(ctx, Actor{ID: "admin", Admin: true}, req)
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if err != nil || string(a) != string(b) {
		t.Fatalf("plan changed with the container: %s then %s: %v", a, b, err)
	}
	if len(docker.ran) != 0 {
		t.Fatalf("a dry run ran %v", docker.ran)
	}
}

// TestComposeExecNeedsAServiceWhenTheAppHasSeveral: without --service an app with two running services is refused, at the
// plan and as the operation's failure, and nothing runs.
func TestComposeExecNeedsAServiceWhenTheAppHasSeveral(t *testing.T) {
	c, s, docker := execComposeController(t,
		engine.ComposeContainer{ID: "pg-2", Service: "postgres", State: "running", Number: 2},
		engine.ComposeContainer{ID: "side-1", Service: "side", State: "running", Number: 1},
	)
	ctx := context.Background()
	var r Refusal
	if _, err := c.Plan(ctx, Actor{ID: "admin", Admin: true}, Request{App: "db", Kind: "exec", Command: []string{"true"}}); !errors.As(err, &r) || r != errExecServices {
		t.Fatalf("plan: %v", err)
	}
	op, _, err := admit(t, c, ctx, Actor{ID: "admin", Admin: true}, Request{RequestID: "exec", App: "db", Kind: "exec", Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitComposeOperation(t, s, op.ID); got.State != "failed" || got.Err != string(errExecServices) {
		t.Fatalf("exec %s: %q", got.State, got.Err)
	}
	if len(docker.ran) != 0 {
		t.Fatalf("exec ran in %v", docker.ran)
	}
}

// TestComposeExecNamesAServiceWithNoRunningContainer: a service with only stopped containers is refused with its name.
func TestComposeExecNamesAServiceWithNoRunningContainer(t *testing.T) {
	c, _, _ := execComposeController(t,
		engine.ComposeContainer{ID: "pg-old", Service: "postgres", State: "exited", Number: 1},
		engine.ComposeContainer{ID: "side-1", Service: "side", State: "running", Number: 1},
	)
	var r Refusal
	if _, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{App: "db", Kind: "exec", Service: "postgres", Command: []string{"true"}}); !errors.As(err, &r) || r != "no running container for db/postgres" {
		t.Fatalf("plan: %v", err)
	}
}

// TestExecRefusesAFlagTheAppKindDoesNotTake: --role on a Compose app and --service on an image app are refused before anything is
// journaled, both at Admit and at Plan.
func TestExecRefusesAFlagTheAppKindDoesNotTake(t *testing.T) {
	ctx := context.Background()
	c, _, _ := execComposeController(t)
	admin := Actor{ID: "admin", Admin: true}
	if _, _, err := admit(t, c, ctx, admin, Request{RequestID: "role", App: "db", Kind: "exec", Role: "web", Command: []string{"true"}}); !errors.Is(err, errComposeRole) {
		t.Fatalf("compose app with a role, admit: %v", err)
	}
	if _, err := c.Plan(ctx, admin, Request{App: "db", Kind: "exec", Role: "web", Command: []string{"true"}}); !errors.Is(err, errComposeRole) {
		t.Fatalf("compose app with a role, plan: %v", err)
	}
	image := Controller{Store: openStore(t), Engine: &positiveEngine{}, Environment: "test"}
	if _, _, err := admit(t, &image, ctx, admin, Request{RequestID: "svc", App: "app", Kind: "exec", Service: "postgres", Command: []string{"true"}}); !errors.Is(err, errExecServiceImage) {
		t.Fatalf("image app with a service, admit: %v", err)
	}
	if _, err := image.Plan(ctx, admin, Request{App: "app", Kind: "exec", Service: "postgres", Command: []string{"true"}}); !errors.Is(err, errExecServiceImage) {
		t.Fatalf("image app with a service, plan: %v", err)
	}
}
