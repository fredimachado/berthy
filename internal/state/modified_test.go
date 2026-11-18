package state

import (
	"context"
	"testing"
)

func TestReleaseAndOperationRecordTheModifiedFlag(t *testing.T) {
	s, dir, key := testStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, Admission{RequestID: "release", ActorID: "a", Kind: "deploy", App: "app", Commit: "abc", Modified: true, PublicRequest: []byte(`{"modified":true}`)})
	if err != nil || !op.Modified {
		t.Fatalf("admit: %+v %v", op, err)
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "succeeded", Release: &Release{App: "app", Commit: "abc", Modified: true, Manifest: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	failed, _, err := s.Admit(ctx, Admission{RequestID: "clean", ActorID: "a", Kind: "deploy", App: "app", Commit: "def", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, failed.ID, Outcome{State: "failed", Err: "private error"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	applied, err := s.Applied(ctx, "app")
	if err != nil || !applied.Modified || applied.Commit != "abc" {
		t.Fatalf("applied: %+v %v", applied, err)
	}
	history, err := s.History(ctx, "app")
	if err != nil || len(history) != 1 || !history[0].Modified {
		t.Fatalf("history: %+v %v", history, err)
	}
	rows, err := s.DeployHistory(ctx, "app")
	if err != nil || len(rows) != 1 || rows[0].OperationID != failed.ID || rows[0].Modified {
		t.Fatalf("failed deploys: %+v %v", rows, err)
	}
	// A failed operation that carried the flag keeps it in the history of failed deploys.
	flagged, _, err := s.Admit(ctx, Admission{RequestID: "flagged", ActorID: "a", Kind: "deploy", App: "app", Commit: "ghi", Modified: true, PublicRequest: []byte(`{"modified":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, flagged.ID, Outcome{State: "rolled_back", Err: "private error"}); err != nil {
		t.Fatal(err)
	}
	rows, err = s.DeployHistory(ctx, "app")
	if err != nil || len(rows) != 2 || rows[0].OperationID != flagged.ID || !rows[0].Modified || rows[1].Modified {
		t.Fatalf("failed deploys after a flagged one: %+v %v", rows, err)
	}
	read, err := s.Operation(ctx, flagged.ID)
	if err != nil || !read.Modified || read.Commit != "ghi" {
		t.Fatalf("operation read back: %+v %v", read, err)
	}
}

func TestUpgradeVersionThirteenAddsTheModifiedFlags(t *testing.T) {
	s, dir, key := testStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, Admission{RequestID: "release", ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "succeeded", Release: &Release{App: "app", Digest: "digest", Manifest: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	// A schema 13 database has no modified column on either table.
	if _, err = s.db.Exec(`ALTER TABLE releases DROP COLUMN modified; ALTER TABLE operations DROP COLUMN modified; PRAGMA user_version=13`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	upgraded, err := Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err = upgraded.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 16 {
		t.Fatalf("migration: %d %v", version, err)
	}
	history, err := upgraded.History(ctx, "app")
	if err != nil || len(history) != 1 || history[0].Digest != "digest" || history[0].Modified {
		t.Fatalf("history after upgrade %+v: %v", history, err)
	}
	next, _, err := upgraded.Admit(ctx, Admission{RequestID: "next", ActorID: "a", Kind: "deploy", App: "app", Modified: true, PublicRequest: []byte(`{"modified":true}`)})
	if err != nil || !next.Modified {
		t.Fatalf("admit after upgrade: %+v %v", next, err)
	}
	upgraded.Close()
	again, err := Open(dir, key, "test")
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer again.Close()
	if err = again.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 16 {
		t.Fatalf("second open migration: %d %v", version, err)
	}
}
