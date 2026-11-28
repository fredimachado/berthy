package state

import (
	"context"
	"database/sql"
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

func TestConcurrentFirstPromotionAdmission(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	zero := int64(0)
	start := make(chan struct{})
	type result struct {
		a     Admission
		op    Operation
		fresh bool
		err   error
	}
	results := make(chan result, 2)
	for _, id := range []string{"first", "second"} {
		a := Admission{RequestID: id, ActorID: "actor", Kind: "deploy", App: "api", ExpectedApplied: &zero, PublicRequest: json.RawMessage(`{"expected_applied":0}`)}
		go func() { <-start; op, fresh, err := s.Admit(ctx, a); results <- result{a, op, fresh, err} }()
	}
	close(start)
	var winner, loser result
	for range 2 {
		r := <-results
		if r.err == nil && r.fresh {
			if winner.op.ID != "" {
				t.Fatal("two first promotions admitted")
			}
			winner = r
		} else if errors.Is(r.err, ErrConflict) {
			loser = r
		} else {
			t.Fatalf("unexpected admission: %+v", r)
		}
	}
	if winner.op.ID == "" || loser.a.RequestID == "" {
		t.Fatalf("winner %+v loser %+v", winner, loser)
	}
	if err := s.Finish(ctx, winner.op.ID, Outcome{State: "succeeded", Release: &Release{App: "api", Digest: "digest", Manifest: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := s.Admit(ctx, loser.a); fresh || !errors.Is(err, ErrConflict) {
		t.Fatalf("stale first promotion: fresh=%v err=%v", fresh, err)
	}
	if _, err := s.AdmissionForRequest(ctx, loser.a.RequestID, loser.a.ActorID, loser.a.RequestHash); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("loser operation: %v", err)
	}
	if _, err := s.Active(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("active lock: %v", err)
	}
	audit, err := s.AuditLog(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range audit {
		if row.OperationID != winner.op.ID {
			t.Fatalf("unexpected acceptance: %+v", row)
		}
	}
}

func TestLegacyRequestsRetainEmptyAuditAndHistoryTag(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	for _, tc := range []struct{ id, outcome string }{{"old-success", "succeeded"}, {"old-failure", "failed"}} {
		a := Admission{RequestID: tc.id, ActorID: "actor", Kind: "deploy", App: "api", PublicRequest: json.RawMessage(`{"app":"api","version":"sha256:old"}`)}
		op, _, err := s.Admit(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(op.PublicRequest, &fields); err != nil {
			t.Fatal(err)
		}
		if _, ok := fields["version_tag"]; ok {
			t.Fatal("legacy request acquired version_tag")
		}
		outcome := Outcome{State: tc.outcome}
		if tc.outcome == "succeeded" {
			outcome.Release = &Release{App: "api", Digest: "sha256:old", Manifest: json.RawMessage(`{}`)}
		}
		if err := s.Finish(ctx, op.ID, outcome); err != nil {
			t.Fatal(err)
		}
		stored, err := s.Operation(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		fields = nil
		if err := json.Unmarshal(stored.PublicRequest, &fields); err != nil {
			t.Fatal(err)
		}
		if _, ok := fields["version_tag"]; ok {
			t.Fatal("stored legacy request acquired version_tag")
		}
	}
	releases, err := s.History(ctx, "api")
	if err != nil || len(releases) != 1 || releases[0].Tag != "" {
		t.Fatalf("legacy release: %+v %v", releases, err)
	}
	failures, err := s.DeployHistory(ctx, "api")
	if err != nil || len(failures) != 1 || failures[0].Tag != "" {
		t.Fatalf("legacy history: %+v %v", failures, err)
	}
	audit, err := s.AuditLog(ctx, 100)
	if err != nil || len(audit) == 0 {
		t.Fatalf("legacy audit: %+v %v", audit, err)
	}
	for _, row := range audit {
		if row.Tag != "" {
			t.Fatalf("legacy audit tag: %+v", row)
		}
	}
}

func TestHistoryAndAuditRejectInvalidStoredRequest(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, Admission{RequestID: "bad-json", ActorID: "actor", Kind: "deploy", App: "api", PublicRequest: json.RawMessage(`{"app":"api"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, op.ID, Outcome{State: "failed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE operations SET public_request=? WHERE id=?`, `{`, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeployHistory(ctx, "api"); err == nil {
		t.Error("history accepted invalid stored request")
	}
	if _, err := s.AuditLog(ctx, 100); err == nil {
		t.Error("audit accepted invalid stored request")
	}
}
