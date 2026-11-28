package deploy

import (
	"context"
	"testing"
	"testing/synctest"

	"berthy/internal/manifest"
	"berthy/internal/state"
)

type postTimeoutEngine struct {
	*finalCheckEngine
	waits int
}

func (f *postTimeoutEngine) WaitContainer(ctx context.Context, id string) (int, error) {
	f.waits++
	if f.waits == 4 {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return f.finalCheckEngine.WaitContainer(ctx, id)
}

func TestPostDeployTimeoutRecordsRecovery(t *testing.T) {
	s := openStore(t)
	base := &budgetEngine{boundFake: &boundFake{}}
	seedBudgetUpdate(t, s, base)
	ctx := context.Background()
	m := budgetManifest()
	m.Protected = true
	app := m.Apps["web"]
	delete(app.Roles, "b")
	app.Migrations = &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}}
	app.Hooks.PostDeploy = []manifest.Hook{{Run: []string{"notify"}}}
	m.Apps["web"] = app
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "post-timeout", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	oneOff := newDiagFake()
	oneOff.exits = []int{0, 0, 0}
	oneOff.versions = []string{"v1", "v2"}
	f := &postTimeoutEngine{finalCheckEngine: &finalCheckEngine{budgetEngine: base, store: s, oneOff: oneOff}}
	c := &Controller{Store: s, Engine: f, Environment: "test"}
	synctest.Test(t, func(t *testing.T) {
		c.execute(ctx, op, Actor{ID: "a", Admin: true}, Request{RequestID: "post-timeout", App: "web", Version: digestOfLetter("a"), Manifest: m})
	})
	op, err = s.Operation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != "recovery_required" || op.Err != "post-deploy command outcome uncertain" {
		t.Fatalf("operation state=%s error=%q, want recovery_required and post-deploy error", op.State, op.Err)
	}
	if op.SchemaBefore != "v1" || op.SchemaAfter != "v2" || op.Reason != "post-deploy command 1 timed out" {
		t.Errorf("schema before=%q after=%q reason=%q", op.SchemaBefore, op.SchemaAfter, op.Reason)
	}
	lock, err := s.Lock(ctx)
	if err != nil || lock.OperationID != op.ID {
		t.Errorf("lock = %+v, %v", lock, err)
	}
	events, err := s.EventsAfter(ctx, op.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == "recovery_required" {
			return
		}
	}
	t.Fatal("missing recovery_required event")
}
