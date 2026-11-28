package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

func TestBarrierDeadlinePersistsRecoveryRequired(t *testing.T) {
	s := openStore(t)
	base := &budgetEngine{boundFake: &boundFake{}}
	seedBudgetUpdate(t, s, base)
	ctx := context.Background()
	previous, err := s.Applied(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	m := budgetManifest()
	m.Protected = true
	a := m.Apps["web"]
	delete(a.Roles, "b")
	a.Migrations = &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}}
	m.Apps["web"] = a
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "barrier-timeout", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	var started, ended, deadline time.Time
	f := &finalCheckEngine{budgetEngine: base, store: s, oneOff: newDiagFake()}
	f.oneOff.exits = []int{0, 0, 0}
	f.oneOff.versions = []string{"v1", "v2"}
	c := &Controller{Store: s, Engine: f, Environment: "test"}
	c.barrierWrite = func(ctx context.Context, _ string, i state.Intent) error {
		started = time.Now()
		deadline = i.Deadline
		<-ctx.Done()
		ended = time.Now()
		return ctx.Err()
	}
	synctest.Test(t, func(t *testing.T) {
		c.execute(ctx, op, Actor{ID: "a", Admin: true}, Request{RequestID: "barrier-timeout", App: "web", Version: digestOfLetter("a"), Manifest: m})
	})
	if started.IsZero() || ended.IsZero() || !ended.Equal(deadline) || !started.Before(deadline) {
		t.Fatalf("barrier write started=%v ended=%v deadline=%v", started, ended, deadline)
	}
	op, err = s.Operation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != "recovery_required" || op.Err != "verification barrier unavailable" {
		t.Fatalf("operation state=%s error=%q, want recovery_required and barrier error", op.State, op.Err)
	}
	if op.SchemaBefore != "v1" || op.SchemaAfter != "v2" {
		t.Fatalf("schema observations before=%q after=%q", op.SchemaBefore, op.SchemaAfter)
	}
	lock, err := s.Lock(ctx)
	if err != nil || lock.OperationID != op.ID {
		t.Fatalf("lock = %+v, %v", lock, err)
	}
	applied, err := s.Applied(ctx, "web")
	if err != nil || applied.ID != previous.ID || applied.Digest != previous.Digest {
		t.Fatalf("applied release = %+v, %v", applied, err)
	}
	protected, err := s.Protected(ctx)
	if err != nil || protected {
		t.Fatalf("protection changed: %v, %v", protected, err)
	}
	_, exists, err := s.Settings(ctx)
	if err != nil || exists {
		t.Fatalf("settings recorded: exists=%v, error=%v", exists, err)
	}
	events, err := s.EventsAfter(ctx, op.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	terminal := false
	for _, e := range events {
		if e.Kind == "recovery_required" {
			terminal = true
		}
	}
	if !terminal {
		t.Fatal("missing terminal event")
	}
}

type lateFirstRoleEngine struct {
	*budgetEngine
	confirmationEnded time.Time
}

