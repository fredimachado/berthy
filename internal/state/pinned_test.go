package state

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestPinnedRequestRetryAndDestinationConflict(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	zero := int64(0)
	a := Admission{RequestID: "one", ActorID: "actor", Kind: "deploy", App: "api", ExpectedApplied: &zero, PublicRequest: json.RawMessage(`{"version_tag":"v1","expected_applied":0}`)}
	op, fresh, err := s.Admit(ctx, a)
	if err != nil || !fresh {
		t.Fatalf("admit: %v %v", fresh, err)
	}
	if err := s.Finish(ctx, op.ID, Outcome{State: "succeeded", Release: &Release{App: "api", Digest: "digest", Tag: "v1", Manifest: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	replay, fresh, err := s.Admit(ctx, a)
	if err != nil || fresh || replay.ID != op.ID {
		t.Fatalf("replay: %+v %v %v", replay, fresh, err)
	}
	a.RequestHash[0] = 1
	if _, _, err := s.Admit(ctx, a); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed request: %v", err)
	}
	a.RequestID = "two"
	if _, _, err := s.Admit(ctx, a); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale destination: %v", err)
	}
	if _, err := s.Active(ctx); err == nil {
		t.Fatal("stale request acquired lock")
	}
	audit, err := s.AuditLog(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range audit {
		if entry.OperationID == op.ID && entry.Tag != "v1" {
			t.Fatalf("audit tag: %+v", entry)
		}
	}
}
