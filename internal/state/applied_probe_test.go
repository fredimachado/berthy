//go:build linux

package state

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"berthy/internal/scope"
)

func TestHasAppliedAppsChecksOwnerAndCommittedWAL(t *testing.T) {
	s, dir, key := testStoreEnv(t, "dev")
	ctx := context.Background()
	id := scope.ID{Project: "alpha", Environment: "dev"}
	if err := s.Claim(ctx, id.Project); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE); PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	check := func(want bool) {
		t.Helper()
		if got, err := HasAppliedApps(ctx, dir, id); err != nil || got != want {
			t.Fatalf("HasAppliedApps = %t, %v, want %t", got, err, want)
		}
	}
	check(false)
	for _, other := range []scope.ID{{Project: "beta", Environment: "dev"}, {Project: "alpha", Environment: "prod"}} {
		var mismatch *OwnerError
		if got, err := HasAppliedApps(ctx, dir, other); got || !errors.As(err, &mismatch) {
			t.Fatalf("read as %s = %t, %v", other, got, err)
		}
	}
	for _, app := range []string{"orphan", "ingress"} {
		op, _, err := s.Admit(ctx, Admission{RequestID: "deploy-" + app, ActorID: "a", Kind: "deploy", App: app, PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Finish(ctx, op.ID, Outcome{State: "succeeded", Release: &Release{App: app, Digest: "digest", Manifest: []byte(`{}`)}}); err != nil {
			t.Fatal(err)
		}
		check(true)
		op, _, err = s.Admit(ctx, Admission{RequestID: "destroy-" + app, ActorID: "a", Kind: "destroy", PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.ForgetApp(ctx, op.ID, app); err != nil {
			t.Fatal(err)
		}
		if err := s.Finish(ctx, op.ID, Outcome{State: "succeeded"}); err != nil {
			t.Fatal(err)
		}
		if history, err := s.History(ctx, app); err != nil || len(history) != 1 {
			t.Fatalf("history for %s = %v, %v", app, history, err)
		}
		check(false)
	}
}

func TestHasAppliedAppsRefusesWALWithoutAnExistingSnapshot(t *testing.T) {
	s, dir, _ := testStoreEnv(t, "dev")
	ctx := context.Background()
	id := scope.ID{Project: "alpha", Environment: "dev"}
	if err := s.Claim(ctx, id.Project); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(snapshot, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"state.db", "state.db-wal"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(snapshot, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := HasAppliedApps(ctx, snapshot, id); got || err == nil {
		t.Fatalf("WAL without shared memory = %t, %v", got, err)
	}
	if entries, err := os.ReadDir(snapshot); err != nil || len(entries) != 2 {
		t.Fatalf("probe created a sidecar: %v, %v", entries, err)
	}
}

func TestHasAppliedAppsEscapesDatabaseURI(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state ?mode=rw# %25")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id := scope.ID{Project: "alpha", Environment: "dev"}
	// Create a clean rollback-journal database before moving it to the URI-sensitive path.
	plain := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", plain)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE metadata (name TEXT PRIMARY KEY, value BLOB NOT NULL);
INSERT INTO metadata VALUES ('project', 'alpha'), ('environment', 'dev');
CREATE TABLE applied (environment TEXT);
INSERT INTO applied VALUES ('prod');`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(plain, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(plain, filepath.Join(dir, "state.db")); err != nil {
		t.Fatal(err)
	}
	if got, err := HasAppliedApps(ctx, dir, id); got || err != nil {
		t.Fatalf("read from URI-sensitive path = %t, %v", got, err)
	}
}
