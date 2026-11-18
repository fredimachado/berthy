package state

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// finishOp admits one operation of kind on app and finishes it in state.
func finishOp(t *testing.T, s *Store, request, kind, app, state string) {
	t.Helper()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, Admission{RequestID: request, ActorID: "admin", ActorName: "ops", Kind: kind, App: app, PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: state}); err != nil {
		t.Fatal(err)
	}
}

func TestLatestEventsKeepsTheLatestEventOfEachAppOfTheKinds(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	before := time.Now().Add(-time.Second)
	finishOp(t, s, "deploy-web", "deploy", "web", "succeeded")
	finishOp(t, s, "restart-shop", "restart", "shop", "failed")
	finishOp(t, s, "ingress", "ingress", "ingress", "succeeded")
	finishOp(t, s, "exec-web", "exec", "web", "succeeded")

	got, err := s.LatestEvents(ctx, []string{"deploy", "restart"}, before)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["web"].IsZero() || got["shop"].IsZero() {
		t.Fatalf("latest events %v, want web and shop only", got)
	}
	if got["web"].Before(before) {
		t.Fatalf("latest event of web %v is before %v", got["web"], before)
	}

	// Events older than since are not kept.
	got, err = s.LatestEvents(ctx, []string{"deploy", "restart"}, time.Now().Add(time.Hour))
	if err != nil || len(got) != 0 {
		t.Fatalf("events after the window: %v %v", got, err)
	}
	// No kinds, no answer.
	if got, err = s.LatestEvents(ctx, nil, before); err != nil || len(got) != 0 {
		t.Fatalf("no kinds: %v %v", got, err)
	}
}
