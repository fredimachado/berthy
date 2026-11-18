package state

import (
	"context"
	"errors"
	"testing"
)

// generatedAudits counts the secret_generate rows of the store that name secret name and succeeded.
func generatedAudits(t *testing.T, s *Store, name string) int {
	t.Helper()
	rows, err := s.AuditLog(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range rows {
		if a.Action == "secret_generate:"+name && a.Outcome == "succeeded" {
			n++
		}
	}
	return n
}

// TestGenerateSecretStoresAndAuditsTheValueUnderItsOwnKind checks that a generated secret is stored, is reported as new, and has
// an audit row of its own kind.
func TestGenerateSecretStoresAndAuditsTheValueUnderItsOwnKind(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	actor := Actor{ID: "admin", Name: "Alice", Remote: "local"}
	replaced, err := s.GenerateSecret(ctx, actor, "GEN_PASS", false, []byte("generated-value"))
	if err != nil || replaced {
		t.Fatalf("generate: replaced %v, err %v", replaced, err)
	}
	if _, v, err := s.Secret(ctx, "GEN_PASS"); err != nil || string(v) != "generated-value" {
		t.Fatalf("stored %q, %v", v, err)
	}
	if n := generatedAudits(t, s, "GEN_PASS"); n != 1 {
		t.Fatalf("secret_generate:GEN_PASS rows: %d, want 1", n)
	}
}

// TestGenerateSecretRefusesAStoredSecretUnlessItReplacesIt checks that a name already stored is refused without replace, with
// nothing changed and nothing audited, and that with replace it is replaced and says so.
func TestGenerateSecretRefusesAStoredSecretUnlessItReplacesIt(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	actor := Actor{ID: "admin", Name: "Alice", Remote: "local"}
	if err := s.PutSecret(ctx, actor, SecretInfo{Name: "GEN_PASS"}, []byte("in-use")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GenerateSecret(ctx, actor, "GEN_PASS", false, []byte("new")); !errors.Is(err, ErrSecretExists) {
		t.Fatalf("refused generate: %v, want ErrSecretExists", err)
	}
	if _, v, err := s.Secret(ctx, "GEN_PASS"); err != nil || string(v) != "in-use" {
		t.Fatalf("a refused generate changed the value: %q, %v", v, err)
	}
	if n := generatedAudits(t, s, "GEN_PASS"); n != 0 {
		t.Fatalf("a refused generate was audited %d times", n)
	}
	replaced, err := s.GenerateSecret(ctx, actor, "GEN_PASS", true, []byte("new"))
	if err != nil || !replaced {
		t.Fatalf("replacing generate: replaced %v, err %v", replaced, err)
	}
	if _, v, err := s.Secret(ctx, "GEN_PASS"); err != nil || string(v) != "new" {
		t.Fatalf("replaced value: %q, %v", v, err)
	}
}

// TestGenerateSecretRefusesARegistryPassword checks that a name stored as a registry password is refused, even with replace, and
// keeps its value.
func TestGenerateSecretRefusesARegistryPassword(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	actor := Actor{ID: "admin", Name: "Alice", Remote: "local"}
	if err := s.PutSecret(ctx, actor, SecretInfo{Name: "GHCR_TOKEN", Registry: "ghcr.io"}, []byte("pull")); err != nil {
		t.Fatal(err)
	}
	_, err := s.GenerateSecret(ctx, actor, "GHCR_TOKEN", true, []byte("generated"))
	var rs RegistrySecretError
	if !errors.As(err, &rs) || rs.Name != "GHCR_TOKEN" {
		t.Fatalf("generate of a registry password: %v, want RegistrySecretError", err)
	}
	if _, v, err := s.Secret(ctx, "GHCR_TOKEN"); err != nil || string(v) != "pull" {
		t.Fatalf("the registry password changed: %q, %v", v, err)
	}
}

// TestGenerateSecretIsRefusedWhileTheEnvironmentIsLocked checks that the lock is respected as it is for a plain write.
func TestGenerateSecretIsRefusedWhileTheEnvironmentIsLocked(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	actor := Actor{ID: "admin", Name: "Alice", Remote: "local"}
	if err := s.AcquireManual(ctx, actor, "Alice", "maintenance"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GenerateSecret(ctx, actor, "GEN_PASS", false, []byte("v")); !errors.Is(err, ErrConflict) {
		t.Fatalf("generate while locked: %v, want ErrConflict", err)
	}
	if got := storedNames(t, s); len(got) != 0 {
		t.Fatalf("stored %v while locked", got)
	}
}
