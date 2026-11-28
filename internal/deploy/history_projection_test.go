package deploy

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"

	"berthy/internal/engine"
	"berthy/internal/state"
)

// Production Open begins creating these tables when the numbered migration is registered.
func projectionStore(t *testing.T, ddl string, args ...any) *state.Store {
	t.Helper()
	dir := t.TempDir()
	s := openStoreAt(t, dir)
	db, err := sql.Open("sqlite", filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ddl, args...); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRedactorKeepsAnEvictedTemplate(t *testing.T) {
	s := projectionStore(t, `CREATE TABLE IF NOT EXISTS release_facts (environment TEXT, app TEXT, kind TEXT, value TEXT, PRIMARY KEY(environment,app,kind,value));
		INSERT INTO release_facts VALUES ('test','','template',?)`, templateText)
	putSecret(t, s, "POSTGRES_PASSWORD", "pg-s3cret")
	c := Controller{Store: s}
	redact, ok := c.Redactor(t.Context())
	if !ok {
		t.Fatal("redactor unavailable")
	}
	if redact("DATABASE_URL="+templateFilled+" pw=pg-s3cret") != "DATABASE_URL=[redacted] pw=[redacted]" {
		t.Fatal("redaction did not replace the archived template as a whole")
	}
}

type archivedHookEngine struct {
	*pruneEngine
	containers map[string]engine.ContainerSnapshot
	inspected  []string
}

func (e *archivedHookEngine) InspectContainer(_ context.Context, id string) (engine.ContainerSnapshot, error) {
	e.inspected = append(e.inspected, id)
	snapshot, ok := e.containers[id]
	if !ok {
		return engine.ContainerSnapshot{}, &engine.StatusError{Code: 404}
	}
	return snapshot, nil
}

func (e *archivedHookEngine) RemoveContainer(_ context.Context, id string) error {
	delete(e.containers, id)
	return nil
}

func TestPruneUsesAnEvictedOneOffReceipt(t *testing.T) {
	s := projectionStore(t, `CREATE TABLE IF NOT EXISTS prune_oneoffs (environment TEXT, operation_id TEXT, id TEXT, name TEXT, role TEXT, PRIMARY KEY(environment,id));
		INSERT INTO prune_oneoffs VALUES ('test','evicted','hook','owned','pre_hook'), ('test','evicted','missing','missing','pre_hook')`)
	labels := map[string]string{"berthy.environment": "test", "berthy.operation": "evicted", "berthy.oneoff": "pre_hook"}
	e := &archivedHookEngine{pruneEngine: &pruneEngine{runtimeEngine: &runtimeEngine{}}, containers: map[string]engine.ContainerSnapshot{
		"hook": {ID: "hook", Labels: labels}, "foreign": {ID: "foreign", Labels: labels},
	}}
	c := Controller{Store: s, Engine: e, Environment: "test"}
	ctx := t.Context()
	actor := Actor{Admin: true}
	req := Request{Kind: "prune"}
	if _, err := c.planOperation(ctx, actor, req); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.containers["hook"]; !ok {
		t.Fatal("dry-run removed the journaled container")
	}
	for _, request := range []string{"first-prune", "second-prune"} {
		op, _, err := s.Admit(ctx, state.Admission{RequestID: request, ActorID: "a", Kind: "prune", PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		e.inspected = nil
		out := c.runOperation(ctx, op, actor, req)
		if err := s.Finish(ctx, op.ID, out); err != nil || out.State != "succeeded" {
			t.Fatalf("prune outcome %+v, finish error %v", out, err)
		}
		if _, ok := e.containers["hook"]; ok {
			t.Fatal("prune kept the stopped journaled hook")
		}
		if _, ok := e.containers["foreign"]; !ok {
			t.Fatal("prune removed a container with matching labels and a different ID")
		}
		if request == "first-prune" && !slices.Contains(e.inspected, "missing") {
			t.Fatal("dry-run acknowledged an absent container before actual prune")
		}
		if request == "second-prune" && len(e.inspected) != 0 {
			t.Fatalf("prune inspected acknowledged receipts again: %v", e.inspected)
		}
	}
}

func TestRemoveUsesEvictedComposeTargets(t *testing.T) {
	s := projectionStore(t, `CREATE TABLE IF NOT EXISTS compose_down_targets (environment TEXT, project TEXT, kind TEXT, name TEXT, PRIMARY KEY(environment,project,kind,name));
		INSERT INTO compose_down_targets VALUES ('test','shop-test-db','volume','retired-volume')`)
	ctx := t.Context()
	admitRemoval := func(kind, id string) state.Operation {
		t.Helper()
		op, _, err := s.Admit(ctx, state.Admission{RequestID: id, ActorID: "admin", Kind: kind, App: "db", PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	deploy := admitRemoval("deploy", "deploy")
	m := []byte(`{"project":"shop","network":"berthy","apps":{"db":{"kind":"compose","file":"compose.yaml"}}}`)
	if err := s.Finish(ctx, deploy.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "db", Manifest: m}}); err != nil {
		t.Fatal(err)
	}
	old := admitRemoval("remove", "retired-remove")
	if err := s.ForgetApp(ctx, old.ID, "db"); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, old.ID, state.Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	claim := state.Claim{Kind: state.ClaimVolume, Name: "retired-volume"}
	if err := s.AddClaims(ctx, []state.Claim{claim}); err != nil {
		t.Fatal(err)
	}
	c := Controller{Store: s, Engine: newComposeDocker(), Environment: "test"}
	for _, tc := range []struct {
		id      string
		volumes bool
	}{{"without-volumes", false}, {"with-volumes", true}} {
		op := admitRemoval("remove", tc.id)
		out := c.runRemove(ctx, op, Actor{ID: "admin", Admin: true}, Request{App: "db", Volumes: tc.volumes})
		if err := s.Finish(ctx, op.ID, out); err != nil || out.State != "succeeded" {
			t.Fatalf("remove outcome %+v, finish error %v", out, err)
		}
		held, err := s.Claims(ctx)
		if err != nil || slices.Contains(held, claim) == tc.volumes {
			t.Fatalf("volumes=%t: claims %v, error %v", tc.volumes, held, err)
		}
	}
}
