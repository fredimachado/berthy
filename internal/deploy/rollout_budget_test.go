package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

type budgetEngine struct {
	*boundFake
	start                                     time.Time
	secondReady                               time.Duration
	dropAfter                                 time.Duration
	confirmSlow, finalSlow, never, nativeSlow bool
	confirmed, updated                        bool
	secondStopped                             time.Time
	secondStarted                             time.Time
	confirmedAt, finalAt, lostAt, nativeAt    time.Time
}

func (f *budgetEngine) CreateService(ctx context.Context, spec json.RawMessage, auth *engine.RegistryAuth) (string, error) {
	if f.start.IsZero() {
		f.start = time.Now()
	}
	if strings.HasSuffix(specName(spec), "-b") {
		f.secondStarted = time.Now()
	}
	return f.boundFake.CreateService(ctx, spec, auth)
}

func (f *budgetEngine) UpdateService(_ context.Context, id string, _ uint64, spec json.RawMessage, _ *engine.RegistryAuth) error {
	if f.start.IsZero() {
		f.start = time.Now()
	}
	f.boundFake.mu.Lock()
	defer f.boundFake.mu.Unlock()
	for i := range f.services {
		if f.services[i].ID == id {
			f.services[i].Spec = append(json.RawMessage(nil), spec...)
			f.updated = true
			return nil
		}
	}
	return errors.New("missing service")
}

func waitFakeUntil(ctx context.Context, target time.Time) error {
	if d := time.Until(target); d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (f *budgetEngine) InspectService(ctx context.Context, ref string) (engine.ServiceSnapshot, error) {
	if f.finalSlow && f.confirmed {
		if err := waitFakeUntil(ctx, f.start.Add(36*time.Second)); err != nil {
			return engine.ServiceSnapshot{}, err
		}
		if f.finalAt.IsZero() {
			f.finalAt = time.Now()
		}
	}
	live, err := f.boundFake.InspectService(ctx, ref)
	if err == nil && f.nativeSlow && f.updated && strings.HasSuffix(specName(live.Spec), "-a") {
		if time.Since(f.start) < 16*time.Second {
			live.UpdateStatus.State = "updating"
		} else if f.nativeAt.IsZero() {
			f.nativeAt = time.Now()
		}
	}
	return live, err
}

func (f *budgetEngine) ServiceTasks(ctx context.Context, id string) ([]engine.ServiceTask, error) {
	live, err := f.boundFake.InspectService(ctx, id)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(specName(live.Spec), "-b") {
		if f.never {
			return nil, nil
		}
		if f.dropAfter > 0 && time.Since(f.start) >= f.dropAfter {
			if f.lostAt.IsZero() {
				f.lostAt = time.Now()
			}
			return nil, nil
		}
		if err := waitFakeUntil(ctx, f.start.Add(f.secondReady)); err != nil {
			f.secondStopped = time.Now()
			return nil, err
		}
	}
	return f.boundFake.ServiceTasks(ctx, id)
}

func (f *budgetEngine) InspectContainerHealth(ctx context.Context, id string) (string, error) {
	if f.confirmSlow && id == "c-new-2" && time.Since(f.start) >= 19*time.Second && !f.confirmed {
		if err := waitFakeUntil(ctx, f.start.Add(22*time.Second)); err != nil {
			return "", err
		}
		f.confirmed = true
		f.confirmedAt = time.Now()
	}
	return "healthy", nil
}

func (f *budgetEngine) DiagnoseContainer(context.Context, string) (engine.ContainerDiagnosis, error) {
	return engine.ContainerDiagnosis{State: "running", Health: "healthy"}, nil
}

func (f *budgetEngine) LogTail(context.Context, string, int, int) (string, bool, error) {
	return "", false, nil
}

func budgetManifest() manifest.Manifest {
	m := dockerManifest(nil)
	a := m.Apps["web"]
	a.Deploy.DeployTimeout = "10s"
	r := a.Roles["web"]
	a.Roles = map[string]manifest.Role{"a": r, "b": r}
	m.Apps["web"] = a
	return m
}

func budgetDeploy(t *testing.T, f *budgetEngine, s *state.Store, req Request) (state.Operation, []state.Intent) {
	t.Helper()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: req.RequestID, ActorID: "a", Kind: "deploy", App: "web", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	c := &Controller{Store: s, Engine: f, Environment: "test"}
	synctest.Test(t, func(t *testing.T) {
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); c.execute(runCtx, op, Actor{ID: "a", Admin: true}, req) }()
		defer func() { cancel(); <-done }()
		<-done
	})
	op, err = s.Operation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	intents, err := s.Intents(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return op, intents
}

