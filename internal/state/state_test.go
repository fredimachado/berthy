package state

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	return testStoreEnv(t, "test")
}

func testStoreEnv(t *testing.T, env string) (*Store, string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, key, env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir, key
}
func TestAdmissionAndFinalization(t *testing.T) {
	s, dir, key := testStore(t)
	ctx := context.Background()
	a := Admission{RequestID: "request", ActorID: "actor", ActorName: "name", Kind: "deploy", App: "app", PublicRequest: json.RawMessage(`{}`)}
	var wg sync.WaitGroup
	inserted := make(chan bool, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, fresh, err := s.Admit(ctx, a)
			if err != nil {
				t.Error(err)
			}
			inserted <- fresh
		}()
	}
	wg.Wait()
	close(inserted)
	n := 0
	for v := range inserted {
		if v {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("inserted %d", n)
	}
	a.ActorID = "other"
	if _, _, err := s.Admit(ctx, a); err == nil {
		t.Fatal("mismatched actor accepted")
	}
	if err := s.ReleaseManual(ctx, Actor{ID: "test"}); err == nil {
		t.Fatal("unlocked active operation")
	}
	op, err := s.Active(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordIntent(ctx, op.ID, Intent{Ordinal: 1, Kind: "service", Before: json.RawMessage(`{"secret":"baseline"}`), Desired: json.RawMessage(`{"secret":"desired"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, op.ID, Outcome{State: "succeeded", Release: &Release{App: "app", Digest: "sha256:abc", Manifest: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Active(ctx); err == nil {
		t.Fatal("lock not released")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	steps, err := s2.Intents(ctx, op.ID)
	if err != nil || string(steps[0].Before) != `{"secret":"baseline"}` {
		t.Fatalf("reopen: %v %v", steps, err)
	}
	releases, err := s2.History(ctx, "app")
	if err != nil || len(releases) != 1 {
		t.Fatalf("history: %v %v", releases, err)
	}
	events, err := s2.EventsAfter(ctx, op.ID, 0, 10)
	if err != nil || len(events) < 2 {
		t.Fatalf("events: %v %v", events, err)
	}
}
func TestRecoveryAndEncryption(t *testing.T) {
	s, dir, key := testStore(t)
	ctx := context.Background()
	if err := s.PutSecret(ctx, Actor{ID: "test"}, SecretInfo{Name: "password"}, []byte("sensitive-value")); err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(ctx, Admission{RequestID: "r", ActorID: "a", Kind: "deploy", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "recovery_required"}); err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseManual(ctx, Actor{ID: "test"}); err == nil {
		t.Fatal("recovery unlocked")
	}
	s.Close()
	data, err := os.ReadFile(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "sensitive-value" {
		t.Fatal("plaintext")
	}
	if err = os.WriteFile(key, []byte("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(dir, key, "test"); err == nil {
		t.Fatal("wrong key accepted")
	}
}
func TestOwnerConflictAndAtomicRollback(t *testing.T) {
	s, dir, key := testStore(t)
	ctx := context.Background()
	if other, err := Open(dir, key, "test"); err == nil {
		other.Close()
		t.Fatal("second owner accepted")
	}
	op, _, err := s.Admit(ctx, Admission{RequestID: "one", ActorID: "a", Kind: "deploy", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Admit(ctx, Admission{RequestID: "two", ActorID: "b", Kind: "deploy", PublicRequest: []byte(`{}`)}); err == nil {
		t.Fatal("concurrent operation accepted")
	}
	err = s.Finish(ctx, op.ID, Outcome{State: "succeeded", Release: &Release{App: "app", Manifest: []byte(`{}`), Bindings: []Binding{{App: "app", Role: "web", Name: "same", ServiceID: "one"}, {App: "app", Role: "worker", Name: "same", ServiceID: "two"}}}})
	if err == nil {
		t.Fatal("duplicate binding accepted")
	}
	if _, err = s.Active(ctx); err != nil {
		t.Fatalf("failed finish released lock: %v", err)
	}
	history, err := s.History(ctx, "app")
	if err != nil || len(history) != 0 {
		t.Fatalf("failed finish wrote release: %v %v", history, err)
	}
}
func TestSecretMetadataTamper(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	if err := s.PutSecret(ctx, Actor{ID: "test"}, SecretInfo{Name: "cred", Registry: "host"}, []byte("value")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE secrets SET registry='other' WHERE name='cred'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Secret(ctx, "cred"); err == nil {
		t.Fatal("modified registry accepted")
	}
}
func TestAuditAndStickyProtection(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	actor := Actor{ID: "alice", Name: "Alice", Remote: "local"}
	if err := s.PutSecret(ctx, actor, SecretInfo{Name: "token"}, []byte("value")); err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireManual(ctx, actor, "Alice", "maintenance"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseManual(ctx, actor); err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(ctx, Admission{RequestID: "p", ActorID: "alice", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "succeeded", Protected: true, Release: &Release{App: "app", Manifest: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	protected, err := s.Protected(ctx)
	if err != nil || !protected {
		t.Fatalf("protection: %v %v", protected, err)
	}
	audit, err := s.AuditLog(ctx, 10)
	if err != nil || len(audit) != 5 {
		t.Fatalf("audit: %v %v", audit, err)
	}
}

func TestOutcomeAndPrivateReceipt(t *testing.T) {
	s, dir, _ := testStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, Admission{RequestID: "r", ActorID: "a", Kind: "deploy", App: "api", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordIntent(ctx, op.ID, Intent{Desired: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkStep(ctx, op.ID, 0, "sent", []byte(`{"token":"receipt-sensitive"}`)); err != nil {
		t.Fatal(err)
	}
	items, err := s.Intents(ctx, op.ID)
	if err != nil || string(items[0].Receipt) != `{"token":"receipt-sensitive"}` {
		t.Fatalf("receipt: %v", err)
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "failed", Release: &Release{App: "other", Manifest: []byte(`{}`)}}); err == nil {
		t.Fatal("accepted different app")
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "failed", Release: &Release{App: "api", Manifest: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("receipt-sensitive")) {
		t.Fatal("receipt exposed")
	}
}
func TestExistingMissingKeyVerifierFails(t *testing.T) {
	s, dir, key := testStore(t)
	if _, err := s.db.Exec(`DELETE FROM metadata WHERE name='key_check'`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if reopened, err := Open(dir, key, "test"); err == nil {
		reopened.Close()
		t.Fatal("missing verifier accepted")
	}
}
func TestInterruptedAndRolledBackOutcomes(t *testing.T) {
	for _, state := range []string{"interrupted", "rolled_back"} {
		t.Run(state, func(t *testing.T) {
			s, _, _ := testStore(t)
			op, _, err := s.Admit(context.Background(), Admission{RequestID: "r", ActorID: "a", Kind: "deploy", PublicRequest: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Finish(context.Background(), op.ID, Outcome{State: state}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDatabasePathRejectsDSNDelimiters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state#redirect")
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(dir, key, "test"); err == nil {
		s.Close()
		t.Fatal("accepted ambiguous SQLite path")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("created ambiguous state path")
	}
}

func TestPublicDataRejectsPrivateFields(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	if _, _, err := s.Admit(ctx, Admission{RequestID: "bad", ActorID: "a", Kind: "deploy", PublicRequest: []byte(`{"token":"canary"}`)}); err == nil {
		t.Fatal("accepted private request")
	}
	op, _, err := s.Admit(ctx, Admission{RequestID: "good", ActorID: "a", Kind: "deploy", PublicRequest: []byte(`{"app":"api"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AppendEvent(ctx, op.ID, Event{Kind: "step", Data: []byte(`{"token":"canary"}`)}); err == nil {
		t.Fatal("accepted private event")
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "failed", Event: []byte(`{"token":"canary"}`)}); err == nil {
		t.Fatal("accepted private outcome")
	}
}

// TestPublicRequestAcceptsTheKeptVersionAndItsDigest: keep_digest is a digest the client read, not a secret, so a request that
// carries it is recorded.
func TestPublicRequestAcceptsTheKeptVersionAndItsDigest(t *testing.T) {
	s, _, _ := testStore(t)
	public := []byte(`{"app":"api","keep_version":true,"keep_digest":"example/api@sha256:` + strings.Repeat("a", 64) + `"}`)
	if _, _, err := s.Admit(context.Background(), Admission{RequestID: "keep", ActorID: "a", Kind: "deploy", App: "api", PublicRequest: public}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Admit(context.Background(), Admission{RequestID: "bad", ActorID: "a", Kind: "deploy", App: "api", PublicRequest: []byte(`{"keep_digest":true}`)}); err == nil {
		t.Fatal("accepted a keep_digest that is not a string")
	}
}

func TestResolveRecoveryFinishesStuckOperation(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	admin := Actor{ID: "admin-id", Name: "admin", Remote: "10.0.0.2:4000"}
	op, _, err := s.Admit(ctx, Admission{RequestID: "stuck", ActorID: "actor", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ResolveRecovery(ctx, admin, "missing", "service removed"); !errors.Is(err, ErrConflict) {
		t.Fatalf("operation without the lock resolved: %v", err)
	}
	if err = s.ResolveRecovery(ctx, Actor{}, op.ID, "service removed"); err == nil {
		t.Fatal("accepted resolve without an actor")
	}
	if err = s.ResolveRecovery(ctx, admin, op.ID, "service removed"); !errors.Is(err, ErrConflict) {
		t.Fatalf("running operation resolved: %v", err)
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "recovery_required", Err: "service recovery required", SchemaBefore: "v1", SchemaAfter: "v2"}); err != nil {
		t.Fatal(err)
	}
	if err = s.ResolveRecovery(ctx, admin, op.ID, ""); err == nil {
		t.Fatal("accepted resolve without a reason")
	}
	if err = s.ResolveRecovery(ctx, admin, op.ID, "service deleted outside berthy"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Operation(ctx, op.ID)
	if err != nil || got.State != "failed" || got.Err != "recovery resolved by admin" || got.SchemaBefore != "v1" || got.SchemaAfter != "v2" {
		t.Fatalf("operation %+v: %v", got, err)
	}
	if _, err = s.Lock(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("lock still held: %v", err)
	}
	audit, err := s.AuditLog(ctx, 1)
	if err != nil || len(audit) != 1 || audit[0].Action != "recovery_resolve" || audit[0].ActorID != "admin-id" || audit[0].ActorName != "admin" || audit[0].Remote != "10.0.0.2:4000" || audit[0].OperationID != op.ID || audit[0].Outcome != "succeeded" {
		t.Fatalf("audit %+v: %v", audit, err)
	}
	events, err := s.EventsAfter(ctx, op.ID, 0, 100)
	if err != nil || len(events) == 0 {
		t.Fatalf("events %+v: %v", events, err)
	}
	last := events[len(events)-1]
	var data map[string]string
	if last.Kind != "failed" || json.Unmarshal(last.Data, &data) != nil || data["reason"] != "service deleted outside berthy" || data["state"] != "failed" {
		t.Fatalf("last event %+v", last)
	}
	if err = s.ResolveRecovery(ctx, admin, op.ID, "again"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second resolve: %v", err)
	}
}

func TestAdmitReturnsWhenTheOperationStarted(t *testing.T) {
	s, _, _ := testStore(t)
	before := time.Now().Add(-time.Second)
	op, _, err := s.Admit(context.Background(), Admission{RequestID: "r", ActorID: "a", Kind: "deploy", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := s.Operation(context.Background(), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if op.Started.Before(before) || op.Started.After(time.Now().Add(time.Second)) || !op.Started.Equal(stored.Started) {
		t.Fatalf("admitted %v, stored %v", op.Started, stored.Started)
	}
}
