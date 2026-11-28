package state

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
)

func TestHistorySchemaToleratesExistingObjects(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(bootstrapSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`ALTER TABLE audit ADD COLUMN retired_tag TEXT;
		ALTER TABLE audit ADD COLUMN retired_request_id TEXT;
		INSERT INTO audit(environment,operation_id,actor_id,actor_name,remote,action,outcome,created,retired_tag,retired_request_id)
		VALUES('test','held','actor','','','deploy','accepted',0,'v1','request')`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := schema20(tx); err != nil {
			t.Fatal(err)
		}
		var tag, request string
		if err := tx.QueryRow(`SELECT retired_tag, retired_request_id FROM audit WHERE operation_id='held'`).Scan(&tag, &request); err != nil || tag != "v1" || request != "request" {
			t.Fatalf("audit row = %q, %q, error %v", tag, request, err)
		}
		var columns, indexes int
		if err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info('audit') WHERE name IN
			('retired_request_id','retired_request_hash','retired_tag','retired_app','retired_release_id')`).Scan(&columns); err != nil || columns != 5 {
			t.Fatalf("audit has %d retirement columns, error %v", columns, err)
		}
		if err := tx.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='index' AND name IN
			('audit_retired_request','audit_retired_release','audit_operation','releases_app_id','operations_app_started','release_facts_secret_reader')`).Scan(&indexes); err != nil || indexes != 6 {
			t.Fatalf("history has %d supporting indexes, error %v", indexes, err)
		}
	}
}

func TestHistoryProjectsFactsFromARelease(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := t.Context()
	op, _, err := s.Admit(ctx, Admission{RequestID: "deploy", ActorID: "admin", Kind: "deploy", App: "api", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	const raw = `{"project":"demo","apps":{
		"api":{"kind":"image","image":"alpine","roles":{"web":{}},"env":{"secret":[{"OLD_PASSWORD":"PASSWORD"}]}},
		"sibling":{"kind":"image","image":"alpine","roles":{"web":{}},"env":{"template":{"DSN":"prefix:${DB_PASS}"}}}}}`
	if err := s.Finish(ctx, op.ID, Outcome{State: "succeeded", Release: &Release{App: "api", Manifest: []byte(raw)}}); err != nil {
		t.Fatal(err)
	}
	release, err := s.Applied(ctx, "api")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := schema20(tx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.rememberRelease(ctx, tx, release); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM applied`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`DELETE FROM releases WHERE id=?`, release.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	secrets, err := s.HistoricalSecretNames(ctx, "api")
	if err != nil || !slices.Equal(secrets, []string{"PASSWORD"}) {
		t.Fatalf("historical secrets %v, error %v", secrets, err)
	}
	vars, err := s.HistoricalSensitiveVars(ctx, "api")
	if err != nil || !slices.Equal(vars, []string{"OLD_PASSWORD"}) {
		t.Fatalf("historical variables %v, error %v", vars, err)
	}
	templates, err := s.HistoricalTemplates(ctx)
	if err != nil || !slices.Equal(templates, []string{"prefix:${DB_PASS}"}) {
		t.Fatalf("historical templates %v, error %v", templates, err)
	}
}

func TestHistoryProjectsEligibleOperationReceipts(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := t.Context()
	op, _, err := s.Admit(ctx, Admission{RequestID: "remove", ActorID: "admin", Kind: "remove", App: "api", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct {
		kind, role, name, phase, receipt string
	}{
		{"oneoff", "pre_hook", "hook-name", "finished", `{"id":"hook","private":"not projected"}`},
		{"oneoff", "exec", "exec-name", "finished", `{"id":"exec"}`},
		{"oneoff", "run", "empty", "created", `{}`},
		{"compose_down", "", "demo-test-api", "issued", `{"targets":[{"Kind":"volume","Name":"db-data"}]}`},
	} {
		ordinal := i + 1
		if err := s.RecordIntent(ctx, op.ID, Intent{Ordinal: ordinal, Kind: tc.kind, Role: tc.role, Name: tc.name, Desired: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkStep(ctx, op.ID, ordinal, tc.phase, []byte(tc.receipt)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Finish(ctx, op.ID, Outcome{State: "interrupted"}); err != nil {
		t.Fatal(err)
	}
	tx, err := s.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := schema20(tx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.rememberOperationReceipts(ctx, tx, op.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM intents WHERE operation_id=?`, op.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	hooks, err := s.CompletedOneOffs(ctx)
	if want := []CompletedOneOff{{OperationID: op.ID, ID: "hook", Name: "hook-name", Role: "pre_hook"}}; err != nil || !slices.Equal(hooks, want) {
		t.Fatalf("historical hooks %v, error %v", hooks, err)
	}
	claims, err := s.HistoricalComposeTargets(ctx, "demo-test-api")
	if want := []Claim{{Kind: ClaimVolume, Name: "db-data"}}; err != nil || !slices.Equal(claims, want) {
		t.Fatalf("historical targets %v, error %v", claims, err)
	}
}
