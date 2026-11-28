package state

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestClaimsAreKeptAcrossAReopen(t *testing.T) {
	s, dir, key := testStore(t)
	ctx := context.Background()
	want := []Claim{{Kind: ClaimService, Name: "web"}, {Kind: ClaimVolume, Name: "data"}}
	if err := s.AddClaims(ctx, want); err != nil {
		t.Fatal(err)
	}
	if err := s.AddClaims(ctx, want); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Claims(ctx)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("claims: %v, %v", got, err)
	}
	if err := s.ReleaseClaims(ctx, want); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseClaims(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err = s.Claims(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("released: %v, %v", got, err)
	}
}

func TestAStateOfVersion18GetsTheClaimsTable(t *testing.T) {
	s, dir, key := testStore(t)
	if _, err := s.db.Exec(`DROP TABLE IF EXISTS claims; PRAGMA user_version=18`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("version: %d, %v", version, err)
	}
	if err := s.AddClaims(context.Background(), []Claim{{Kind: ClaimNetwork, Name: "net"}}); err != nil {
		t.Fatal(err)
	}
}

func TestClaimsOnAClosedStoreIsErrClosed(t *testing.T) {
	s, _, _ := testStore(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	s.hold = func() {
		close(entered)
		<-release
	}
	errc := make(chan error, 1)
	go func() {
		_, err := s.Claims(context.Background())
		errc <- err
	}()
	select {
	case <-entered:
	case <-t.Context().Done():
		t.Fatal("Claims never started")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-errc; !errors.Is(err, ErrClosed) {
		t.Fatalf("in-flight Claims: %v, want ErrClosed", err)
	}
	_, err := s.Claims(context.Background())
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("after Close: %v, want ErrClosed", err)
	}
}
