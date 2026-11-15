package state

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestImageProvenanceReopenAndOwnership(t *testing.T) {
	s, dir, key := testStore(t)
	ctx := context.Background()
	if err := s.RecordImageAcquired(ctx, "example/app@sha256:abc", "sha256:local"); err == nil {
		t.Fatal("unlocked provenance mutation")
	}
	_, _, err := s.Admit(ctx, Admission{RequestID: "p", ActorID: "a", Kind: "deploy", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordImageAcquired(ctx, "example/app@sha256:abc", "sha256:local"); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordImageAcquired(ctx, "example/app@sha256:abc", "sha256:other"); err == nil {
		t.Fatal("reassigned provenance")
	}
	s.Close()
	s, err = Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	images, err := s.AcquiredImages(ctx)
	if err != nil || len(images) != 1 || images[0].ID != "sha256:local" {
		t.Fatalf("provenance %v %v", images, err)
	}
	if err = s.ForgetImage(ctx, images[0].Ref, "sha256:other"); err == nil {
		t.Fatal("deleted changed image")
	}
	if err = s.ForgetImage(ctx, images[0].Ref, images[0].ID); err != nil {
		t.Fatal(err)
	}
}
func TestUpgradeVersionOne(t *testing.T) {
	s, dir, key := testStore(t)
	if _, err := s.db.Exec(`DROP TABLE acquired_images; ALTER TABLE releases DROP COLUMN operation_id; ALTER TABLE releases DROP COLUMN schema_before; ALTER TABLE releases DROP COLUMN schema_after; PRAGMA user_version=1`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	upgraded, err := Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var version int
	if err = upgraded.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 5 {
		t.Fatalf("migration: %d %v", version, err)
	}
	if _, err = upgraded.AcquiredImages(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestCompletedOneOffsExcludeRecovery(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, Admission{RequestID: "r", ActorID: "a", Kind: "deploy", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordIntent(ctx, op.ID, Intent{Ordinal: 10000, Kind: "oneoff", Role: "pre_hook", Name: "task", Desired: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkStep(ctx, op.ID, 10000, "finished", []byte(`{"id":"container","exit":0}`)); err != nil {
		t.Fatal(err)
	}
	steps, err := s.CompletedOneOffs(ctx)
	if err != nil || len(steps) != 0 {
		t.Fatalf("active steps: %v %v", steps, err)
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "recovery_required"}); err != nil {
		t.Fatal(err)
	}
	steps, err = s.CompletedOneOffs(ctx)
	if err != nil || len(steps) != 0 {
		t.Fatalf("recovery steps: %v %v", steps, err)
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "failed"}); err != nil {
		t.Fatal(err)
	}
	steps, err = s.CompletedOneOffs(ctx)
	if err != nil || len(steps) != 1 || steps[0].ID != "container" {
		t.Fatalf("finished steps: %v %v", steps, err)
	}
}
func TestDeployHistoryShowsFailedOnly(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, Admission{RequestID: "r", ActorID: "private", Kind: "deploy", App: "app", Commit: "abc", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "failed", Err: "private error"}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.DeployHistory(ctx, "app")
	if err != nil || len(rows) != 1 || rows[0].State != "failed" || rows[0].OperationID != op.ID {
		t.Fatalf("history %v %v", rows, err)
	}
}
func TestReleaseRecordsSchemaAndOperation(t *testing.T) {
	s, dir, key := testStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, Admission{RequestID: "release", ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "succeeded", SchemaBefore: "v1", SchemaAfter: "v2", Release: &Release{App: "app", Manifest: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	release, err := s.Applied(ctx, "app")
	if err != nil || release.SchemaBefore != "v1" || release.SchemaAfter != "v2" || release.OperationID != op.ID {
		t.Fatalf("release %v %v", release, err)
	}
	history, err := s.History(ctx, "app")
	if err != nil || len(history) != 1 || history[0].SchemaAfter != "v2" {
		t.Fatalf("history %v %v", history, err)
	}
}
func TestUpgradeVersionTwoPreservesProvenance(t *testing.T) {
	s, dir, key := testStore(t)
	ctx := context.Background()
	_, _, err := s.Admit(ctx, Admission{RequestID: "r", ActorID: "a", Kind: "deploy", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordImageAcquired(ctx, "image@sha256:old", "sha256:id"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`ALTER TABLE releases DROP COLUMN operation_id; ALTER TABLE releases DROP COLUMN schema_before; ALTER TABLE releases DROP COLUMN schema_after; PRAGMA user_version=2`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	images, err := s.AcquiredImages(ctx)
	if err != nil || len(images) != 1 {
		t.Fatalf("lost provenance: %v %v", images, err)
	}
}
func TestUpgradeVersionThreeAddsIngressTables(t *testing.T) {
	s, dir, key := testStore(t)
	ctx := context.Background()
	if err := s.PutSecret(ctx, Actor{ID: "a"}, SecretInfo{Name: "KEPT"}, []byte("kept-value")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE ingress_routes; DROP TABLE ingress; PRAGMA user_version=3`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	upgraded, err := Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var version int
	if err = upgraded.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 5 {
		t.Fatalf("migration: %d %v", version, err)
	}
	if _, value, err := upgraded.Secret(ctx, "KEPT"); err != nil || string(value) != "kept-value" {
		t.Fatalf("secret lost in upgrade: %q %v", value, err)
	}
	if _, err = upgraded.Ingress(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ingress after upgrade: %v", err)
	}
	op, _, err := upgraded.Admit(ctx, Admission{RequestID: "r", ActorID: "a", Kind: "ingress", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = upgraded.SetIngress(ctx, op.ID, IngressState{AccountID: "acct", APITokenSecret: "CLOUDFLARE_API_TOKEN", TunnelID: "t", TunnelName: "demo-prod"}); err != nil {
		t.Fatalf("new table unusable after upgrade: %v", err)
	}
}

func TestUnknownSchemaVersionIsRefused(t *testing.T) {
	s, dir, key := testStore(t)
	if _, err := s.db.Exec(`PRAGMA user_version=6`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(dir, key, "test"); err == nil {
		t.Fatal("opened a newer schema")
	}
}

func TestLatestMigrationPicksNewestRecordedDeploy(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	finish := func(id, kind string, out Outcome) Operation {
		t.Helper()
		op, _, err := s.Admit(ctx, Admission{RequestID: id, ActorID: "a", Kind: kind, App: "app", PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Finish(ctx, op.ID, out); err != nil {
			t.Fatal(err)
		}
		return op
	}
	if _, err := s.LatestMigration(ctx, "app"); err == nil {
		t.Fatal("migration found without history")
	}
	finish("old", "deploy", Outcome{State: "succeeded", SchemaBefore: "v0", SchemaAfter: "v1"})
	finish("plain", "deploy", Outcome{State: "succeeded"})
	finish("rollback", "rollback", Outcome{State: "succeeded", SchemaBefore: "v9", SchemaAfter: "v8"})
	want := finish("new", "deploy", Outcome{State: "failed", SchemaBefore: "v1", SchemaAfter: "v2"})
	held, _, err := s.Admit(ctx, Admission{RequestID: "held", ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, held.ID, Outcome{State: "recovery_required", SchemaBefore: "v2", SchemaAfter: "v3"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestMigration(ctx, "app")
	if err != nil || got.ID != want.ID || got.SchemaBefore != "v1" || got.SchemaAfter != "v2" {
		t.Fatalf("latest migration %+v: %v", got, err)
	}
	if _, err = s.LatestMigration(ctx, "other"); err == nil {
		t.Fatal("migration found for another app")
	}
}
func TestSchemaRevertRequestFieldsArePublic(t *testing.T) {
	s, _, _ := testStore(t)
	if _, _, err := s.Admit(context.Background(), Admission{RequestID: "revert", ActorID: "a", Kind: "rollback", App: "app", PublicRequest: []byte(`{"app":"app","revert_schema":true,"target_release":3}`)}); err != nil {
		t.Fatal(err)
	}
}
