package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"testing/synctest"
	"time"

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
	delayed bool
}

func (f *lateFirstRoleEngine) InspectContainerHealth(ctx context.Context, id string) (string, error) {
	if id == "c-new-1" && !f.delayed && time.Since(f.start) >= 5*time.Second {
		f.delayed = true
		if err := waitFakeUntil(ctx, f.start.Add(16*time.Second)); err != nil {
			return "", err
		}
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
		c.execute(ctx, op, Actor{ID: "a", Admin: true}, Request{RequestID: "mixed-cutoff", App: "web", Version: digestOfLetter("a"), Manifest: budgetManifest()})
	})
	if !f.delayed {
		t.Fatal("first role did not reach slow confirmation")
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
