package state

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func version19History(t *testing.T) (dir, keyFile string) {
	t.Helper()
	ctx := t.Context()
	dir = filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	keyFile = filepath.Join(dir, "key")
	rawKey := randomKey(t)
	if err := os.WriteFile(keyFile, rawKey, 0600); err != nil {
		t.Fatal(err)
	}
	key, err := newStateKey(rawKey)
	if err != nil {
		t.Fatal(err)
	}
	check, err := sealWith(key, "", keyCheckName, []byte(keyCheckPlain))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepareDatabase(dir); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	schema, err := os.ReadFile("testdata/schema-v19.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys=ON; `+string(schema)); err != nil {
		t.Fatal(err)
	}
	const oldManifest = `{"project":"demo","apps":{"api":{"kind":"image","image":"alpine","roles":{"web":{}},"deploy":{"retain":2},"env":{"secret":[{"DB_URL":"OLD_DB"}]}}}}`
	if _, err := db.ExecContext(ctx, `INSERT INTO metadata VALUES('key_check',?)`, check); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<7)
		INSERT INTO operations(id,environment,request_id,actor_id,actor_name,remote,kind,app,commit_sha,request_hash,public_request,state,started,error,schema_before,schema_after)
		SELECT 'legacy-'||i,'test','req-'||i,'admin','legacy admin','','deploy','api','',zeroblob(32),CAST('{"version_tag":"v19"}' AS BLOB),
			CASE WHEN i<=4 THEN 'succeeded' ELSE 'failed' END,i,'','','' FROM n;
		INSERT INTO audit(environment,operation_id,actor_id,actor_name,remote,action,outcome,created)
		SELECT environment,id,actor_id,actor_name,remote,kind,'accepted',started FROM operations ORDER BY started;
		INSERT INTO audit(environment,operation_id,actor_id,actor_name,remote,action,outcome,created)
		SELECT environment,id,actor_id,actor_name,remote,kind,state,started FROM operations ORDER BY started;
		INSERT INTO events(operation_id,kind,data,created)
		SELECT id,'accepted',CAST('{}' AS BLOB),started FROM operations
		UNION ALL SELECT id,state,CAST('{}' AS BLOB),started FROM operations;`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO releases(id,environment,app,digest,commit_sha,manifest,created,operation_id,tag)
		SELECT started,environment,app,'digest-'||started,'',CASE WHEN started<=2 THEN ? ELSE ? END,started,id,'v19'
		FROM operations WHERE state='succeeded' ORDER BY started`, []byte(oldManifest), []byte(retentionManifest)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO applied VALUES('test','api',4)`); err != nil {
		t.Fatal(err)
	}
	return dir, keyFile
}

func TestUpgradeVersion19DefersRetentionUntilATerminalWrite(t *testing.T) {
	dir, keyFile := version19History(t)
	ctx := t.Context()
	s, err := Open(dir, keyFile, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if version, err := s.SchemaVersion(ctx); err != nil || version != schemaVersion || version < 20 {
		t.Fatalf("upgraded schema = %d, error %v, want schema 20 or later", version, err)
	}
	assertReleases := func(want []int64) {
		t.Helper()
		releases, err := s.History(ctx, "api")
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, release := range releases {
			ids = append(ids, release.ID)
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("releases %v, want %v", ids, want)
		}
	}
	assertReleases([]int64{4, 3, 2, 1})
	if operations, events := counted(t, s, `SELECT count(*) FROM operations`), counted(t, s, `SELECT count(*) FROM events`); operations != 7 || events != 14 {
		t.Fatalf("upgrade kept %d operations and %d events, want 7 and 14", operations, events)
	}
	if retired := counted(t, s, `SELECT count(*) FROM audit WHERE retired_request_id IS NOT NULL OR retired_release_id IS NOT NULL`); retired != 0 {
		t.Fatalf("upgrade retired %d audit references before the first write", retired)
	}
	op := retentionAdmit(t, s, Admission{RequestID: "first-new-write", ActorID: "admin", Kind: "deploy", App: "api", PublicRequest: []byte(`{}`)})
	retentionFinish(t, s, op, Outcome{State: "failed"})
	assertReleases([]int64{4, 3})
	if physical := counted(t, s, `SELECT count(*) FROM operations WHERE id NOT IN ('legacy-3','legacy-4','legacy-7',?)`, op.ID); physical != 0 {
		t.Fatalf("first write left %d unexpected operation rows", physical)
	}
	if n := counted(t, s, `SELECT count(*) FROM operations`); n != 4 {
		t.Fatalf("first write left %d operation rows, want 4 including two release producers", n)
	}
	if vars, err := s.HistoricalSensitiveVars(ctx, "api"); err != nil || !slices.Equal(vars, []string{"DB_URL"}) {
		t.Fatalf("retired legacy variables = %v, error %v", vars, err)
	}
	if names, err := s.HistoricalSecretNames(ctx, "api"); err != nil || !slices.Equal(names, []string{"OLD_DB"}) {
		t.Fatalf("retired legacy secret names = %v, error %v", names, err)
	}
	if n := counted(t, s, `SELECT count(*) FROM audit WHERE retired_app='api' AND retired_release_id=1 AND retired_request_id='req-1' AND retired_tag='v19'`); n != 1 {
		t.Fatalf("retired legacy acceptance and release references = %d, want 1", n)
	}
	if n := counted(t, s, `SELECT count(*) FROM pragma_foreign_key_check`); n != 0 {
		t.Fatalf("foreign key violations after retention = %d", n)
	}
}
