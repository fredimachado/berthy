package state

import (
	"context"
	"errors"
	"testing"
)

func TestCheckOwnerNeverWritesAndRefusesAMissingOwner(t *testing.T) {
	s, _, _ := testStoreEnv(t, "dev")
	ctx := context.Background()
	if err := s.CheckOwner(ctx, "alpha"); !errors.Is(err, ErrNoOwner) {
		t.Fatalf("missing owner: %v", err)
	}
	if err := s.Claim(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckOwner(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	var oe *OwnerError
	if err := s.CheckOwner(ctx, "beta"); !errors.As(err, &oe) || err.Error() != "state belongs to alpha/dev, not beta/dev" {
		t.Fatalf("another project: %v", err)
	}
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
