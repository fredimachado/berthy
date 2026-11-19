package state

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	agentActor    = Actor{ID: "agent-id", Name: "claude", Remote: "10.0.0.5:1"}
	approverActor = Actor{ID: "admin-id", Name: "alice", Remote: "10.0.0.9:2"}
	approvalT0    = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
)

func newApproval(summary string) Approval {
	return Approval{ActorID: agentActor.ID, ActorName: agentActor.Name, Remote: agentActor.Remote, Method: "POST", Path: "/v1/environments/test/secrets",
		Summary: summary, Plan: []byte(`{"dry_run":true}`), Created: approvalT0, Expires: approvalT0.Add(15 * time.Minute)}
}

func auditActions(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.AuditLog(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := len(rows) - 1; i >= 0; i-- {
		out = append(out, rows[i].ActorName+" "+rows[i].Action)
	}
	return out
}

func TestApprovalIsStoredWithItsRequestSealed(t *testing.T) {
	s, dir, _ := testStore(t)
	ctx := context.Background()
	a, err := s.CreateApproval(ctx, newApproval("secret set DATABASE_URL"), []byte(`{"name":"DATABASE_URL","value":"postgres://hunter2"}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.Status != "pending" {
		t.Fatalf("created: %+v", a)
	}
	var sealed []byte
	if err = s.db.QueryRow(`SELECT request_sealed FROM approvals WHERE id=?`, a.ID).Scan(&sealed); err != nil || bytes.Contains(sealed, []byte("hunter2")) {
		t.Fatalf("the request is not sealed: %v", err)
	}
	if _, err = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "state.db"))
	if err != nil || bytes.Contains(raw, []byte("hunter2")) {
		t.Fatalf("the secret value is in the database file: %v", err)
	}
	request, err := s.ApprovalRequest(ctx, a.ID)
	if err != nil || !strings.Contains(string(request), "hunter2") {
		t.Fatalf("sealed request unreadable: %q %v", request, err)
	}
	got, err := s.Approval(ctx, a.ID)
	if err != nil || got.Summary != "secret set DATABASE_URL" || got.ActorName != "claude" || got.Method != "POST" || string(got.Plan) != `{"dry_run":true}` || !got.Expires.Equal(approvalT0.Add(15*time.Minute)) {
		t.Fatalf("read back: %+v %v", got, err)
	}
	if actions := auditActions(t, s); len(actions) != 1 || actions[0] != "claude approval_request:secret set DATABASE_URL" {
		t.Fatalf("audit: %v", actions)
	}
}

func TestApprovalsListPendingFirstFilterAndExpire(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	old := newApproval("old")
	old.Created, old.Expires = approvalT0.Add(-time.Hour), approvalT0.Add(-45*time.Minute)
	if _, err := s.CreateApproval(ctx, old, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.CreateApproval(ctx, newApproval("fresh"), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.Approvals(ctx, "pending", approvalT0)
	if err != nil || len(pending) != 1 || pending[0].ID != fresh.ID {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	all, err := s.Approvals(ctx, "", approvalT0)
	if err != nil || len(all) != 2 || all[0].Summary != "fresh" || all[1].Status != "expired" {
		t.Fatalf("all: %+v %v", all, err)
	}
	if actions := auditActions(t, s); actions[len(actions)-1] != "berthy approval_expired" {
		t.Fatalf("audit: %v", actions)
	}
	// Past its expiry the same request is no longer pending.
	later, err := s.Approvals(ctx, "pending", approvalT0.Add(15*time.Minute))
	if err != nil || len(later) != 0 {
		t.Fatalf("expired still pending: %+v %v", later, err)
	}
}

func TestDecideApprovalApprovesOnceAndRecordsTheApprover(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	a, _ := s.CreateApproval(ctx, newApproval("deploy api 1"), []byte(`{}`))
	got, err := s.DecideApproval(ctx, a.ID, approverActor, "approved", "", approvalT0.Add(time.Minute))
	if err != nil || got.Status != "approved" || got.DecidedBy != "alice" || !got.DecidedAt.Equal(approvalT0.Add(time.Minute)) {
		t.Fatalf("approve: %+v %v", got, err)
	}
	if _, err = s.DecideApproval(ctx, a.ID, approverActor, "approved", "", approvalT0.Add(time.Minute)); !errors.Is(err, ErrApprovalNotPending) {
		t.Fatalf("second approve: %v", err)
	}
	if _, err = s.DecideApproval(ctx, a.ID, approverActor, "rejected", "no", approvalT0.Add(time.Minute)); !errors.Is(err, ErrApprovalNotPending) {
		t.Fatalf("reject after approve: %v", err)
	}
	if err = s.FinishApproval(ctx, a.ID, "approved", "operation op-1"); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishApproval(ctx, a.ID, "failed", "again"); !errors.Is(err, ErrApprovalNotPending) {
		t.Fatalf("a result is recorded once: %v", err)
	}
	done, _ := s.Approval(ctx, a.ID)
	if done.Status != "approved" || done.Result != "operation op-1" {
		t.Fatalf("result: %+v", done)
	}
	if actions := auditActions(t, s); actions[len(actions)-1] != "alice approval_approve" {
		t.Fatalf("audit: %v", actions)
	}
	if _, err = s.DecideApproval(ctx, "missing", approverActor, "approved", "", approvalT0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestDecideApprovalRefusesAnExpiredRequestAndMarksItExpired(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	a, _ := s.CreateApproval(ctx, newApproval("deploy api 1"), []byte(`{}`))
	_, err := s.DecideApproval(ctx, a.ID, approverActor, "approved", "", approvalT0.Add(15*time.Minute))
	if !errors.Is(err, ErrApprovalExpired) {
		t.Fatalf("expired approve: %v", err)
	}
	got, _ := s.Approval(ctx, a.ID)
	if got.Status != "expired" {
		t.Fatalf("status %q", got.Status)
	}
	if actions := auditActions(t, s); actions[len(actions)-1] != "alice approval_expired" {
		t.Fatalf("the decider is not in the audit: %v", actions)
	}
	if _, err = s.DecideApproval(ctx, a.ID, approverActor, "approved", "", approvalT0); !errors.Is(err, ErrApprovalNotPending) {
		t.Fatalf("an expired request is decided again: %v", err)
	}
}

func TestDecideApprovalRejectsAndFailsWithAReason(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	r, _ := s.CreateApproval(ctx, newApproval("a"), []byte(`{}`))
	f, _ := s.CreateApproval(ctx, newApproval("b"), []byte(`{}`))
	if got, err := s.DecideApproval(ctx, r.ID, approverActor, "rejected", "not now", approvalT0); err != nil || got.Status != "rejected" || got.Result != "not now" {
		t.Fatalf("reject: %+v %v", got, err)
	}
	if got, err := s.DecideApproval(ctx, f.ID, approverActor, "failed", "the plan changed", approvalT0); err != nil || got.Status != "failed" || got.Result != "the plan changed" {
		t.Fatalf("fail: %+v %v", got, err)
	}
	if _, err := s.DecideApproval(ctx, r.ID, approverActor, "maybe", "", approvalT0); err == nil {
		t.Fatal("an unknown decision is accepted")
	}
	actions := auditActions(t, s)
	if actions[len(actions)-2] != "alice approval_reject" || actions[len(actions)-1] != "alice approval_failed" {
		t.Fatalf("audit: %v", actions)
	}
}

func TestApprovalsKeepTheNewestFiveHundredDecided(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	var first, last Approval
	for i := 0; i < 505; i++ {
		a := newApproval(fmt.Sprintf("n%d", i))
		a.Created = approvalT0.Add(time.Duration(i) * time.Second)
		a.Expires = a.Created.Add(time.Hour)
		created, err := s.CreateApproval(ctx, a, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.DecideApproval(ctx, created.ID, approverActor, "rejected", "", a.Created); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = created
		}
		last = created
	}
	pending, err := s.CreateApproval(ctx, newApproval("pending"), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var decided int
	if err = s.db.QueryRow(`SELECT count(*) FROM approvals WHERE status<>'pending'`).Scan(&decided); err != nil || decided != 500 {
		t.Fatalf("%d decided approvals kept: %v", decided, err)
	}
	if _, err = s.Approval(ctx, first.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("the oldest is kept: %v", err)
	}
	if _, err = s.Approval(ctx, last.ID); err != nil {
		t.Fatalf("the newest is gone: %v", err)
	}
	if got, err := s.Approval(ctx, pending.ID); err != nil || got.Status != "pending" {
		t.Fatalf("a pending approval is pruned: %v", err)
	}
}

func TestCreateApprovalRefusesTooManyPending(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	for i := 0; i < MaxPendingApprovals; i++ {
		if _, err := s.CreateApproval(ctx, newApproval("x"), []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateApproval(ctx, newApproval("x"), []byte(`{}`)); !errors.Is(err, ErrTooManyApprovals) {
		t.Fatalf("over the cap: %v", err)
	}
}

func TestUpgradeVersionElevenAddsApprovalsTable(t *testing.T) {
	s, dir, key := testStore(t)
	if _, err := s.db.Exec(`DROP TABLE approvals; PRAGMA user_version=11`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	upgraded, err := Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var version int
	if err = upgraded.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 18 {
		t.Fatalf("migration: %d %v", version, err)
	}
	if _, err = upgraded.CreateApproval(context.Background(), newApproval("x"), []byte(`{}`)); err != nil {
		t.Fatalf("approvals table unusable after upgrade: %v", err)
	}
}
