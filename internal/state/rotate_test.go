package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

var rotator = Actor{ID: "admin-id", Name: "alice", Remote: "10.0.0.9:2"}

// fillState puts one value in every sealed column and returns what a reader must still find after a rotation.
func fillState(t *testing.T, s *Store) (opID string) {
	t.Helper()
	ctx := context.Background()
	if err := s.PutSecret(ctx, rotator, SecretInfo{Name: "DATABASE_URL"}, []byte("postgres://hunter2")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSecret(ctx, rotator, SecretInfo{Name: "REGISTRY_PASSWORD", Registry: "registry.example.com"}, []byte("pull-password")); err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(ctx, Admission{RequestID: "deploy-1", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordIntent(ctx, op.ID, Intent{Ordinal: 0, Kind: "service", Before: json.RawMessage(`{"env":"before"}`), Desired: json.RawMessage(`{"env":"desired"}`), Deadline: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordIntent(ctx, op.ID, Intent{Ordinal: 1, Kind: "service", Desired: json.RawMessage(`{"env":"second"}`), Deadline: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkStep(ctx, op.ID, 0, "finished", json.RawMessage(`{"id":"container-1"}`)); err != nil {
		t.Fatal(err)
	}
	if err = s.PutDiagnostics(ctx, op.ID, []json.RawMessage{json.RawMessage(`{"line":"boom"}`), json.RawMessage(`{"line":"bang"}`)}); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "failed", Err: "rollout failed and was cleaned up"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateApproval(ctx, newApproval("secret set DATABASE_URL"), []byte(`{"name":"DATABASE_URL","value":"held-value"}`)); err != nil {
		t.Fatal(err)
	}
	return op.ID
}

// readsEverything checks that every value fillState wrote opens in s.
func readsEverything(t *testing.T, s *Store, opID string) {
	t.Helper()
	ctx := context.Background()
	if _, v, err := s.Secret(ctx, "DATABASE_URL"); err != nil || string(v) != "postgres://hunter2" {
		t.Fatalf("secret: %q %v", v, err)
	}
	if info, v, err := s.Secret(ctx, "REGISTRY_PASSWORD"); err != nil || string(v) != "pull-password" || info.Registry != "registry.example.com" {
		t.Fatalf("registry secret: %+v %q %v", info, v, err)
	}
	intents, err := s.Intents(ctx, opID)
	if err != nil || len(intents) != 2 {
		t.Fatalf("intents: %v %v", intents, err)
	}
	if string(intents[0].Before) != `{"env":"before"}` || string(intents[0].Desired) != `{"env":"desired"}` || string(intents[0].Receipt) != `{"id":"container-1"}` || string(intents[1].Receipt) != `{}` {
		t.Fatalf("intents: %s %s %s %s", intents[0].Before, intents[0].Desired, intents[0].Receipt, intents[1].Receipt)
	}
	diagnostics, err := s.Diagnostics(ctx, opID)
	if err != nil || len(diagnostics) != 2 || string(diagnostics[1]) != `{"line":"bang"}` {
		t.Fatalf("diagnostics: %s %v", diagnostics, err)
	}
	approvals, err := s.Approvals(ctx, "", approvalT0)
	if err != nil || len(approvals) != 1 {
		t.Fatalf("approvals: %v %v", approvals, err)
	}
	if request, err := s.ApprovalRequest(ctx, approvals[0].ID); err != nil || !strings.Contains(string(request), "held-value") {
		t.Fatalf("approval request: %q %v", request, err)
	}
}

func TestRotateKeyResealsEverything(t *testing.T) {
	oldKey := randomKey(t)
	s, dir, keyFile := keyedStore(t, oldKey)
	ctx := context.Background()
	opID := fillState(t, s)
	oldID := s.KeyID()

	rep, err := s.RotateKey(ctx, rotator, false)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	// 2 secrets, 2 intents x 3 columns less the one before that was never set... a missing before is sealed empty, so 6, 2
	// diagnostics, 1 approval and the key check.
	if rep.OldKeyID != oldID || rep.NewKeyID == oldID || rep.NewKeyID != s.KeyID() || rep.Values != 2+6+2+1+1 || rep.KeyFile != keyFile {
		t.Fatalf("report %+v (old %s)", rep, oldID)
	}
	readsEverything(t, s, opID)

	newKey, err := os.ReadFile(keyFile)
	if err != nil || len(newKey) != 32 || bytes.Equal(newKey, oldKey) {
		t.Fatalf("the key file was not replaced: %v", err)
	}
	if prev, err := os.ReadFile(keyFile + ".prev"); err != nil || !bytes.Equal(prev, oldKey) {
		t.Fatalf("the previous key was not kept: %v", err)
	}
	if _, err = os.Stat(keyFile + ".next"); !os.IsNotExist(err) {
		t.Fatalf("the next key file is left: %v", err)
	}
	if actions := auditActions(t, s); !slices.Contains(actions, "alice key_rotate") {
		t.Fatalf("audit: %v", actions)
	}
	s.Close()

	// Only the new key opens the state.
	onlyNew := writeKey(t, keyFile+".only-new", newKey)
	reopened, err := Open(dir, onlyNew, "prod")
	if err != nil {
		t.Fatalf("open with the new key alone: %v", err)
	}
	readsEverything(t, reopened, opID)
	reopened.Close()
	onlyOld := writeKey(t, keyFile+".only-old", oldKey)
	if _, err = Open(dir, onlyOld, "prod"); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("open with the old key alone: %v", err)
	}
}

func TestRotateKeyReportsWhatItWouldDoAndChangesNothing(t *testing.T) {
	oldKey := randomKey(t)
	s, _, keyFile := keyedStore(t, oldKey)
	ctx := context.Background()
	opID := fillState(t, s)
	before := s.KeyID()
	auditBefore := len(auditActions(t, s))
	rep, err := s.RotateKey(ctx, rotator, true)
	if err != nil || rep.Values != 12 || rep.OldKeyID != before || rep.NewKeyID != "" {
		t.Fatalf("dry run: %+v %v", rep, err)
	}
	if s.KeyID() != before || len(auditActions(t, s)) != auditBefore {
		t.Fatal("a dry run changed the key or the audit log")
	}
	if got, _ := os.ReadFile(keyFile); !bytes.Equal(got, oldKey) {
		t.Fatal("a dry run replaced the key file")
	}
	for _, suffix := range []string{".next", ".prev"} {
		if _, err = os.Stat(keyFile + suffix); !os.IsNotExist(err) {
			t.Fatalf("a dry run left %s: %v", suffix, err)
		}
	}
	readsEverything(t, s, opID)
}

func TestRotateKeyIsRefusedWhileTheLockIsHeld(t *testing.T) {
	s, _, keyFile := keyedStore(t, randomKey(t))
	ctx := context.Background()
	if err := s.AcquireManual(ctx, rotator, "alice", "maintenance"); err != nil {
		t.Fatal(err)
	}
	before := s.KeyID()
	for _, dry := range []bool{false, true} {
		if _, err := s.RotateKey(ctx, rotator, dry); !errors.Is(err, ErrConflict) {
			t.Fatalf("rotate (dry run %v) with the lock held: %v", dry, err)
		}
	}
	if err := s.ReleaseManual(ctx, rotator); err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(ctx, Admission{RequestID: "deploy-1", ActorID: "a", Kind: "deploy", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RotateKey(ctx, rotator, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("rotate while an operation runs: %v", err)
	}
	if s.KeyID() != before {
		t.Fatal("a refused rotation changed the key")
	}
	if _, err = os.Stat(keyFile + ".next"); !os.IsNotExist(err) {
		t.Fatalf("a refused rotation left a next key: %v", err)
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RotateKey(ctx, rotator, false); err != nil {
		t.Fatalf("rotate once the lock is free: %v", err)
	}
}

func TestRotateKeyNeedsAnActor(t *testing.T) {
	s, _, _ := keyedStore(t, randomKey(t))
	if _, err := s.RotateKey(context.Background(), Actor{}, false); err == nil {
		t.Fatal("rotated without an actor")
	}
}

// A crash after the next key was written and before the transaction committed leaves a stray next key. The state is still
// sealed with the key file, so Open works and removes the stray.
func TestACrashBeforeTheCommitLeavesTheKeyInUse(t *testing.T) {
	oldKey := randomKey(t)
	s, dir, keyFile := keyedStore(t, oldKey)
	opID := fillState(t, s)
	s.Close()
	writeKey(t, keyFile+".next", randomKey(t))

	reopened, err := Open(dir, keyFile, "prod")
	if err != nil {
		t.Fatalf("open after a crash before the commit: %v", err)
	}
	defer reopened.Close()
	readsEverything(t, reopened, opID)
	if _, err = os.Stat(keyFile + ".next"); !os.IsNotExist(err) {
		t.Fatalf("the stray next key is left: %v", err)
	}
	if got, _ := os.ReadFile(keyFile); !bytes.Equal(got, oldKey) {
		t.Fatal("the key file changed")
	}
}

// A crash after the commit and before the key files were renamed leaves the state sealed with the next key. Open finishes the
// renames, so the key file is the new key and the old one is the previous key.
func TestACrashAfterTheCommitIsFinishedByOpen(t *testing.T) {
	for _, name := range []string{"before any rename", "after the previous key was written"} {
		t.Run(name, func(t *testing.T) {
			oldKey := randomKey(t)
			s, dir, keyFile := keyedStore(t, oldKey)
			opID := fillState(t, s)
			if _, err := s.RotateKey(context.Background(), rotator, false); err != nil {
				t.Fatal(err)
			}
			newKey, err := os.ReadFile(keyFile)
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
			// What the disk held when the agent died: the committed state, the old key still in place, the new one beside it.
			writeKey(t, keyFile, oldKey)
			writeKey(t, keyFile+".next", newKey)
			if name == "before any rename" {
				os.Remove(keyFile + ".prev")
			}

			reopened, err := Open(dir, keyFile, "prod")
			if err != nil {
				t.Fatalf("open after a crash after the commit: %v", err)
			}
			defer reopened.Close()
			readsEverything(t, reopened, opID)
			if got, _ := os.ReadFile(keyFile); !bytes.Equal(got, newKey) {
				t.Fatal("the key file is not the new key")
			}
			if got, _ := os.ReadFile(keyFile + ".prev"); !bytes.Equal(got, oldKey) {
				t.Fatal("the previous key is not the old key")
			}
			if _, err = os.Stat(keyFile + ".next"); !os.IsNotExist(err) {
				t.Fatalf("the next key is left: %v", err)
			}
		})
	}
}

func TestOpenRefusesAKeyThatOpensNeitherTheStateNorItsNextKey(t *testing.T) {
	s, dir, keyFile := keyedStore(t, randomKey(t))
	s.Close()
	other := writeKey(t, keyFile+".other", randomKey(t))
	writeKey(t, other+".next", randomKey(t))
	if _, err := Open(dir, other, "prod"); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("open with a key that is neither: %v", err)
	}
	if _, err := os.Stat(other + ".next"); err != nil {
		t.Fatalf("a refused open removed a key file: %v", err)
	}
}

func TestForgetPreviousKeyWaitsForASnapshotMadeWithTheCurrentKey(t *testing.T) {
	s, _, keyFile := keyedStore(t, randomKey(t))
	ctx := context.Background()
	opID := fillState(t, s)
	old := s.KeyID()
	if _, err := s.RotateKey(ctx, rotator, false); err != nil {
		t.Fatal(err)
	}
	// A snapshot made with the old key does not release the previous key.
	if err := s.ForgetPreviousKey(ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyFile + ".prev"); err != nil {
		t.Fatalf("a snapshot of the old key released the previous key: %v", err)
	}
	if !s.HasPreviousKey() {
		t.Fatal("HasPreviousKey is false")
	}
	if err := s.ForgetPreviousKey(ctx, s.KeyID()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyFile + ".prev"); !os.IsNotExist(err) {
		t.Fatalf("the previous key is kept: %v", err)
	}
	if s.HasPreviousKey() {
		t.Fatal("HasPreviousKey is true")
	}
	readsEverything(t, s, opID)
	if err := s.ForgetPreviousKey(ctx, s.KeyID()); err != nil {
		t.Fatalf("forgetting twice: %v", err)
	}
}

func TestSnapshotNamesTheKeyThatSealedIt(t *testing.T) {
	s, _, _ := keyedStore(t, randomKey(t))
	ctx := context.Background()
	fillState(t, s)
	first, err := s.Snapshot(ctx, t.TempDir()+"/a.db")
	if err != nil || first != s.KeyID() {
		t.Fatalf("snapshot key %q (%v), store key %s", first, err, s.KeyID())
	}
	if _, err = s.RotateKey(ctx, rotator, false); err != nil {
		t.Fatal(err)
	}
	second, err := s.Snapshot(ctx, t.TempDir()+"/b.db")
	if err != nil || second != s.KeyID() || second == first {
		t.Fatalf("snapshot key after a rotation %q (%v), store key %s", second, err, s.KeyID())
	}
}

// TestEverySealedColumnIsRotated reads the schema and fails for a column that can hold a sealed value and is missing from
// sealedColumns: one named sealed, ending in _sealed or named receipt, and any BLOB column that is not listed as plain.
func TestEverySealedColumnIsRotated(t *testing.T) {
	s, _, _ := keyedStore(t, randomKey(t))
	// BLOB columns that hold plain data. A new BLOB column is either one of these or a sealed column in sealedColumns.
	plain := map[string]bool{"operations.request_hash": true, "operations.public_request": true, "events.data": true, "releases.manifest": true, "environment_settings.manifest": true, "environment_settings.apps": true, "approvals.plan": true}
	rotated := map[string]bool{}
	for _, c := range sealedColumns {
		rotated[c.table+"."+c.column] = true
	}
	rows, err := s.db.Query(`SELECT m.name, p.name, p.type FROM sqlite_master m, pragma_table_info(m.name) p WHERE m.type='table' AND m.name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var table, column, typ string
		if err = rows.Scan(&table, &column, &typ); err != nil {
			t.Fatal(err)
		}
		name := table + "." + column
		seen[name] = true
		looksSealed := column == "sealed" || column == "receipt" || strings.HasSuffix(column, "_sealed")
		switch {
		case rotated[name]:
		case looksSealed:
			t.Errorf("%s holds a sealed value and RotateKey does not reseal it: add it to sealedColumns", name)
		case typ == "BLOB" && !plain[name]:
			t.Errorf("%s is a BLOB column that is neither sealed nor listed as plain: if it holds a sealed value, add it to sealedColumns", name)
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	for name := range rotated {
		if !seen[name] {
			t.Errorf("sealedColumns names %s, which is not in the schema", name)
		}
	}
}

func TestRotateKeyKeepsAValueOfAnotherEnvironmentOpenable(t *testing.T) {
	// The state holds one environment's values, but a row of another one must not be lost by a rotation: it is resealed under
	// its own environment.
	s, _, _ := keyedStore(t, randomKey(t))
	ctx := context.Background()
	sealed, err := s.sealFor("other", "secret:X\x00", []byte("other-value"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO secrets VALUES('other','X','',?)`, sealed); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RotateKey(ctx, rotator, false); err != nil {
		t.Fatal(err)
	}
	var got []byte
	if err = s.db.QueryRow(`SELECT sealed FROM secrets WHERE environment='other'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if plain, err := s.openFor("other", "secret:X\x00", got); err != nil || string(plain) != "other-value" {
		t.Fatalf("other environment's value: %q %v", plain, err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT count(*) FROM secrets WHERE environment='other'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("%d %v", count, err)
	}
}

// A secret written while a rotation runs is sealed with the old key before the commit and the new one after it, never the old
// key after the commit: dropping the previous key must not lose it.
func TestSecretsWrittenDuringARotationSurviveDroppingThePreviousKey(t *testing.T) {
	s, _, _ := keyedStore(t, randomKey(t))
	ctx := context.Background()
	stop := make(chan struct{})
	done := make(chan []string)
	go func() {
		var names []string
		for i := 0; ; i++ {
			select {
			case <-stop:
				done <- names
				return
			default:
			}
			name := "S" + strconv.Itoa(i)
			if err := s.PutSecret(ctx, rotator, SecretInfo{Name: name}, []byte("v"+name)); err != nil {
				t.Error(err)
				done <- names
				return
			}
			names = append(names, name)
		}
	}()
	// Held requests are sealed outside any lock, so they are written meanwhile too.
	held := make(chan []string)
	go func() {
		var ids []string
		for len(ids) < MaxPendingApprovals-5 {
			select {
			case <-stop:
				held <- ids
				return
			default:
			}
			a, err := s.CreateApproval(ctx, newApproval("secret set X"), []byte(`{"value":"held"}`))
			if err != nil {
				t.Error(err)
				break
			}
			ids = append(ids, a.ID)
		}
		held <- ids
	}()
	time.Sleep(50 * time.Millisecond)
	if _, err := s.RotateKey(ctx, rotator, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	names, ids := <-done, <-held
	if err := s.ForgetPreviousKey(ctx, s.KeyID()); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if request, err := s.ApprovalRequest(ctx, id); err != nil || string(request) != `{"value":"held"}` {
			t.Fatalf("approval %s after the previous key was dropped: %q %v", id, request, err)
		}
	}
	for _, name := range names {
		if _, v, err := s.Secret(ctx, name); err != nil || string(v) != "v"+name {
			t.Fatalf("%s after the previous key was dropped: %q %v", name, v, err)
		}
	}
}
