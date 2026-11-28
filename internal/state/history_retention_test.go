package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
)

const retentionManifest = `{"project":"demo","apps":{"api":{"kind":"image","image":"alpine","roles":{"web":{}},"deploy":{"retain":2}},"peer":{"kind":"image","image":"alpine","roles":{"web":{}}}}}`

func retentionAdmit(t *testing.T, s *Store, a Admission) Operation {
	t.Helper()
	op, fresh, err := s.Admit(t.Context(), a)
	if err != nil || !fresh {
		t.Fatalf("admit %s: fresh=%t, error %v", a.RequestID, fresh, err)
	}
	return op
}

func retentionFinish(t *testing.T, s *Store, op Operation, out Outcome) {
	t.Helper()
	if err := s.Finish(t.Context(), op.ID, out); err != nil {
		t.Fatal(err)
	}
}

func counted(t testing.TB, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func BenchmarkFinishWithLegacyOperationHistory(b *testing.B) {
	for range b.N {
		b.StopTimer()
		s, _, _ := testStore(b)
		ctx := b.Context()
		if _, err := s.db.ExecContext(ctx, `
			WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<5000)
			INSERT INTO operations(id,environment,request_id,actor_id,actor_name,remote,kind,app,commit_sha,request_hash,public_request,state,started,error,schema_before,schema_after)
			SELECT 'old-'||i,'test','req-'||i,'actor','','','deploy','api','',zeroblob(32),CAST('{}' AS BLOB),'failed',i,'','','' FROM n;
			WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<25)
			INSERT INTO events(operation_id,kind,data,created)
			SELECT o.id,CASE WHEN i=25 THEN 'failed' ELSE 'step' END,CAST('{}' AS BLOB),o.started FROM operations o CROSS JOIN n;
			INSERT INTO audit(environment,operation_id,actor_id,actor_name,remote,action,outcome,created)
			SELECT environment,id,actor_id,actor_name,remote,kind,'accepted',started FROM operations
			UNION ALL SELECT environment,id,actor_id,actor_name,remote,kind,state,started FROM operations`); err != nil {
			b.Fatal(err)
		}
		op, _, err := s.Admit(ctx, Admission{RequestID: "next", ActorID: "actor", Kind: "deploy", App: "api", PublicRequest: []byte(`{}`)})
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		err = s.Finish(ctx, op.ID, Outcome{State: "failed"})
		b.StopTimer()
		if err != nil {
			b.Fatal(err)
		}
		if n := counted(b, s, `SELECT count(*) FROM operations`); n != 5 {
			b.Fatalf("retained %d operations, want 5", n)
		}
		if n := counted(b, s, `SELECT count(*) FROM events`); n != 102 {
			b.Fatalf("retained %d events, want 102", n)
		}
	}
}

func TestHistoryKeepsNewestReleasesAndApplied(t *testing.T) {
	s, _, _ := testStore(t)
	deploy := func(app, req string) int64 {
		op := retentionAdmit(t, s, Admission{RequestID: req, ActorID: "a", Kind: "deploy", App: app, PublicRequest: []byte(`{}`)})
		retentionFinish(t, s, op, Outcome{State: "succeeded", Release: &Release{App: app, Manifest: []byte(retentionManifest)}})
		r, err := s.Applied(t.Context(), app)
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	peer := deploy("peer", "peer")
	var ids []int64
	for i := range 7 {
		ids = append(ids, deploy("api", fmt.Sprint(i)))
	}
	assertIDs := func(app string, want ...int64) {
		t.Helper()
		history, err := s.History(t.Context(), app)
		if err != nil {
			t.Fatal(err)
		}
		var got []int64
		for _, r := range history {
			got = append(got, r.ID)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s releases %v, want %v", app, got, want)
		}
	}
	assertIDs("api", ids[6], ids[5])
	assertIDs("peer", peer)
	// Move the applied pointer to an older release in a pre-existing history window.
	if _, err := s.db.Exec(`INSERT INTO releases(id,environment,app,digest,commit_sha,manifest,created) VALUES(?,?,?,?,?,?,?)`, ids[0], "test", "api", "", "", []byte(retentionManifest), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE applied SET release_id=? WHERE environment='test' AND app='api'`, ids[0]); err != nil {
		t.Fatal(err)
	}
	op := retentionAdmit(t, s, Admission{RequestID: "restart", ActorID: "a", Kind: "restart", App: "api", PublicRequest: []byte(`{}`)})
	retentionFinish(t, s, op, Outcome{State: "succeeded"})
	assertIDs("api", ids[6], ids[5], ids[0])
	assertIDs("peer", peer)
}

func TestTerminalOperationsAgeOutWithoutARelease(t *testing.T) {
	s, _, _ := testStore(t)
	for _, bucket := range []struct{ app, kind string }{{"api", "deploy"}, {"", "prune"}} {
		for i := range 8 {
			op := retentionAdmit(t, s, Admission{RequestID: fmt.Sprintf("%s-%d", bucket.kind, i), ActorID: "a", Kind: bucket.kind, App: bucket.app, PublicRequest: []byte(`{}`)})
			if err := s.RecordIntent(t.Context(), op.ID, Intent{Kind: "update", Desired: []byte(`{}`)}); err != nil {
				t.Fatal(err)
			}
			retentionFinish(t, s, op, Outcome{State: "failed"})
		}
		for _, tc := range []struct {
			query string
			want  int
		}{
			{`SELECT count(*) FROM operations WHERE app=?`, 5},
			{`SELECT count(*) FROM intents i JOIN operations o ON o.id=i.operation_id WHERE o.app=?`, 5},
			{`SELECT count(*) FROM events e JOIN operations o ON o.id=e.operation_id WHERE o.app=?`, 10},
		} {
			if n := counted(t, s, tc.query, bucket.app); n != tc.want {
				t.Errorf("bucket %q has %d rows, want %d for %s", bucket.app, n, tc.want, tc.query)
			}
		}
		history, err := s.DeployHistory(t.Context(), bucket.app)
		if err != nil || len(history) != 5 {
			t.Errorf("bucket %q failure history length %d, error %v", bucket.app, len(history), err)
		}
	}
}

func TestRetiredRequestCannotStartAnotherOperation(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := t.Context()
	a := Admission{RequestID: "original", ActorID: "original-actor", Kind: "deploy", App: "api", RequestHash: [32]byte{1}, PublicRequest: []byte(`{}`)}
	op := retentionAdmit(t, s, a)
	retentionFinish(t, s, op, Outcome{State: "recovery_required"})
	if err := s.ResolveRecovery(ctx, Actor{ID: "admin"}, op.ID, "operator verified recovery"); err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		other := retentionAdmit(t, s, Admission{RequestID: fmt.Sprintf("new-%d", i), ActorID: "a", App: "api", Kind: "deploy", PublicRequest: []byte(`{}`)})
		retentionFinish(t, s, other, Outcome{State: "failed"})
	}
	if n := counted(t, s, `SELECT count(*) FROM operations WHERE id=?`, op.ID); n != 0 {
		t.Fatalf("original operation still has %d full records", n)
	}
	assertOriginal := func(o Operation, err error) {
		t.Helper()
		if err != nil || o.ID != op.ID || o.State != "failed" || o.App != "api" || o.Kind != "deploy" || !o.Started.Equal(op.Started) {
			t.Fatalf("replayed operation %+v, error %v", o, err)
		}
	}
	assertOriginal(s.AdmissionForRequest(ctx, a.RequestID, a.ActorID, a.RequestHash))
	assertOriginal(s.Operation(ctx, op.ID))
	got, fresh, err := s.Admit(ctx, a)
	assertOriginal(got, err)
	if fresh {
		t.Fatal("retry created an operation")
	}
	badActor, badHash := a, a
	badActor.ActorID, badHash.RequestHash = "other", [32]byte{2}
	for _, conflict := range []Admission{badActor, badHash} {
		if _, err := s.AdmissionForRequest(ctx, a.RequestID, conflict.ActorID, conflict.RequestHash); !errors.Is(err, ErrConflict) {
			t.Fatalf("idempotency conflict: %v", err)
		}
		if _, _, err := s.Admit(ctx, conflict); !errors.Is(err, ErrConflict) {
			t.Fatalf("admit conflict: %v", err)
		}
	}
	if _, err := s.Active(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("replay acquired a lock: %v", err)
	}
	if n := counted(t, s, `SELECT count(*) FROM audit WHERE operation_id=? AND outcome='accepted'`, op.ID); n != 1 {
		t.Fatalf("acceptance audit rows = %d", n)
	}
}

func TestRetiredOperationKeepsAuditAndDiagnosticEvidence(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := t.Context()
	approved, err := s.CreateApproval(ctx, newApproval("run"), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideApproval(ctx, approved.ID, approverActor, "approved", "", approvalT0); err != nil {
		t.Fatal(err)
	}
	var ops []Operation
	for i := range 9 {
		op := retentionAdmit(t, s, Admission{RequestID: fmt.Sprint(i), ActorID: agentActor.ID, ActorName: agentActor.Name, Kind: "run", App: "api", PublicRequest: []byte(`{"version_tag":"v1"}`)})
		ops = append(ops, op)
		if err := s.RecordIntent(ctx, op.ID, Intent{Kind: "update", Desired: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := s.RecordIntent(ctx, op.ID, Intent{Ordinal: 1, Kind: "exec", Desired: []byte(`{}`)}); err != nil {
				t.Fatal(err)
			}
			if err := s.MarkStep(ctx, op.ID, 1, "finished", []byte(`{"output":"approved output"}`)); err != nil {
				t.Fatal(err)
			}
			if err := s.PutDiagnostics(ctx, op.ID, []json.RawMessage{[]byte(`{"detail":"diagnostic"}`)}); err != nil {
				t.Fatal(err)
			}
		}
		retentionFinish(t, s, op, Outcome{State: "failed"})
		if i == 0 {
			if err := s.FinishApproval(ctx, approved.ID, "approved", "operation "+op.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n := counted(t, s, `SELECT count(*) FROM operations WHERE id=?`, ops[1].ID); n != 0 {
		t.Errorf("unpinned operation still has %d rows", n)
	}
	if n := counted(t, s, `SELECT count(*) FROM intents WHERE operation_id=?`, ops[1].ID); n != 0 {
		t.Errorf("unpinned operation still has %d intents", n)
	}
	intents, err := s.Intents(ctx, ops[0].ID)
	if err != nil || len(intents) != 1 || intents[0].Kind != "exec" || string(intents[0].Receipt) != `{"output":"approved output"}` {
		t.Errorf("approved output journal: %+v, error %v", intents, err)
	}
	if d, err := s.Diagnostics(ctx, ops[0].ID); err != nil || len(d) != 1 || string(d[0]) != `{"detail":"diagnostic"}` {
		t.Errorf("diagnostics %s, error %v", d, err)
	}
	if got, err := s.Operation(ctx, ops[0].ID); err != nil || got.State != "failed" {
		t.Errorf("approved operation %+v, error %v", got, err)
	}
	audit, err := s.AuditLog(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var approvedBy bool
	var outcomes []string
	for _, entry := range audit {
		if entry.ActorID == approverActor.ID && entry.Outcome == "approved "+approved.ID {
			approvedBy = true
		}
		if entry.OperationID == ops[1].ID && entry.ActorID == agentActor.ID && entry.Tag == "v1" {
			outcomes = append(outcomes, entry.Outcome)
		}
	}
	if !approvedBy || !slices.Equal(outcomes, []string{"failed", "accepted"}) {
		t.Fatalf("approval attribution=%t, retired tagged audit outcomes=%v", approvedBy, outcomes)
	}
}
