package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// failedOperation admits and finishes one failed deploy of app, storing items as its diagnostics.
func failedOperation(t *testing.T, s *Store, n int, app string, items ...string) string {
	t.Helper()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, Admission{RequestID: fmt.Sprintf("r%d", n), ActorID: "a", Kind: "deploy", App: app, PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) > 0 {
		raw := make([]json.RawMessage, len(items))
		for i, item := range items {
			raw[i] = json.RawMessage(item)
		}
		if err = s.PutDiagnostics(ctx, op.ID, raw); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "failed", Err: "rollout failed and was cleaned up", Reason: "role web: task exited with code 1"}); err != nil {
		t.Fatal(err)
	}
	return op.ID
}

func TestFinishRecordsReasonInOperationEventAndHistory(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	id := failedOperation(t, s, 1, "web")
	op, err := s.Operation(ctx, id)
	if err != nil || op.Reason != "role web: task exited with code 1" || op.Err != "rollout failed and was cleaned up" {
		t.Fatalf("operation %+v: %v", op, err)
	}
	events, err := s.EventsAfter(ctx, id, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Kind != "failed" || string(last.Data) != `{"reason":"role web: task exited with code 1","state":"failed"}` && string(last.Data) != `{"state":"failed","reason":"role web: task exited with code 1"}` {
		t.Fatalf("event %s %s", last.Kind, last.Data)
	}
	history, err := s.DeployHistory(ctx, "web")
	if err != nil || len(history) != 1 || history[0].Reason != "role web: task exited with code 1" {
		t.Fatalf("history %+v: %v", history, err)
	}
}

func TestFinishWithoutReasonKeepsPlainEvent(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, Admission{RequestID: "r", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "failed", Err: "x"}); err != nil {
		t.Fatal(err)
	}
	events, _ := s.EventsAfter(ctx, op.ID, 0, 10)
	if got := string(events[len(events)-1].Data); got != `{}` {
		t.Fatalf("event %s", got)
	}
}

func TestDiagnosticsAreSealedAndReadBack(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	id := failedOperation(t, s, 1, "web", `{"kind":"task","logs":"DATABASE_URL=postgres://plaintext-marker"}`, `{"kind":"oneoff"}`)
	var raw []byte
	rows, err := s.db.Query(`SELECT sealed FROM diagnostics`)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		if err = rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		n++
		if bytes.Contains(raw, []byte("plaintext-marker")) || bytes.Contains(raw, []byte(`"kind"`)) {
			t.Fatal("diagnostics are stored in plaintext")
		}
	}
	rows.Close()
	if n != 2 {
		t.Fatalf("stored %d rows", n)
	}
	got, err := s.Diagnostics(ctx, id)
	if err != nil || len(got) != 2 || !strings.Contains(string(got[0]), "plaintext-marker") || string(got[1]) != `{"kind":"oneoff"}` {
		t.Fatalf("diagnostics %s: %v", got, err)
	}
	if none, err := s.Diagnostics(ctx, "missing"); err != nil || len(none) != 0 {
		t.Fatalf("unknown operation: %v %v", none, err)
	}
}

func TestDiagnosticsRowsCannotMoveBetweenOperations(t *testing.T) {
	s, _, _ := testStore(t)
	a := failedOperation(t, s, 1, "web", `{"kind":"task"}`)
	b := failedOperation(t, s, 2, "web", `{"kind":"oneoff"}`)
	if _, err := s.db.Exec(`UPDATE diagnostics SET operation_id=?, seq=5 WHERE operation_id=?`, b, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Diagnostics(context.Background(), b); err == nil {
		t.Fatal("a sealed diagnostic was accepted under another operation")
	}
}

func TestPutDiagnosticsRequiresTheOperationLockAndBoundsSize(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	id := failedOperation(t, s, 1, "web")
	if err := s.PutDiagnostics(ctx, id, []json.RawMessage{json.RawMessage(`{}`)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("finished operation accepted diagnostics: %v", err)
	}
	op, _, err := s.Admit(ctx, Admission{RequestID: "r2", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	huge := json.RawMessage(`{"logs":"` + strings.Repeat("x", 3<<20) + `"}`)
	if err = s.PutDiagnostics(ctx, op.ID, []json.RawMessage{huge}); err == nil {
		t.Fatal("oversized diagnostics were accepted")
	}
	if err = s.PutDiagnostics(ctx, op.ID, []json.RawMessage{json.RawMessage(`not json`)}); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
}

func TestDiagnosticsKeepTheNewest50Operations(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	var ids []string
	for i := 0; i < 52; i++ {
		ids = append(ids, failedOperation(t, s, i, "web", fmt.Sprintf(`{"kind":"task","name":"n%d"}`, i)))
	}
	for i, id := range ids {
		got, err := s.Diagnostics(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if kept := len(got) == 1; kept != (i >= 2) {
			t.Fatalf("operation %d kept=%v", i, kept)
		}
	}
	// Operations without diagnostics do not push older diagnostics out.
	for i := 100; i < 110; i++ {
		failedOperation(t, s, i, "web")
	}
	if got, _ := s.Diagnostics(ctx, ids[2]); len(got) != 1 {
		t.Fatal("operations without diagnostics evicted diagnostics")
	}
}

func TestUpgradeVersionSevenAddsReasonAndDiagnostics(t *testing.T) {
	s, dir, key := testStore(t)
	if _, err := s.db.Exec(`DROP TABLE diagnostics; ALTER TABLE operations DROP COLUMN reason; PRAGMA user_version=7`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	upgraded, err := Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var version int
	if err = upgraded.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 14 {
		t.Fatalf("migration: %d %v", version, err)
	}
	id := failedOperation(t, upgraded, 1, "web", `{"kind":"task"}`)
	op, err := upgraded.Operation(context.Background(), id)
	if err != nil || op.Reason == "" {
		t.Fatalf("reason unusable after upgrade: %+v %v", op, err)
	}
	if got, err := upgraded.Diagnostics(context.Background(), id); err != nil || len(got) != 1 {
		t.Fatalf("diagnostics unusable after upgrade: %v %v", got, err)
	}
}

func TestReasonSurvivesTheFinishThatFollowsRecovery(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, Admission{RequestID: "r", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "recovery_required", Err: "one-off outcome uncertain", Reason: "pre-deploy command 1 timed out"}); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "interrupted", Err: "operation interrupted and cleaned up"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Operation(ctx, op.ID)
	if err != nil || got.State != "interrupted" || got.Reason != "pre-deploy command 1 timed out" {
		t.Fatalf("operation %s reason %q: %v", got.State, got.Reason, err)
	}
}
