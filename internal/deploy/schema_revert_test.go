package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"berthy/internal/manifest"
	"berthy/internal/state"
)

var (
	oldImage   = "docker.io/example/app@sha256:" + strings.Repeat("a", 64)
	newImage   = "docker.io/example/app@sha256:" + strings.Repeat("b", 64)
	newerImage = "docker.io/example/app@sha256:" + strings.Repeat("c", 64)
)

// revertManifest declares the migrations every fixture release carries.
func revertManifest() manifest.Manifest {
	one := 1
	return manifest.Manifest{Project: "proj", Network: "overlay", Apps: map[string]manifest.App{"app": {Kind: "image", Image: "example/app", Deploy: manifest.Deploy{DeployTimeout: "1s", DrainTimeout: "1s"}, Roles: map[string]manifest.Role{"web": {Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}}, Migrations: &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}, Revert: []string{"migrate", "down", "{target}"}}}}}
}

// revertFake runs one-offs through hookEngine and serves one service through positiveEngine.
type revertFake struct {
	*positiveEngine
	*hookEngine
	atOneOff []string // live service spec seen when each one-off container is created
}

func (f *revertFake) CreateContainer(ctx context.Context, name string, spec json.RawMessage) (string, error) {
	f.atOneOff = append(f.atOneOff, string(f.positiveEngine.spec))
	return f.hookEngine.CreateContainer(ctx, name, spec)
}

// newRevertFake scripts the exit code of each one-off and the version output lines.
func newRevertFake(s *state.Store, op string, exits []int, logs ...string) *revertFake {
	h := &hookEngine{store: s, op: op, exits: exits}
	for _, l := range logs {
		h.logs = append(h.logs, []byte(l+"\n"))
	}
	return &revertFake{positiveEngine: &positiveEngine{}, hookEngine: h}
}

