package deploy

import (
	"berthy/internal/state"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// stuckOperation leaves an operation in recovery_required with one unverified update that still holds the lock.
func stuckOperation(t *testing.T, s *state.Store) state.Operation {
	t.Helper()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "stuck", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordIntent(ctx, op.ID, state.Intent{Kind: "update", Role: "web", Name: "svc", ServiceID: "id", Before: json.RawMessage(`{"Name":"svc","Env":["SECRET=x"]}`), Desired: json.RawMessage(`{"Name":"svc","Env":["SECRET=x"]}`)}); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "recovery_required", Err: "service recovery required"}); err != nil {
		t.Fatal(err)
	}
	return op
}

func TestResolveRecoveryRequiresAdministrator(t *testing.T) {
	s := openStore(t)
	op := stuckOperation(t, s)
	c := Controller{Store: s}
	ctx := context.Background()
	if _, err := c.ResolveRecovery(ctx, Actor{ID: "deployer"}, op.ID, "service removed", false); !errors.Is(err, errResolveAdmin) {
		t.Fatalf("non-admin: %v", err)
	}
	if lock, err := s.Lock(ctx); err != nil || lock.OperationID != op.ID {
		t.Fatalf("lock after refusal: %+v %v", lock, err)
	}
}

func TestResolveRecoveryRefusesOtherOperations(t *testing.T) {
	s := openStore(t)
	op := stuckOperation(t, s)
	c := Controller{Store: s}
	ctx := context.Background()
	if _, err := c.ResolveRecovery(ctx, Actor{ID: "admin", Admin: true}, "other", "service removed", false); !errors.Is(err, errNotRecovering) {
		t.Fatalf("wrong id: %v", err)
	}
	if lock, err := s.Lock(ctx); err != nil || lock.OperationID != op.ID {
		t.Fatalf("wrong id changed the lock: %+v %v", lock, err)
	}
}

func TestResolveRecoveryDryRunShowsStepsWithoutEnvironment(t *testing.T) {
	s := openStore(t)
	op := stuckOperation(t, s)
	c := Controller{Store: s}
	ctx := context.Background()
	report, err := c.ResolveRecovery(ctx, Actor{ID: "admin", Admin: true}, op.ID, "service removed", true)
	if err != nil || report.Resolved || report.OperationID != op.ID || report.Error != "service recovery required" || len(report.Steps) != 1 {
		t.Fatalf("dry run %+v: %v", report, err)
	}
	step := report.Steps[0]
	if step.Kind != "update" || step.Role != "web" || step.Name != "svc" || step.ServiceID != "id" || step.Phase != "intent" {
		t.Fatalf("step %+v", step)
	}
	b, err := json.Marshal(report)
	if err != nil || strings.Contains(string(b), "SECRET") {
		t.Fatalf("report exposes environment: %s %v", b, err)
	}
	if lock, err := s.Lock(ctx); err != nil || lock.OperationID != op.ID {
		t.Fatalf("dry run released the lock: %+v %v", lock, err)
	}
}

func TestResolveRecoveryReleasesLockForRecover(t *testing.T) {
	s := openStore(t)
	op := stuckOperation(t, s)
	c := Controller{Engine: &positiveEngine{}, Store: s}
	ctx := context.Background()
	report, err := c.ResolveRecovery(ctx, Actor{ID: "admin", Name: "admin", Remote: "10.0.0.2:4000", Admin: true}, op.ID, "service deleted outside berthy", false)
	if err != nil || !report.Resolved {
		t.Fatalf("resolve %+v: %v", report, err)
	}
	if err = c.Recover(ctx); err != nil {
		t.Fatalf("recover after resolve: %v", err)
	}
	if _, err = s.Active(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("lock still held: %v", err)
	}
	got, err := s.Operation(ctx, op.ID)
	if err != nil || got.State != "failed" || got.Err != "recovery resolved by admin" {
		t.Fatalf("operation %+v: %v", got, err)
	}
	if _, err = c.ResolveRecovery(ctx, Actor{ID: "admin", Admin: true}, op.ID, "again", false); !errors.Is(err, errNotRecovering) {
		t.Fatalf("second resolve: %v", err)
	}
}

// Recover and ResolveRecovery share recoverMu, so neither can run while the other holds it.
func TestRecoverAndResolveWaitForEachOther(t *testing.T) {
	s := openStore(t)
	op := stuckOperation(t, s)
	c := &Controller{Engine: &positiveEngine{spec: json.RawMessage(`{"Name":"foreign"}`)}, Store: s}
	calls := map[string]func() error{
		"recover": func() error { return c.Recover(context.Background()) },
		"resolve": func() error {
			_, err := c.ResolveRecovery(context.Background(), Actor{ID: "admin", Admin: true}, op.ID, "service removed", true)
			return err
		},
	}
	for name, call := range calls {
		c.recoverMu.Lock()
		done := make(chan error, 1)
		go func() { done <- call() }()
		select {
		case err := <-done:
			c.recoverMu.Unlock()
			t.Fatalf("%s ran while a recovery attempt held the lock: %v", name, err)
		case <-time.After(50 * time.Millisecond):
		}
		c.recoverMu.Unlock()
		<-done
	}
}
