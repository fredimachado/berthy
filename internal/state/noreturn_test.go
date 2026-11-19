package state

import (
	"context"
	"encoding/json"
	"testing"
)

// The point of no return of an operation is recorded in the same transaction as the step that crosses it, and it is set once.
func TestRecordedIntentCrossesThePointOfNoReturnWithTheStep(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, Admission{RequestID: "r", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Operation(ctx, op.ID); got.NoReturn != "" {
		t.Fatalf("a new operation has crossed: %q", got.NoReturn)
	}
	if err = s.RecordIntent(ctx, op.ID, Intent{Ordinal: 1, Kind: "create", Desired: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Operation(ctx, op.ID); got.NoReturn != "" {
		t.Fatalf("an ordinary step crossed: %q", got.NoReturn)
	}
	if err = s.RecordIntent(ctx, op.ID, Intent{Ordinal: 9999, Kind: "barrier", Desired: json.RawMessage(`{}`), NoReturn: "the deploy already switched traffic"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Operation(ctx, op.ID)
	if err != nil || got.NoReturn != "the deploy already switched traffic" {
		t.Fatalf("operation %+v, error %v", got, err)
	}
	active, err := s.Active(ctx)
	if err != nil || active.NoReturn != got.NoReturn {
		t.Fatalf("active %+v, error %v", active, err)
	}
	// The first reason stays: an operation crosses once.
	if err = s.RecordIntent(ctx, op.ID, Intent{Ordinal: 10000, Kind: "oneoff", Desired: json.RawMessage(`{}`), NoReturn: "another reason"}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Operation(ctx, op.ID); got.NoReturn != "the deploy already switched traffic" {
		t.Fatalf("the reason changed to %q", got.NoReturn)
	}
}

// A state from before schema 18 has no no_return column: opening it adds the column, and the operations it holds have not crossed.
func TestUpgradeVersionSeventeenAddsThePointOfNoReturn(t *testing.T) {
	s, dir, key := testStore(t)
	ctx := context.Background()
	old, _, err := s.Admit(ctx, Admission{RequestID: "old", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`ALTER TABLE operations DROP COLUMN no_return; PRAGMA user_version=17`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	upgraded, err := Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	got, err := upgraded.Operation(ctx, old.ID)
	if err != nil || got.NoReturn != "" {
		t.Fatalf("operation %+v: %v", got, err)
	}
	if err = upgraded.RecordIntent(ctx, old.ID, Intent{Ordinal: 9999, Kind: "barrier", Desired: json.RawMessage(`{}`), NoReturn: "crossed"}); err != nil {
		t.Fatalf("operations lacks the column after the upgrade: %v", err)
	}
	if got, _ = upgraded.Operation(ctx, old.ID); got.NoReturn != "crossed" {
		t.Fatalf("reason %q", got.NoReturn)
	}
}

// A step that fails to record does not cross.
func TestFailedIntentDoesNotCross(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, Admission{RequestID: "r", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordIntent(ctx, op.ID, Intent{Ordinal: 1, Kind: "create", Desired: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	// The same ordinal again is refused by the primary key, so the operation does not cross.
	if err = s.RecordIntent(ctx, op.ID, Intent{Ordinal: 1, Kind: "barrier", Desired: json.RawMessage(`{}`), NoReturn: "x"}); err == nil {
		t.Fatal("a repeated step was recorded")
	}
	if got, _ := s.Operation(ctx, op.ID); got.NoReturn != "" {
		t.Fatalf("reason %q", got.NoReturn)
	}
}
