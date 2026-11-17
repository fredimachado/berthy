package state

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// storedNames lists the names the store holds, in order.
func storedNames(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.Secrets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range rows {
		names = append(names, r.Name)
	}
	return names
}

func TestPutSecretsStoresEveryWriteAndSaysWhatItCreatedAndReplaced(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	actor := Actor{ID: "admin", Name: "Alice", Remote: "local"}
	if err := s.PutSecret(ctx, actor, SecretInfo{Name: "OLD"}, []byte("old-value")); err != nil {
		t.Fatal(err)
	}
	created, replaced, err := s.PutSecrets(ctx, actor, []SecretWrite{
		{Name: "NEW_A", Value: []byte("a")},
		{Name: "NEW_B", Value: []byte("b")},
		{Name: "OLD", Value: []byte("new-value")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 || created[0] != "NEW_A" || created[1] != "NEW_B" || len(replaced) != 1 || replaced[0] != "OLD" {
		t.Fatalf("created %v replaced %v", created, replaced)
	}
	if got := storedNames(t, s); len(got) != 3 {
		t.Fatalf("stored %v, want three secrets", got)
	}
	if _, v, err := s.Secret(ctx, "OLD"); err != nil || string(v) != "new-value" {
		t.Fatalf("OLD: %q %v", v, err)
	}
	if _, v, err := s.Secret(ctx, "NEW_B"); err != nil || string(v) != "b" {
		t.Fatalf("NEW_B: %q %v", v, err)
	}
}

func TestPutSecretsIsAllOrNothing(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	actor := Actor{ID: "admin"}
	if err := s.PutSecret(ctx, actor, SecretInfo{Name: "KEEP"}, []byte("kept")); err != nil {
		t.Fatal(err)
	}
	// The last write fails inside the transaction, after the others have been written.
	if _, err := s.db.Exec(`CREATE TRIGGER fail_last BEFORE INSERT ON secrets WHEN NEW.name='S3' BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.PutSecrets(ctx, actor, []SecretWrite{
		{Name: "KEEP", Value: []byte("changed")},
		{Name: "S1", Value: []byte("one")},
		{Name: "S2", Value: []byte("two")},
		{Name: "S3", Value: []byte("three")},
	})
	if err == nil {
		t.Fatal("the failing write was not reported")
	}
	if got := storedNames(t, s); len(got) != 1 || got[0] != "KEEP" {
		t.Fatalf("stored %v after a failed batch, want only KEEP", got)
	}
	if _, v, err := s.Secret(ctx, "KEEP"); err != nil || string(v) != "kept" {
		t.Fatalf("KEEP changed by a failed batch: %q %v", v, err)
	}
	audit, err := s.AuditLog(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	// The write made before the batch is the only secret row. The failed batch, which wrote S1 and S2 before S3 failed, left none.
	var rows []string
	for _, a := range audit {
		if strings.HasPrefix(a.Action, "secret_set:") {
			rows = append(rows, a.Action)
		}
	}
	if len(rows) != 1 || rows[0] != "secret_set:KEEP" {
		t.Fatalf("audit rows %v after a failed batch, want only secret_set:KEEP", rows)
	}
}

func TestPutSecretsRefusesWhileAnOperationHoldsTheLock(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	actor := Actor{ID: "admin", Name: "Alice", Remote: "local"}
	if err := s.AcquireManual(ctx, actor, "Alice", "maintenance"); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.PutSecrets(ctx, actor, []SecretWrite{{Name: "A", Value: []byte("a")}})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err %v, want ErrConflict", err)
	}
	if got := storedNames(t, s); len(got) != 0 {
		t.Fatalf("stored %v during a lock", got)
	}
}

func TestPutSecretsWritesOneAuditRowPerSecret(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	actor := Actor{ID: "alice", Name: "Alice", Remote: "local"}
	if _, _, err := s.PutSecrets(ctx, actor, []SecretWrite{{Name: "A", Value: []byte("a")}, {Name: "B", Value: []byte("b")}, {Name: "C", Value: []byte("c")}}); err != nil {
		t.Fatal(err)
	}
	audit, err := s.AuditLog(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, a := range audit {
		if a.ActorID == "alice" && a.Outcome == "succeeded" {
			kinds[a.Action]++
		}
	}
	for _, name := range []string{"A", "B", "C"} {
		if kinds["secret_set:"+name] != 1 {
			t.Fatalf("audit %v, want one secret_set:%s row", kinds, name)
		}
	}
}

func TestPutSecretsRefusesARegistryPasswordAndWritesNothing(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	actor := Actor{ID: "admin"}
	if err := s.PutSecret(ctx, actor, SecretInfo{Name: "GHCR_TOKEN", Registry: "ghcr.io"}, []byte("pull")); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.PutSecrets(ctx, actor, []SecretWrite{{Name: "A", Value: []byte("a")}, {Name: "GHCR_TOKEN", Value: []byte("plain")}})
	var rs RegistrySecretError
	if !errors.As(err, &rs) || rs.Name != "GHCR_TOKEN" {
		t.Fatalf("err %v, want a RegistrySecretError for GHCR_TOKEN", err)
	}
	if info, v, err := s.Secret(ctx, "GHCR_TOKEN"); err != nil || info.Registry != "ghcr.io" || string(v) != "pull" {
		t.Fatalf("the registry password changed: %+v %q %v", info, v, err)
	}
	if got := storedNames(t, s); len(got) != 1 {
		t.Fatalf("stored %v, want only the registry password", got)
	}
}

func TestPutSecretsRefusesDuplicateNamesAndEmptyNames(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	actor := Actor{ID: "admin"}
	if _, _, err := s.PutSecrets(ctx, actor, []SecretWrite{{Name: "A", Value: []byte("1")}, {Name: "A", Value: []byte("2")}}); err == nil {
		t.Fatal("a duplicate name was accepted")
	}
	if _, _, err := s.PutSecrets(ctx, actor, []SecretWrite{{Name: "", Value: []byte("1")}}); err == nil {
		t.Fatal("an empty name was accepted")
	}
	if _, _, err := s.PutSecrets(ctx, Actor{}, []SecretWrite{{Name: "A", Value: []byte("1")}}); err == nil {
		t.Fatal("a batch without an actor was accepted")
	}
	if got := storedNames(t, s); len(got) != 0 {
		t.Fatalf("stored %v after refusals", got)
	}
}

func TestClassifySecretsSortsNewFromReplacedAndRefusesRegistryNames(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	actor := Actor{ID: "admin"}
	if err := s.PutSecret(ctx, actor, SecretInfo{Name: "B"}, []byte("b")); err != nil {
		t.Fatal(err)
	}
	create, replace, err := s.ClassifySecrets(ctx, []string{"A", "B", "C"})
	if err != nil {
		t.Fatal(err)
	}
	if len(create) != 2 || create[0] != "A" || create[1] != "C" || len(replace) != 1 || replace[0] != "B" {
		t.Fatalf("create %v replace %v", create, replace)
	}
	if got := storedNames(t, s); len(got) != 1 {
		t.Fatalf("classifying wrote: %v", got)
	}
	if err := s.PutSecret(ctx, actor, SecretInfo{Name: "PULL", Registry: "ghcr.io"}, []byte("p")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClassifySecrets(ctx, []string{"A", "PULL"}); !errors.As(err, new(RegistrySecretError)) {
		t.Fatalf("err %v, want a RegistrySecretError", err)
	}
	if create, replace, err := s.ClassifySecrets(ctx, nil); err != nil || create == nil || replace == nil {
		t.Fatalf("empty classify: %v %v %v, want empty lists and no error", create, replace, err)
	}
}
