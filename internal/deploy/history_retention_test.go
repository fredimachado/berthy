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

	"berthy/internal/manifest"
	"berthy/internal/state"
)

func TestRollbackReportsRetiredTarget(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	m := revertManifest()
	a := m.Apps["app"]
	keep := 2
	a.Deploy.Retain = &keep
	m.Apps["app"] = a
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		op := admitRetentionFixture(t, s, fmt.Sprint(i), "deploy", "app")
		finishRetentionFixture(t, s, op, state.Outcome{State: "succeeded", Release: &state.Release{App: "app", Digest: oldImage, Manifest: raw}})
		if i == 0 {
			migration := admitRetentionFixture(t, s, "migration", "deploy", "app")
			source, err := json.Marshal(map[string]any{"manifest": m, "image": newImage})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.RecordIntent(ctx, migration.ID, state.Intent{Kind: "schema_source", Desired: source}); err != nil {
				t.Fatal(err)
			}
			finishRetentionFixture(t, s, migration, state.Outcome{State: "failed", SchemaBefore: "v1", SchemaAfter: "v2"})
		}
	}
	peerRaw, err := json.Marshal(apiManifest(nil))
	if err != nil {
		t.Fatal(err)
	}
	peer := admitRetentionFixture(t, s, "peer", "deploy", "api")
	finishRetentionFixture(t, s, peer, state.Outcome{State: "succeeded", Release: &state.Release{App: "api", Digest: newDigest, Manifest: peerRaw}})
	current, err := s.Applied(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.Applied(ctx, "api")
	if err != nil {
		t.Fatal(err)
	}
	c := &Controller{Store: s, Engine: &positiveEngine{}, Environment: "test"}
	actor := Actor{ID: "admin", Admin: true}
	const want = "rollback release is outside retained history; choose a release from berthy history"
	req := Request{Kind: "rollback", App: "app", RequestID: "rollback-admission", TargetRelease: current.ID - 2}
	check := func(label string, err error) {
		t.Helper()
		var refusal Refusal
		if !errors.As(err, &refusal) || string(refusal) != want {
			t.Errorf("%s = %v, want retired release refusal", label, err)
		}
	}
	_, err = c.Plan(ctx, actor, req)
	check("plan", err)
	_, fresh, err := c.Admit(ctx, actor, req)
	if err := c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	check("admission", err)
	if fresh {
		t.Error("admitted a rollback to a retired release")
	}
	op := admitRetentionFixture(t, s, "rollback-worker", "rollback", "app")
	c.execute(ctx, op, actor, req)
	if finished, err := s.Operation(ctx, op.ID); err != nil || finished.State != "failed" || finished.Err != want || !PublicMessage(want) {
		t.Errorf("worker = %s, %q, error %v, want a public retired release refusal", finished.State, finished.Err, err)
	}
	schema := req
	schema.RevertSchema = true
	_, err = c.schemaRevertPlan(ctx, schema)
	check("explicit schema target", err)
	schema.RequestID = "schema-admission"
	_, fresh, err = c.Admit(ctx, actor, schema)
	if err := c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	check("explicit schema admission", err)
	if fresh {
		t.Error("admitted a schema revert to a retired release")
	}
	for _, id := range []int64{foreign.ID, current.ID + 100} {
		other := req
		other.TargetRelease = id
		if _, err := c.selectRollback(ctx, other); err == nil || err.Error() != "rollback release unavailable" {
			t.Errorf("unavailable release %d = %v", id, err)
		}
	}
	schema.TargetRelease = current.ID
	if _, err := c.schemaRevertPlan(ctx, schema); !errors.Is(err, errTargetMissing) {
		t.Errorf("invalid retained schema target = %v", err)
	}
	if applied, err := s.Applied(ctx, "app"); err != nil || applied.ID != current.ID {
		t.Fatalf("applied after refusals = %d, error %v", applied.ID, err)
	}
}

func TestHistoryDiffReportsExpiredPredecessor(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	c := &Controller{Store: s}
	keep := 2
	m := apiManifest(func(_ *manifest.Manifest, a *manifest.App) { a.Deploy.Retain = &keep })
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var releases []int64
	for i := range 3 {
		op := admitRetentionFixture(t, s, fmt.Sprint(i), "deploy", "api")
		finishRetentionFixture(t, s, op, state.Outcome{State: "succeeded", Release: &state.Release{App: "api", Digest: newDigest, Manifest: raw}})
		r, err := s.Applied(ctx, "api")
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, r.ID)
		if i == 0 {
			if diff, err := c.DiffReleases(ctx, "api", r.ID, 0); err != nil || !diff.New {
				t.Fatalf("first-ever release diff = %+v, error %v", diff, err)
			}
		}
	}
	const want = "previous release is outside retained history"
	if diff, err := c.DiffReleases(ctx, "api", releases[1], 0); err == nil || err.Error() != want {
		t.Errorf("oldest retained release diff = %+v, error %v, want %q", diff, err, want)
	}
	if diff, err := c.DiffReleases(ctx, "api", releases[2], 0); err != nil || !diff.Same {
		t.Errorf("implicit retained predecessor diff = %+v, error %v", diff, err)
	}
}

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
	later := admitAndDrain(t, c, Request{RequestID: "later", Kind: "restart", App: "app"})
	if later.State != "succeeded" || len(obs.ended) != 2 || obs.ended[1].OperationID != later.ID || len(*maintenance) != 2 || !strings.Contains((*maintenance)[1], later.ID) {
		t.Fatalf("persistent maintenance failure: restart %s, ends %v, reports %v", later.State, obs.ended, *maintenance)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_retention`); err != nil {
		t.Fatal(err)
	}
	fresh := admitRetentionFixture(t, c.Store, "fresh", "restart", "app")
	if intents, err := c.Store.Intents(ctx, old); err != nil || len(intents) != 1 {
		t.Fatalf("admission changed pending evidence: %v, %v", intents, err)
	}
	finishRetentionFixture(t, c.Store, fresh, state.Outcome{State: "succeeded"})
	if intents, err := c.Store.Intents(ctx, old); err != nil || len(intents) != 0 {
		t.Fatalf("terminal write did not retry pending cleanup: %v, %v", intents, err)
	}
	if len(obs.ended) != 2 || len(*maintenance) != 2 {
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