func (f *lateFirstRoleEngine) InspectContainerHealth(ctx context.Context, id string) (string, error) {
	if id == "c-new-1" && f.confirmationEnded.IsZero() && time.Since(f.start) >= 5*time.Second {
		<-ctx.Done()
		f.confirmationEnded = time.Now()
		return "", ctx.Err()
	}
	return f.budgetEngine.InspectContainerHealth(ctx, id)
}
func TestUnwindIgnoresUntouchedUpdate(t *testing.T) {
	s := openStore(t)
	base := &budgetEngine{boundFake: &boundFake{}}
	ctx := context.Background()
	prior, _, err := s.Admit(ctx, state.Admission{RequestID: "prior-b", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(budgetManifest())
	if err != nil {
		t.Fatal(err)
	}
	old := json.RawMessage(`{"Name":"proj-test-web-b","TaskTemplate":{"ContainerSpec":{"Image":"example/web@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","Labels":{"berthy.operation":"previous"}}},"Mode":{"Replicated":{"Replicas":1}}}`)
	if err = s.Finish(ctx, prior.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "web", Digest: digestOfLetter("b"), Manifest: raw, Bindings: []state.Binding{{App: "web", Role: "b", Name: "proj-test-web-b", ServiceID: "old-b"}}}}); err != nil {
		t.Fatal(err)
	}
	base.add("old-b", old)
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "mixed-cutoff", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	f := &lateFirstRoleEngine{budgetEngine: base}
	c := &Controller{Store: s, Engine: f, Environment: "test"}
	synctest.Test(t, func(t *testing.T) {
		runCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		c.execute(runCtx, op, Actor{ID: "a", Admin: true}, Request{RequestID: "mixed-cutoff", App: "web", Version: digestOfLetter("a"), Manifest: budgetManifest()})
	})
	if f.confirmationEnded.IsZero() || f.confirmationEnded.Sub(f.start) != 30*time.Second {
		t.Fatalf("confirmation ended at %v, want 30s after startup", f.confirmationEnded.Sub(f.start))
	}
	if base.updates != 0 {
		t.Fatalf("untouched role was updated %d times", base.updates)
	}
	intents, err := s.Intents(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range intents {
		if i.Role == "b" {
			t.Fatalf("untouched update recorded: %+v", i)
		}
	}
	live, err := base.boundFake.InspectService(ctx, "proj-test-web-a")
	if err == nil {
		t.Fatalf("created role not removed: %+v", live)
	}
	live, err = base.boundFake.InspectService(ctx, "proj-test-web-b")
	if err != nil || !sameSpec(live.Spec, old) {
		t.Fatalf("old role changed: %+v %v", live, err)
	}
	op, err = s.Operation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != "failed" {
		t.Fatalf("operation state=%s, want failed for create-only compensation", op.State)
	}
	if _, err = s.Lock(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("lock after compensation: %v", err)
	}
}

type slowPreUpdateEngine struct {
	*budgetEngine
	blocked                bool
	stoppedAt              time.Time
	bUpdates, bHealthReads int
}

func (f *slowPreUpdateEngine) InspectService(ctx context.Context, ref string) (engine.ServiceSnapshot, error) {
	if !f.blocked && !f.start.IsZero() && ref == "proj-test-web-b" && time.Since(f.start) >= 5*time.Second {
		f.blocked = true
		<-ctx.Done()
		f.stoppedAt = time.Now()
		return engine.ServiceSnapshot{}, ctx.Err()
	}
	return f.budgetEngine.InspectService(ctx, ref)
}

func (f *slowPreUpdateEngine) UpdateService(ctx context.Context, id string, version uint64, spec json.RawMessage, auth *engine.RegistryAuth) error {
	if id == "old-b" {
		f.bUpdates++
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.budgetEngine.UpdateService(ctx, id, version, spec, auth)
}

func (f *slowPreUpdateEngine) InspectContainerHealth(ctx context.Context, id string) (string, error) {
	if id == "c-old-b" {
		f.bHealthReads++
		return "unhealthy", nil
	}
	return f.budgetEngine.InspectContainerHealth(ctx, id)
}

func TestUnwindIgnoresUpdateStoppedBeforeMutation(t *testing.T) {
	s := openStore(t)
	base := &budgetEngine{boundFake: &boundFake{}}
	ctx := context.Background()
	prior, _, err := s.Admit(ctx, state.Admission{RequestID: "prior-ab", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(budgetManifest())
	if err != nil {
		t.Fatal(err)
	}
	oldB := json.RawMessage(`{"Name":"proj-test-web-b","TaskTemplate":{"ContainerSpec":{"Image":"example/web@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","Labels":{"berthy.operation":"previous"}}},"Mode":{"Replicated":{"Replicas":1}}}`)
	oldA, err := renamed(oldB, "proj-test-web-a")
	if err != nil {
		t.Fatal(err)
	}
	bindings := []state.Binding{
		{App: "web", Role: "a", Name: "proj-test-web-a", ServiceID: "old"},
		{App: "web", Role: "b", Name: "proj-test-web-b", ServiceID: "old-b"},
	}
	if err = s.Finish(ctx, prior.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "web", Digest: digestOfLetter("b"), Manifest: raw, Bindings: bindings}}); err != nil {
		t.Fatal(err)
	}
	base.add("old", oldA)
	base.add("old-b", oldB)
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "inspection-cutoff", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	f := &slowPreUpdateEngine{budgetEngine: base}
	c := &Controller{Store: s, Engine: f, Environment: "test"}
	synctest.Test(t, func(t *testing.T) {
		runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		c.execute(runCtx, op, Actor{ID: "a", Admin: true}, Request{RequestID: "inspection-cutoff", App: "web", Version: digestOfLetter("a"), Manifest: budgetManifest()})
	})
	if !f.blocked || f.stoppedAt.Sub(f.start) != 15*time.Second {
		t.Fatalf("pre-update inspection blocked=%v stopped at %v", f.blocked, f.stoppedAt.Sub(f.start))
	}
	if f.bUpdates != 0 {
		t.Fatalf("untouched role B was updated %d times", f.bUpdates)
	}
	intents, err := s.Intents(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	sawA, sawB := false, false
	for _, i := range intents {
		if i.Role == "a" && i.Kind == "update" && i.Phase == "verified" {
			sawA = true
		}
		if i.Role == "b" && i.Kind == "update" {
			sawB = true
		}
	}
	if !sawB {
		t.Fatal("update intent was not recorded before inspection")
	}
	if !sawA {
		t.Fatal("role A did not reach verification")
	}
	live, err := base.boundFake.InspectService(ctx, "proj-test-web-a")
	if err != nil {
		t.Fatal(err)
	}
	if !sameSpec(live.Spec, oldA) {
		t.Error("role A was not restored to its predecessor spec")
	}
	live, err = base.boundFake.InspectService(ctx, "proj-test-web-b")
	if err != nil || !sameSpec(live.Spec, oldB) {
		t.Fatalf("role B changed: %+v %v", live, err)
	}
	if f.bHealthReads != 0 {
		t.Errorf("compensation polled untouched role B %d times", f.bHealthReads)
	}
	op, err = s.Operation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != "rolled_back" {
		t.Errorf("operation state=%s, want rolled_back", op.State)
	}
	if _, err = s.Lock(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("lock after compensation: %v", err)
	}
}

type uncertainUpdateFake struct{ mutationFake }

func (f *uncertainUpdateFake) UpdateService(ctx context.Context, id string, version uint64, spec json.RawMessage, auth *engine.RegistryAuth) error {
	if err := f.mutationFake.UpdateService(ctx, id, version, spec, auth); err != nil {
		return err
	}
	f.inspectErr = errors.New("inspection unavailable")
	return errors.New("update outcome unavailable")
}
func TestUncertainUpdateIsTreatedAsTouched(t *testing.T) {
	before, desired := json.RawMessage(`{"Name":"svc","version":"old"}`), json.RawMessage(`{"Name":"svc","version":"new"}`)
	f := &uncertainUpdateFake{mutationFake{live: engine.ServiceSnapshot{ID: "id", Version: 9, Spec: before}}}
	err := applyIntent(context.Background(), &journalFake{}, f, "op", state.Intent{Kind: "update", Name: "svc", ServiceID: "id", Before: before, Desired: desired}, nil)
	var beforeMutation beforeServiceMutation
	if err == nil || errors.As(err, &beforeMutation) || f.updateVersion == 0 {
		t.Fatalf("error=%v update version=%d", err, f.updateVersion)
	}
}