// admitRevert admits a schema revert through the store, as the agent does before it executes one.
func admitRevert(t *testing.T, s *state.Store) state.Operation {
	t.Helper()
	op, _, err := s.Admit(context.Background(), state.Admission{RequestID: "revert", ActorID: "admin", Kind: "rollback", App: "app", PublicRequest: json.RawMessage(`{"app":"app","revert_schema":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

// seedDeploy admits and finishes a deploy. A non-empty image journals the migration source as execute does.
func seedDeploy(t *testing.T, s *state.Store, requestID, image string, out state.Outcome) {
	t.Helper()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: requestID, ActorID: "admin", Kind: "deploy", App: "app", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if image != "" {
		source, _ := json.Marshal(struct {
			Manifest manifest.Manifest `json:"manifest"`
			Image    string            `json:"image"`
		}{revertManifest(), image})
		if err = s.RecordIntent(ctx, op.ID, state.Intent{Ordinal: 9000, Kind: "schema_source", Desired: source}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Finish(ctx, op.ID, out); err != nil {
		t.Fatal(err)
	}
	// Later operations must start and release strictly after this one, so the timestamps order them.
	time.Sleep(3 * time.Millisecond)
}

// releaseOf is the release a successful deploy of image records.
func releaseOf(image string, bindings ...state.Binding) *state.Release {
	raw, _ := json.Marshal(revertManifest())
	return &state.Release{App: "app", Digest: image, Manifest: raw, Bindings: bindings}
}

func releaseID(t *testing.T, s *state.Store, image string) int64 {
	t.Helper()
	history, err := s.History(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range history {
		if r.Digest == image {
			return r.ID
		}
	}
	t.Fatalf("no release for %s", image)
	return 0
}

func TestSchemaRevertRunsFromFailedMigrationImage(t *testing.T) {
	s := openStore(t)
	seedDeploy(t, s, "old", "", state.Outcome{State: "succeeded", Release: releaseOf(oldImage)})
	seedDeploy(t, s, "bad", newImage, state.Outcome{State: "failed", SchemaBefore: "v1", SchemaAfter: "v2"})
	op := admitRevert(t, s)
	f := newRevertFake(s, op.ID, []int{0, 0, 0}, "v2", "v1")
	c := &Controller{Engine: f, Store: s, Environment: "test"}
	c.execute(context.Background(), op, Actor{ID: "admin", Admin: true}, Request{Kind: "rollback", App: "app", RevertSchema: true})
	got, err := s.Operation(context.Background(), op.ID)
	if err != nil || got.State != "succeeded" || got.SchemaBefore != "v2" || got.SchemaAfter != "v1" {
		t.Fatalf("operation %+v: %v", got, err)
	}
	if f.positiveEngine.spec != nil {
		t.Fatal("service was written")
	}
	if len(f.specs) != 3 || !strings.Contains(string(f.specs[1]), `"Image":"`+newImage+`"`) || !strings.Contains(string(f.specs[1]), `"Cmd":["down","v1"]`) {
		t.Fatalf("revert container: %s", f.specs)
	}
}

func TestSchemaRevertPlansImageRollbackFirst(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	seedDeploy(t, s, "old", "", state.Outcome{State: "succeeded", Release: releaseOf(oldImage)})
	seedDeploy(t, s, "new", newImage, state.Outcome{State: "succeeded", SchemaBefore: "v1", SchemaAfter: "v2", Release: releaseOf(newImage)})
	f := newRevertFake(s, "", nil)
	c := &Controller{Engine: f, Store: s, Environment: "test"}
	sr, err := c.schemaRevertPlan(ctx, Request{App: "app", RevertSchema: true})
	if err != nil || sr.Target != "v1" || sr.Image == nil || sr.Image.Digest != oldImage || sr.Prepared.Image != newImage || !reflect.DeepEqual(sr.Revert, []string{"migrate", "down", "v1"}) {
		t.Fatalf("plan %+v: %v", sr, err)
	}
	plan, err := c.Plan(ctx, Actor{ID: "admin", Admin: true}, Request{Kind: "rollback", App: "app", RevertSchema: true})
	if err != nil || plan.Image != oldImage || plan.SchemaRevert == nil || plan.SchemaRevert.Target != "v1" || plan.SchemaRevert.SourceImage != newImage || plan.SchemaRevert.RollbackRelease != sr.Image.ID {
		t.Fatalf("dry run %+v: %v", plan, err)
	}
	if len(f.calls) != 0 || f.positiveEngine.spec != nil {
		t.Fatalf("dry run changed state: %v", f.calls)
	}
}

func TestSchemaRevertRefusesNewerReleaseWithoutTarget(t *testing.T) {
	s := openStore(t)
	seedDeploy(t, s, "old", "", state.Outcome{State: "succeeded", Release: releaseOf(oldImage)})
	seedDeploy(t, s, "new", newImage, state.Outcome{State: "succeeded", SchemaBefore: "v1", SchemaAfter: "v2", Release: releaseOf(newImage)})
	seedDeploy(t, s, "newer", "", state.Outcome{State: "succeeded", Release: releaseOf(newerImage)})
	c := &Controller{Engine: newRevertFake(s, "", nil), Store: s, Environment: "test"}
	if _, err := c.schemaRevertPlan(context.Background(), Request{App: "app", RevertSchema: true}); !errors.Is(err, errNewerRelease) {
		t.Fatalf("newer release accepted: %v", err)
	}
}

func TestSchemaRevertTargetNeedsRecordedState(t *testing.T) {
	s := openStore(t)
	seedDeploy(t, s, "old", "", state.Outcome{State: "succeeded", Release: releaseOf(oldImage)})
	seedDeploy(t, s, "new", newImage, state.Outcome{State: "succeeded", SchemaBefore: "v1", SchemaAfter: "v2", Release: releaseOf(newImage)})
	c := &Controller{Engine: newRevertFake(s, "", nil), Store: s, Environment: "test"}
	if _, err := c.schemaRevertPlan(context.Background(), Request{App: "app", RevertSchema: true, TargetRelease: releaseID(t, s, oldImage)}); !errors.Is(err, errTargetMissing) {
		t.Fatalf("release without a recorded schema accepted as target: %v", err)
	}
}

func TestSchemaRevertToTargetsRecordedRelease(t *testing.T) {
	s := openStore(t)
	seedDeploy(t, s, "first", oldImage, state.Outcome{State: "succeeded", SchemaBefore: "v0", SchemaAfter: "v1", Release: releaseOf(oldImage)})
	seedDeploy(t, s, "second", newImage, state.Outcome{State: "succeeded", SchemaBefore: "v1", SchemaAfter: "v2", Release: releaseOf(newImage)})
	c := &Controller{Engine: newRevertFake(s, "", nil), Store: s, Environment: "test"}
	sr, err := c.schemaRevertPlan(context.Background(), Request{App: "app", RevertSchema: true, TargetRelease: releaseID(t, s, oldImage)})
	if err != nil || sr.Target != "v1" || sr.Image == nil || sr.Image.Digest != oldImage || sr.Prepared.Image != newImage {
		t.Fatalf("plan %+v: %v", sr, err)
	}
}

func TestSchemaRevertReportsUnreachedTarget(t *testing.T) {
	s := openStore(t)
	seedDeploy(t, s, "old", "", state.Outcome{State: "succeeded", Release: releaseOf(oldImage)})
	seedDeploy(t, s, "bad", newImage, state.Outcome{State: "failed", SchemaBefore: "v1", SchemaAfter: "v2"})
	op := admitRevert(t, s)
	f := newRevertFake(s, op.ID, []int{0, 0, 0}, "v2", "v3")
	c := &Controller{Engine: f, Store: s, Environment: "test"}
	c.execute(context.Background(), op, Actor{ID: "admin", Admin: true}, Request{Kind: "rollback", App: "app", RevertSchema: true})
	got, err := s.Operation(context.Background(), op.ID)
	if err != nil || got.State != "failed" || got.Err != "schema revert did not reach target" || got.SchemaBefore != "v2" || got.SchemaAfter != "v3" {
		t.Fatalf("operation %+v: %v", got, err)
	}
}

func TestSchemaRevertPlanRequiresAdminWithoutMutation(t *testing.T) {
	s := openStore(t)
	seedDeploy(t, s, "old", "", state.Outcome{State: "succeeded", Release: releaseOf(oldImage)})
	seedDeploy(t, s, "bad", newImage, state.Outcome{State: "failed", SchemaBefore: "v1", SchemaAfter: "v2"})
	f := newRevertFake(s, "", nil)
	c := &Controller{Engine: f, Store: s, Environment: "test"}
	req := Request{Kind: "rollback", App: "app", RevertSchema: true}
	if _, err := c.Plan(context.Background(), Actor{ID: "user"}, req); !errors.Is(err, errAdminRequired) {
		t.Fatalf("non-admin dry run: %v", err)
	}
	plan, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, req)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(plan); !strings.Contains(string(b), `"roles":[]`) {
		t.Fatalf("revert-only plan roles: %s", b)
	}
	if _, err := c.rollbackRequest(context.Background(), req); err == nil {
		t.Fatal("plain rollback accepted the schema flag")
	}
	if len(f.calls) != 0 || f.positiveEngine.spec != nil {
		t.Fatalf("dry run changed state: %v", f.calls)
	}
}

func TestSchemaRevertUncertainKeepsLock(t *testing.T) {
	s := openStore(t)
	seedDeploy(t, s, "old", "", state.Outcome{State: "succeeded", Release: releaseOf(oldImage)})
	seedDeploy(t, s, "bad", newImage, state.Outcome{State: "failed", SchemaBefore: "v1", SchemaAfter: "v2"})
	op := admitRevert(t, s)
	f := newRevertFake(s, op.ID, []int{0}, "v2")
	f.failStartAt = 2 // the migration_revert container is never confirmed started
	c := &Controller{Engine: f, Store: s, Environment: "test"}
	c.execute(context.Background(), op, Actor{ID: "admin", Admin: true}, Request{Kind: "rollback", App: "app", RevertSchema: true})
	got, err := s.Operation(context.Background(), op.ID)
	if err != nil || got.State != "recovery_required" || got.Err != "schema revert outcome uncertain" || got.SchemaBefore != "v2" || got.SchemaAfter != "" {
		t.Fatalf("operation %+v: %v", got, err)
	}
	lock, err := s.Lock(context.Background())
	if err != nil || lock.OperationID != op.ID {
		t.Fatalf("lock released: %+v %v", lock, err)
	}
}

func TestSchemaRevertRollsBackImageBeforeRevert(t *testing.T) {
	s := openStore(t)
	role := revertManifest().Apps["app"].Roles["web"]
	seedDeploy(t, s, "old", "", state.Outcome{State: "succeeded", Release: releaseOf(oldImage)})
	binding := state.Binding{App: "app", Role: "web", Name: "proj-app-web", ServiceID: "id"}
	seedDeploy(t, s, "new", newImage, state.Outcome{State: "succeeded", SchemaBefore: "v1", SchemaAfter: "v2", Release: releaseOf(newImage, binding)})
	live, err := serviceSpec("test", "proj", "app", "web", "prior", "net", newImage, nil, role, "1s", nil)
	if err != nil {
		t.Fatal(err)
	}
	op := admitRevert(t, s)
	f := newRevertFake(s, op.ID, []int{0, 0, 0}, "v2", "v1")
	f.positiveEngine.spec = live
	c := &Controller{Engine: f, Store: s, Environment: "test"}
	c.execute(context.Background(), op, Actor{ID: "admin", Admin: true}, Request{Kind: "rollback", App: "app", RevertSchema: true})
	got, err := s.Operation(context.Background(), op.ID)
	if err != nil || got.State != "succeeded" || got.SchemaBefore != "v2" || got.SchemaAfter != "v1" {
		t.Fatalf("operation %+v: %v", got, err)
	}
	applied, err := s.Applied(context.Background(), "app")
	if err != nil || applied.Digest != oldImage {
		t.Fatalf("applied %+v: %v", applied, err)
	}
	if len(f.atOneOff) != 3 || !strings.Contains(f.atOneOff[0], oldImage) || strings.Contains(f.atOneOff[0], newImage) {
		t.Fatalf("revert ran before the image rollback: %v", f.atOneOff)
	}
	if !strings.Contains(string(f.specs[1]), newImage) {
		t.Fatalf("revert did not run from the migration image: %s", f.specs[1])
	}
}

func TestOnlyPlanningRefusalsArePublic(t *testing.T) {
	for _, r := range []Refusal{errNoMigration, errSourceMissing, errNoRevertCommand, errSourceImage, errSourceNotPinned, errTargetMissing, errNoEarlierRelease, errNewerRelease, errBadRevertCommand, errAdminRequired} {
		if !PublicMessage(string(r)) {
			t.Fatalf("refusal %q is not public", r)
		}
	}
	if PublicMessage("Docker unavailable") || PublicMessage("applied release unavailable") {
		t.Fatal("engine or store failure is public")
	}
	s := openStore(t)
	seedDeploy(t, s, "old", "", state.Outcome{State: "succeeded", Release: releaseOf(oldImage)})
	seedDeploy(t, s, "bad", newImage, state.Outcome{State: "failed", SchemaBefore: "v1", SchemaAfter: "v2"})
	// Every refusal check passes before the planner reaches the engine, so this failure is the engine's.
	c := &Controller{Store: s, Environment: "test"}
	if _, err := c.schemaRevertPlan(context.Background(), Request{App: "app", RevertSchema: true}); err == nil || errors.As(err, new(Refusal)) {
		t.Fatalf("engine failure reported as refusal: %v", err)
	}
}

func TestSchemaRevertOperationRecordsRefusal(t *testing.T) {
	s := openStore(t)
	seedDeploy(t, s, "old", "", state.Outcome{State: "succeeded", Release: releaseOf(oldImage)})
	seedDeploy(t, s, "new", newImage, state.Outcome{State: "succeeded", SchemaBefore: "v1", SchemaAfter: "v2", Release: releaseOf(newImage)})
	seedDeploy(t, s, "newer", "", state.Outcome{State: "succeeded", Release: releaseOf(newerImage)})
	op := admitRevert(t, s)
	c := &Controller{Engine: newRevertFake(s, op.ID, nil), Store: s, Environment: "test"}
	c.execute(context.Background(), op, Actor{ID: "admin", Admin: true}, Request{Kind: "rollback", App: "app", RevertSchema: true})
	got, err := s.Operation(context.Background(), op.ID)
	if err != nil || got.State != "failed" || got.Err != string(errNewerRelease) {
		t.Fatalf("operation %+v: %v", got, err)
	}
}
