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
	nativeReady                               time.Duration
	restoredAt                                time.Time
	healthyAt                                 time.Time
	begin, finished                           time.Time
	updates                                   int
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
			f.updates++
			if f.updates == 2 {
				f.restoredAt = time.Now()
			}
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
		ready := f.nativeReady
		if ready == 0 {
			ready = 16 * time.Second
		}
		if time.Since(f.start) < ready {
			live.UpdateStatus.State = "updating"
		} else if f.nativeAt.IsZero() {
			f.nativeAt = time.Now()
		}
	}
	return live, err
}

func seedBudgetUpdate(t *testing.T, s *state.Store, f *budgetEngine) {
	t.Helper()
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
	f.add("old", json.RawMessage(`{"Name":"proj-test-web-a","TaskTemplate":{"ContainerSpec":{"Image":"example/web@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","Labels":{"berthy.operation":"previous"}}},"Mode":{"Replicated":{"Replicas":1}}}`))
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
			if f.secondStopped.IsZero() {
				f.secondStopped = time.Now()
			}
			return nil, err
		}
	}
	return f.boundFake.ServiceTasks(ctx, id)
}

func (f *budgetEngine) InspectContainerHealth(ctx context.Context, id string) (string, error) {
	if id == "c-old" && f.updated && f.healthyAt.IsZero() {
		live, err := f.boundFake.InspectService(ctx, "old")
		var spec struct {
			TaskTemplate struct {
				ContainerSpec struct{ Labels map[string]string }
			}
		}
		if err == nil && json.Unmarshal(live.Spec, &spec) == nil {
			generation := spec.TaskTemplate.ContainerSpec.Labels["berthy.operation"]
			if generation != "" && generation != "previous" {
				f.healthyAt = time.Now()
			}
		}
	}
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
		f.begin = time.Now()
		go func() {
			defer close(done)
			c.execute(runCtx, op, Actor{ID: "a", Admin: true}, req)
			f.finished = time.Now()
		}()
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

func requireBudgetStarted(t *testing.T, f *budgetEngine, op state.Operation) {
	t.Helper()
	if f.start.IsZero() {
		if (op.State == "failed" || op.State == "rolled_back") && op.Reason == "deploy deadline exceeded before the task was healthy" {
			t.Fatalf("startup deadline expired at %v, before the first role mutation", f.finished.Sub(f.begin))
		}
		t.Fatalf("rollout fixture did not reach mutation: state=%s error=%q reason=%q", op.State, op.Err, op.Reason)
	}
}

func TestRolloutDoesNotBankUnusedGrace(t *testing.T) {
	t.Run("second role", func(t *testing.T) {
		s := openStore(t)
		f := &budgetEngine{boundFake: &boundFake{}, secondReady: 16 * time.Second}
		op, _ := budgetDeploy(t, f, s, Request{RequestID: "budget", App: "web", Version: digestOfLetter("a"), Manifest: budgetManifest()})
		requireBudgetStarted(t, f, op)
		if len(f.createdSpecs()) != 2 {
			t.Fatalf("created %d roles, want two before the second cutoff", len(f.createdSpecs()))
		}
		if got := f.secondStarted.Sub(f.start); got != 5*time.Second {
			t.Fatalf("second role started at %v, want 5s", got)
		}
		if got := f.secondStopped.Sub(f.start); got != 15*time.Second {
			t.Fatalf("second startup stopped at %v, want 15s", got)
		}
		if op.State != "failed" || op.Reason != "deploy deadline exceeded before the task was healthy" {
			t.Fatalf("operation %s %q", op.State, op.Reason)
		}
		if len(f.services) != 0 {
			t.Fatalf("services remain after compensation: %d", len(f.services))
		}
		if _, err := s.Lock(context.Background()); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("lock after compensation: %v", err)
		}
	})
	t.Run("native completion", func(t *testing.T) {
		s := openStore(t)
		f := &budgetEngine{boundFake: &boundFake{}, nativeSlow: true}
		seedBudgetUpdate(t, s, f)
		op, intents := budgetDeploy(t, f, s, Request{RequestID: "native", App: "web", Version: digestOfLetter("a"), Manifest: budgetManifest()})
		requireBudgetStarted(t, f, op)
		if !f.updated {
			t.Fatal("first role did not update")
		}
		if f.nativeAt.IsZero() && !f.restoredAt.IsZero() {
			t.Fatalf("native completion not reached before compensation at %v, want 16s", f.restoredAt.Sub(f.start))
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
		requireBudgetStarted(t, f, op)
		if got := f.secondStarted.Sub(f.start); got != 5*time.Second {
			t.Fatalf("second role started at %v, want 5s", got)
		}
		if op.State != "succeeded" {
			t.Fatalf("second startup read ended at %v; want healthy at 14.5s and final verification at 36s: %s %s", f.secondStopped.Sub(f.start), op.State, op.Reason)
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
		requireBudgetStarted(t, f, op)
		if len(f.createdSpecs()) != 2 {
			t.Fatalf("created %d roles before the unhealthy wait", len(f.createdSpecs()))
		}
		if got := f.finished.Sub(f.start); got != 15*time.Second {
			t.Fatalf("unhealthy rollout ended at %v, want 15s", got)
		}
		if op.State != "failed" || op.Reason != "deploy deadline exceeded before the task was healthy" {
			t.Fatalf("operation %s %q", op.State, op.Reason)
		}
	})
	t.Run("coverage lost after startup cutoff", func(t *testing.T) {
		s := openStore(t)
		f := &budgetEngine{boundFake: &boundFake{}, secondReady: 14500 * time.Millisecond, dropAfter: 15500 * time.Millisecond}
		op, _ := budgetDeploy(t, f, s, Request{RequestID: "lost", App: "web", Version: digestOfLetter("a"), Manifest: budgetManifest()})
		requireBudgetStarted(t, f, op)
		if op.State != "failed" || op.Reason != "deploy deadline exceeded before the task was healthy" {
			t.Fatalf("operation %s %q", op.State, op.Reason)
		}
		if f.lostAt.IsZero() && !f.secondStopped.IsZero() {
			t.Fatalf("startup read stopped at %v before coverage loss at 15.5s", f.secondStopped.Sub(f.start))
		}
		if got := f.lostAt.Sub(f.start); got != 15500*time.Millisecond {
			t.Fatalf("coverage lost at %v", got)
		}
		if len(f.services) != 0 {
			t.Fatalf("services remain after compensation: %d", len(f.services))
		}
	})
}

func TestNativeCompletionUsesTheExtendedRoleDeadline(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ready time.Duration
		want  string
	}{
		{"completed before role deadline", 12 * time.Second, "succeeded"},
		{"completed only after role deadline", 31 * time.Second, "rolled_back"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			f := &budgetEngine{boundFake: &boundFake{}, nativeSlow: true, nativeReady: tc.ready}
			seedBudgetUpdate(t, s, f)
			m := budgetManifest()
			app := m.Apps["web"]
			delete(app.Roles, "b")
			m.Apps["web"] = app
			op, intents := budgetDeploy(t, f, s, Request{RequestID: "native-deadline", App: "web", Version: digestOfLetter("a"), Manifest: m})
			if !f.updated {
				t.Fatal("candidate update never started")
			}
			if f.healthyAt.IsZero() {
				t.Fatal("candidate update was never observed healthy")
			}
			if op.State != tc.want {
				t.Fatalf("operation %s, want %s: %s", op.State, tc.want, op.Reason)
			}
			if tc.want == "rolled_back" {
				if got := f.restoredAt.Sub(f.start); got != 30*time.Second {
					t.Fatalf("compensation started at %v, want 30s", got)
				}
			}
			var barrier bool
			for _, i := range intents {
				if i.Role == "a" && i.Deadline.Sub(f.start) != 30*time.Second {
					t.Errorf("role deadline = %v", i.Deadline.Sub(f.start))
				}
				if i.Kind == "barrier" {
					barrier = true
				}
			}
			if tc.want == "succeeded" {
				if !barrier || f.nativeAt.Sub(f.start) != tc.ready {
					t.Fatalf("barrier=%v, native completion at %v", barrier, f.nativeAt.Sub(f.start))
				}
			} else {
				if barrier {
					t.Fatal("barrier recorded after native deadline")
				}
				applied, err := s.Applied(context.Background(), "web")
				if err != nil || applied.Digest != digestOfLetter("b") {
					t.Fatalf("previous release = %+v, %v", applied, err)
				}
			}
		})
	}
}

