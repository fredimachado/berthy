package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/state"
)

func admitRetentionFixture(t *testing.T, s *state.Store, request, kind, app string) state.Operation {
	t.Helper()
	op, _, err := s.Admit(t.Context(), state.Admission{RequestID: request, ActorID: "a", Kind: kind, App: app, PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func finishRetentionFixture(t *testing.T, s *state.Store, op state.Operation, out state.Outcome) {
	t.Helper()
	if err := s.Finish(t.Context(), op.ID, out); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionPreservesRecoveryAndRollbackSources(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	compose := admitRetentionFixture(t, s, "compose", "deploy", "db")
	if err := s.RecordIntent(ctx, compose.ID, state.Intent{Kind: "compose_source", Desired: []byte(`{"compose_file":"services: {db: {image: alpine}}"}`)}); err != nil {
		t.Fatal(err)
	}
	m := []byte(`{"project":"demo","apps":{"db":{"kind":"compose","file":"compose.yaml","deploy":{"retain":1}}}}`)
	finishRetentionFixture(t, s, compose, state.Outcome{State: "succeeded", Release: &state.Release{App: "db", Manifest: m}})
	restart := admitRetentionFixture(t, s, "compose-restart", "restart", "db")
	finishRetentionFixture(t, s, restart, state.Outcome{State: "succeeded"})
	migration := admitRetentionFixture(t, s, "migration", "deploy", "app")
	source, err := json.Marshal(map[string]any{"manifest": revertManifest(), "image": newImage})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []state.Intent{
		{Ordinal: 1, Kind: "schema_source", Desired: source},
		{Ordinal: 2, Kind: "update", Before: []byte(`{"Name":"prior"}`), Desired: []byte(`{"Name":"migration"}`)},
	} {
		if err := s.RecordIntent(ctx, migration.ID, i); err != nil {
			t.Fatal(err)
		}
	}
	finishRetentionFixture(t, s, migration, state.Outcome{State: "failed", SchemaBefore: "v1", SchemaAfter: "v2"})
	for i := range 6 {
		op := admitRetentionFixture(t, s, fmt.Sprint(i), "restart", "app")
		finishRetentionFixture(t, s, op, state.Outcome{State: "succeeded"})
	}
	c := Controller{Store: s, Engine: &positiveEngine{}, Environment: "test"}
	if file, err := c.composeSource(ctx, compose.ID); err != nil || file != "services: {db: {image: alpine}}" {
		t.Errorf("Compose source = %q, error %v", file, err)
	}
	if plan, err := c.schemaRevertPlan(ctx, Request{App: "app"}); err != nil || plan.Source.ID != migration.ID || plan.Target != "v1" {
		t.Errorf("schema revert source %s target %s, error %v", plan.Source.ID, plan.Target, err)
	}
	if intents, err := s.Intents(ctx, migration.ID); err != nil || len(intents) != 1 || intents[0].Kind != "schema_source" {
		t.Errorf("migration has %d retained intents, want only schema_source, error %v", len(intents), err)
	}
	stuck := admitRetentionFixture(t, s, "stuck", "restart", "app")
	if err := s.RecordIntent(ctx, stuck.ID, state.Intent{Kind: "update", Desired: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	finishRetentionFixture(t, s, stuck, state.Outcome{State: "recovery_required"})
	if report, err := c.ResolveRecovery(ctx, Actor{ID: "a", Admin: true}, stuck.ID, "inspected", true); err != nil || report.OperationID != stuck.ID || len(report.Steps) != 1 || report.Resolved {
		t.Fatalf("recovery report %+v, error %v", report, err)
	}
	if active, err := s.Active(ctx); err != nil || active.ID != stuck.ID {
		t.Fatalf("recovery lock: %+v, error %v", active, err)
	}
}

func restartMaintenanceFixture(t *testing.T) (*Controller, *recordingObserver, *sql.DB, string, *[]string) {
	t.Helper()
	dir := t.TempDir()
	s := openStoreAt(t, dir)
	ctx := t.Context()
	first := admitRetentionFixture(t, s, "deploy", "deploy", "app")
	finishRetentionFixture(t, s, first, state.Outcome{State: "succeeded", Release: &state.Release{App: "app", Digest: oldImage, Manifest: []byte(operationsReleaseManifest), Bindings: []state.Binding{{App: "app", Role: "web", Name: "app-web", ServiceID: "id"}}}})
	old := admitRetentionFixture(t, s, "old", "restart", "app")
	if err := s.RecordIntent(ctx, old.ID, state.Intent{Kind: "retention-fixture", Desired: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	finishRetentionFixture(t, s, old, state.Outcome{State: "succeeded"})
	next := admitRetentionFixture(t, s, "next", "restart", "app")
	finishRetentionFixture(t, s, next, state.Outcome{State: "succeeded"})
	db, err := sql.Open("sqlite", filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	obs := &recordingObserver{}
	var maintenance []string
	e := &positiveEngine{spec: []byte(`{"Name":"app-web","TaskTemplate":{"ContainerSpec":{"Image":"example/app:tag"}},"Mode":{"Replicated":{"Replicas":1}}}`)}
	c := &Controller{Engine: e, Store: s, Environment: "test", Observer: obs, ReportMaintenance: func(message string) { maintenance = append(maintenance, message) }}
	t.Cleanup(func() {
		if err := c.Drain(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return c, obs, db, old.ID, &maintenance
}

func TestMaintenanceFailureReportsCommittedRestartOnce(t *testing.T) {
	c, obs, db, old, maintenance := restartMaintenanceFixture(t)
	if _, err := db.Exec(`CREATE TRIGGER fail_retention BEFORE DELETE ON intents WHEN OLD.kind='retention-fixture' BEGIN SELECT RAISE(ABORT,'fixture cleanup error'); END`); err != nil {
		t.Fatal(err)
	}
	op := admitAndDrain(t, c, Request{RequestID: "restart", Kind: "restart", App: "app"})
	if op.State != "succeeded" || len(obs.ended) != 1 || obs.ended[0].OperationID != op.ID || obs.ended[0].State != op.State {
		t.Fatalf("committed restart %s, ends %+v", op.State, obs.ended)
	}
	if len(*maintenance) != 1 || !strings.Contains((*maintenance)[0], op.ID) || strings.Contains((*maintenance)[0], "fixture cleanup error") {
		t.Fatalf("maintenance report %v", *maintenance)
	}
	ctx := t.Context()
	if _, err := c.Store.Active(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("restart held the lock: %v", err)
	}
	if intents, err := c.Store.Intents(ctx, old); err != nil || len(intents) != 1 {
		t.Fatalf("failed maintenance did not preserve old evidence: %v, %v", intents, err)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_retention`); err != nil {
		t.Fatal(err)
	}
	fresh := admitRetentionFixture(t, c.Store, "fresh", "restart", "app")
	if intents, err := c.Store.Intents(ctx, old); err != nil || len(intents) != 0 {
		t.Fatalf("fresh admission did not retry pending cleanup: %v, %v", intents, err)
	}
	finishRetentionFixture(t, c.Store, fresh, state.Outcome{State: "succeeded"})
	if len(obs.ended) != 1 || len(*maintenance) != 1 {
		t.Fatalf("duplicate reporting: %v, %v", obs.ended, *maintenance)
	}
}

func TestUncommittedOutcomeSendsNoTerminalEvent(t *testing.T) {
	c, obs, db, _, maintenance := restartMaintenanceFixture(t)
	if _, err := db.Exec(`CREATE TRIGGER fail_outcome BEFORE UPDATE OF state ON operations WHEN OLD.state='running' BEGIN SELECT RAISE(ABORT,'fixture outcome error'); END`); err != nil {
		t.Fatal(err)
	}
	op := admitAndDrain(t, c, Request{RequestID: "restart", Kind: "restart", App: "app"})
	if op.State != "running" || len(obs.ended) != 0 || len(*maintenance) != 0 {
		t.Fatalf("uncommitted restart %s, ends %+v, maintenance %v", op.State, obs.ended, *maintenance)
	}
}
