package state

import (
	"context"
	"encoding/json"
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
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 19 {
		t.Fatalf("version: %d, %v", version, err)
	}
	if err := s.AddClaims(context.Background(), []Claim{{Kind: ClaimNetwork, Name: "net"}}); err != nil {
		t.Fatal(err)
	}
}

func TestReservationJournalsOnlyNewClaims(t *testing.T) {
	ctx := context.Background()
	s, _, _ := testStore(t)
	old := Claim{Kind: ClaimVolume, Name: "old"}
	fresh := Claim{Kind: ClaimVolume, Name: "fresh"}
	if err := s.AddClaims(ctx, []Claim{old}); err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(ctx, Admission{RequestID: "r", ActorID: "a", Kind: "deploy", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	added, err := s.AddClaimsForOperation(ctx, op.ID, []Claim{old, fresh, fresh})
	if err != nil || !reflect.DeepEqual(added, []Claim{fresh}) {
		t.Fatalf("added %v: %v", added, err)
	}
	intents, err := s.Intents(ctx, op.ID)
	if err != nil || len(intents) != 1 || intents[0].Kind != "claims" {
		t.Fatalf("journal: %v, %v", intents, err)
	}
	var journaled []Claim
	if err := json.Unmarshal(intents[0].Desired, &journaled); err != nil || !reflect.DeepEqual(journaled, added) {
		t.Fatalf("journaled %v: %v", journaled, err)
	}
}