type observedCrashEngine struct {
	*diagFake
	seenHealthy bool
}

func (f *observedCrashEngine) InspectContainerHealth(ctx context.Context, id string) (string, error) {
	health, err := f.diagFake.InspectContainerHealth(ctx, id)
	if id == "c-bad" && err == nil && health == "healthy" {
		f.seenHealthy = true
	}
	return health, err
}

func TestDeployUnwindsAHealthyTaskThatThenFails(t *testing.T) {
	for _, kind := range []string{"create", "update"} {
		t.Run(kind, func(t *testing.T) {
			s := openStore(t)
			f := newDiagFake()
			f.diagnose = map[string]engine.ContainerDiagnosis{
				"c-bad":  {State: "exited", ExitCode: 1, Health: "healthy"},
				"c-good": {State: "running", Health: "healthy"},
			}
			f.tails = map[string]string{"c-bad": "candidate exited"}
			var firstCandidate time.Time
			f.tasksFor = func(generation, image string) []engine.ServiceTask {
				if !strings.Contains(image, strings.Repeat("b", 64)) {
					return []engine.ServiceTask{{ID: "good", ContainerID: "c-good", ServiceID: "id", State: "running", DesiredState: "running", Slot: 1, Labels: map[string]string{"berthy.operation": generation}}}
				}
				if firstCandidate.IsZero() {
					firstCandidate = time.Now()
				}
				task := engine.ServiceTask{ID: "candidate", ContainerID: "c-bad", ServiceID: "id", State: "running", DesiredState: "running", Slot: 1, Labels: map[string]string{"berthy.operation": generation}}
				if time.Since(firstCandidate) >= 3*time.Second {
					task.State, task.DesiredState, task.ExitCode = "failed", "shutdown", exitCode(1)
				}
				return []engine.ServiceTask{task}
			}
			observed := &observedCrashEngine{diagFake: f}
			c := &Controller{Store: s, Engine: observed, Environment: "test"}
			m := webManifest("10s", withStartAttempts(1))
			var previous state.Release
			var candidate state.Operation
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				defer func() {
					if err := c.Drain(ctx); err != nil {
						t.Error(err)
					}
				}()
				if kind == "update" {
					prior, _, err := c.Admit(ctx, Actor{ID: "a", Admin: true}, Request{RequestID: "prior", App: "web", Version: digestOfLetter("a"), Manifest: m})
					if err != nil {
						t.Fatal(err)
					}
					if err := c.Drain(ctx); err != nil {
						t.Fatal(err)
					}
					got, err := s.Operation(ctx, prior.ID)
					if err != nil || got.State != "succeeded" {
						t.Fatalf("prior deployment: %+v %v", got, err)
					}
					previous, err = s.Applied(ctx, "web")
					if err != nil {
						t.Fatal(err)
					}
				}
				op, _, err := c.Admit(ctx, Actor{ID: "a", Admin: true}, Request{RequestID: "candidate", App: "web", Version: digestOfLetter("b"), Manifest: m})
				if err != nil {
					t.Fatal(err)
				}
				if err := c.Drain(ctx); err != nil {
					t.Fatal(err)
				}
				candidate, err = s.Operation(ctx, op.ID)
				if err != nil {
					t.Fatal(err)
				}
			})
			if firstCandidate.IsZero() || !observed.seenHealthy {
				t.Fatal("candidate was never observed healthy")
			}
			want := "failed"
			if kind == "update" {
				want = "rolled_back"
			}
			if candidate.State != want {
				t.Fatalf("candidate state %s, want %s: %s", candidate.State, want, candidate.Reason)
			}
			if _, err := s.Lock(context.Background()); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("lock: %v", err)
			}
			for _, event := range rolloutStatuses(t, s, candidate.ID) {
				if event == [2]string{"web", "ready"} {
					t.Fatal("crashing candidate was marked ready")
				}
			}
			diagnostics := storedDiagnostics(t, s, candidate.ID)
			if len(diagnostics) == 0 || diagnostics[0].TaskID != "candidate" || diagnostics[0].ExitCode == nil || *diagnostics[0].ExitCode != 1 {
				t.Fatalf("candidate evidence: %+v", diagnostics)
			}
			live, err := f.InspectService(context.Background(), "proj-test-web-web")
			if kind == "create" {
				var status *engine.StatusError
				if !errors.As(err, &status) || status.Code != 404 {
					t.Fatalf("created service remains: %+v %v", live, err)
				}
			} else {
				applied, e := s.Applied(context.Background(), "web")
				if e != nil || applied.ID != previous.ID || applied.Digest != previous.Digest {
					t.Fatalf("previous release: %+v %v", applied, e)
				}
				if err != nil || !strings.Contains(string(live.Spec), strings.Repeat("a", 64)) {
					t.Fatalf("previous service: %+v %v", live, err)
				}
			}
		})
	}
}