func TestRolloutDoesNotBankUnusedGrace(t *testing.T) {
	t.Run("second role", func(t *testing.T) {
		s := openStore(t)
		f := &budgetEngine{boundFake: &boundFake{}, secondReady: 16 * time.Second}
		op, _ := budgetDeploy(t, f, s, Request{RequestID: "budget", App: "web", Version: digestOfLetter("a"), Manifest: budgetManifest()})
		if len(f.createdSpecs()) != 2 {
			t.Fatalf("created %d roles, want two before the second cutoff", len(f.createdSpecs()))
		}
		if got := f.secondStarted.Sub(f.start); got != 5*time.Second {
			t.Fatalf("second role started at %v, want 5s", got)
		}
		if op.State != "failed" || op.Reason != "deploy deadline exceeded before the task was healthy" {
			t.Fatalf("operation %s %q", op.State, op.Reason)
		}
		if len(f.services) != 0 {
			t.Fatalf("services remain after compensation: %d", len(f.services))
		}
		if got := f.secondStopped.Sub(f.start); got != 15*time.Second {
			t.Fatalf("second startup stopped at %v, want 15s", got)
		}
		if _, err := s.Lock(context.Background()); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("lock after compensation: %v", err)
		}
	})
	t.Run("native completion", func(t *testing.T) {
		s := openStore(t)
		ctx := context.Background()
		prior, _, err := s.Admit(ctx, state.Admission{RequestID: "prior", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(budgetManifest())
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Finish(ctx, prior.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "web", Digest: digestOfLetter("b"), Manifest: raw, Bindings: []state.Binding{{App: "web", Role: "a", Name: "proj-test-web-a", ServiceID: "old"}}}}); err != nil {
			t.Fatal(err)
		}
		f := &budgetEngine{boundFake: &boundFake{}, nativeSlow: true}
		f.add("old", json.RawMessage(`{"Name":"proj-test-web-a","TaskTemplate":{"ContainerSpec":{"Image":"example/web@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","Labels":{"berthy.operation":"previous"}}},"Mode":{"Replicated":{"Replicas":1}}}`))
		op, intents := budgetDeploy(t, f, s, Request{RequestID: "native", App: "web", Version: digestOfLetter("a"), Manifest: budgetManifest()})
		if !f.updated {
			t.Fatal("first role did not update")
		}
		if got := f.nativeAt.Sub(f.start); got != 16*time.Second {
			t.Fatalf("native completion at %v, want 16s", got)
		}
		if op.State != "rolled_back" {
			t.Fatalf("operation state %s", op.State)
		}
		if len(f.createdSpecs()) != 0 {
			t.Fatalf("created %d later roles", len(f.createdSpecs()))
		}
		for _, i := range intents {
			if i.Role == "b" {
				t.Fatalf("recorded later role intent: %+v", i)
			}
		}
	})
}

func TestDeployReservesMonitorTimeForEachRole(t *testing.T) {
	t.Run("confirmation and final verification", func(t *testing.T) {
		s := openStore(t)
		f := &budgetEngine{boundFake: &boundFake{}, secondReady: 14500 * time.Millisecond, confirmSlow: true, finalSlow: true}
		op, intents := budgetDeploy(t, f, s, Request{RequestID: "reserve", App: "web", Version: digestOfLetter("a"), Manifest: budgetManifest()})
		if op.State != "succeeded" {
			t.Fatalf("operation %s: %s %s", op.State, op.Err, op.Reason)
		}
		if got := f.secondStarted.Sub(f.start); got != 5*time.Second {
			t.Fatalf("second role started at %v, want 5s", got)
		}
		deadlines := map[string]time.Time{}
		for _, i := range intents {
			if i.Kind == "barrier" {
				deadlines["barrier"] = i.Deadline
			}
			if i.Role != "" {
				deadlines[i.Role] = i.Deadline
			}
		}
		for role, want := range map[string]time.Duration{"a": 30 * time.Second, "b": 35 * time.Second, "barrier": 37 * time.Second} {
			if got := deadlines[role].Sub(f.start); got != want {
				t.Errorf("%s deadline %v, want %v", role, got, want)
			}
		}
		if !f.confirmed {
			t.Error("second confirmation did not cross 20 seconds")
		}
		if got := f.confirmedAt.Sub(f.start); got != 22*time.Second {
			t.Errorf("second confirmed at %v, want 22s", got)
		}
		if got := f.finalAt.Sub(f.start); got != 36*time.Second {
			t.Errorf("final check at %v, want 36s", got)
		}
		if _, err := s.Lock(context.Background()); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("lock after success: %v", err)
		}
	})
	t.Run("never healthy", func(t *testing.T) {
		s := openStore(t)
		f := &budgetEngine{boundFake: &boundFake{}, never: true}
		op, _ := budgetDeploy(t, f, s, Request{RequestID: "never", App: "web", Version: digestOfLetter("a"), Manifest: budgetManifest()})
		if len(f.createdSpecs()) != 2 {
			t.Fatalf("created %d roles before the unhealthy wait", len(f.createdSpecs()))
		}
		if op.State != "failed" || op.Reason != "deploy deadline exceeded before the task was healthy" {
			t.Fatalf("operation %s %q", op.State, op.Reason)
		}
	})
	t.Run("coverage lost after startup cutoff", func(t *testing.T) {
		s := openStore(t)
		f := &budgetEngine{boundFake: &boundFake{}, secondReady: 14500 * time.Millisecond, dropAfter: 15500 * time.Millisecond}
		op, _ := budgetDeploy(t, f, s, Request{RequestID: "lost", App: "web", Version: digestOfLetter("a"), Manifest: budgetManifest()})
		if op.State != "failed" || op.Reason != "deploy deadline exceeded before the task was healthy" {
			t.Fatalf("operation %s %q", op.State, op.Reason)
		}
		if got := f.lostAt.Sub(f.start); got != 15500*time.Millisecond {
			t.Fatalf("coverage lost at %v", got)
		}
		if len(f.services) != 0 {
			t.Fatalf("services remain after compensation: %d", len(f.services))
		}
	})
}
