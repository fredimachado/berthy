package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

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

func TestClaimIsKeptAndRefusesAnotherProject(t *testing.T) {
	s, dir, key := testStoreEnv(t, "dev")
	ctx := context.Background()
	if err := s.Claim(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := Open(dir, key, "dev")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Claim(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	err = s.Claim(ctx, "beta")
	var oe *OwnerError
	if !errors.As(err, &oe) || err.Error() != "state belongs to alpha/dev, not beta/dev" {
		t.Fatalf("another project: %v", err)
	}
}

func TestClaimRefusesAnotherEnvironmentEvenWithoutAnAuditRow(t *testing.T) {
	s, dir, key := testStoreEnv(t, "dev")
	ctx := context.Background()
	if err := s.Claim(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutCheck(ctx, CheckState{Name: "disk", State: "ok"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := Open(dir, key, "prod")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.Claim(ctx, "alpha")
	var oe *OwnerError
	if !errors.As(err, &oe) || err.Error() != "state belongs to alpha/dev, not alpha/prod" {
		t.Fatalf("another environment: %v", err)
	}
}

func TestARefusedClaimLeavesTheStateAsItWas(t *testing.T) {
	s, dir, key := testStoreEnv(t, "dev")
	ctx := context.Background()
	if err := s.Claim(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	err := s.Claim(ctx, "beta")
	var oe *OwnerError
	if !errors.As(err, &oe) {
		t.Fatalf("another project: %v", err)
	}
	if err = s.Claim(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir, key, "dev")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Claim(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	err = s.Claim(ctx, "beta")
	if !errors.As(err, &oe) || err.Error() != "state belongs to alpha/dev, not beta/dev" {
		t.Fatalf("owner changed after a refused claim: %v", err)
	}
}
