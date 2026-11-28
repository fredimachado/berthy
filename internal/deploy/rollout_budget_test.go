package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
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
	samples                                   map[string][]healthSample
	healthStopped                             time.Time
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

type healthSample struct {
	after      time.Duration
	slots      int
	oldReplica bool
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
	if samples := f.samples[id]; len(samples) > 0 && f.healthStopped.IsZero() {
		start := f.start
		if id == "new-2" {
			start = f.secondStarted
		}
		sample := samples[0]
		if err := waitFakeUntil(ctx, start.Add(sample.after)); err != nil {
			f.healthStopped = time.Now()
			return nil, err
		}
		if len(samples) > 1 {
			f.samples[id] = samples[1:]
		}
		tasks, err := f.boundFake.ServiceTasks(ctx, id)
		if err != nil {
			return nil, err
		}
		candidate := tasks[0]
		tasks = tasks[:0]
		for slot := 1; slot <= sample.slots; slot++ {
			task := candidate
			task.Slot = uint64(slot)
			task.ID = id + "-" + strconv.Itoa(slot)
			task.ContainerID = "c-" + task.ID
			tasks = append(tasks, task)
		}
		if sample.oldReplica {
			candidate.ID = id + "-duplicate"
			candidate.ContainerID = "c-" + candidate.ID
			tasks = append(tasks, candidate) // A second task for slot 1 adds no coverage.
			candidate.ID = id + "-previous"
			candidate.ContainerID = "c-" + candidate.ID
			candidate.Slot = uint64(sample.slots + 1)
			candidate.Labels = map[string]string{"berthy.operation": "previous"}
			tasks = append(tasks, candidate)
		}
		return tasks, nil
	}
	if strings.HasSuffix(specName(live.Spec), "-b") {
		if f.never {
			return nil, nil
		}
		if f.dropAfter > 0 && time.Since(f.start) >= f.secondReady+rolloutMonitor {
			if err := waitFakeUntil(ctx, f.start.Add(f.dropAfter)); err != nil {
				return nil, err
			}
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
		runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		done := make(chan struct{})
		f.begin = time.Now()
		go func() {
			defer close(done)
			c.execute(runCtx, op, Actor{ID: "a", Admin: true}, req)
			f.finished = time.Now()
		}()
		defer func() { cancel(); <-done }()
		synctest.Wait()
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

func progressManifest(replicas int, twoRoles bool) manifest.Manifest {
	m := budgetManifest()
	app := m.Apps["web"]
	app.Deploy.DeployTimeout = "60s"
	role := app.Roles["a"]
	role.Replicas = &replicas
	app.Roles["a"] = role
	if !twoRoles {
		delete(app.Roles, "b")
	}
	m.Apps["web"] = app
	return m
}

func fourReplicaProgress(last healthSample) []healthSample {
	return []healthSample{
		{after: 40 * time.Second, slots: 1},
		{after: 80 * time.Second, slots: 2},
		{after: 120 * time.Second, slots: 3},
		last,
	}
}

func requireProgressOutcome(t *testing.T, s *state.Store, f *budgetEngine, op state.Operation, want string) {
	t.Helper()
	if op.State != want {
		t.Fatalf("operation %s, want %s: %s", op.State, want, op.Reason)
	}
	ctx := context.Background()
	applied, err := s.Applied(ctx, "web")
	if want == "succeeded" {
		if err != nil || applied.Digest != "docker.io/example/web@"+digestOfLetter("a") || applied.OperationID != op.ID {
			t.Fatalf("applied digest=%q, operation=%q, err=%v", applied.Digest, applied.OperationID, err)
		}
	} else {
		if op.Reason != "deploy deadline exceeded before the task was healthy" || len(f.services) != 0 || !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("failed rollout reason=%q, services=%d, applied=%+v, err=%v", op.Reason, len(f.services), applied, err)
		}
	}
	if _, err := s.Lock(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("lock after rollout: %v", err)
	}
}

func TestDeployRenewsDeadlineForHealthyReplicas(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		last        healthSample
		end         time.Duration
	}{
		{"four healthy", "succeeded", healthSample{after: 160 * time.Second, slots: 4}, 165 * time.Second},
		{"fourth stalls", "failed", healthSample{after: time.Hour}, 180 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			f := &budgetEngine{boundFake: &boundFake{}, samples: map[string][]healthSample{"new-1": fourReplicaProgress(tc.last)}}
			op, _ := budgetDeploy(t, f, s, Request{RequestID: "replica-progress", App: "web", Version: digestOfLetter("a"), Manifest: progressManifest(4, false)})
			ended := f.finished
			if !f.healthStopped.IsZero() {
				ended = f.healthStopped
			}
			if got := ended.Sub(f.start); got != tc.end {
				t.Errorf("rollout wait ended at %v, want %v", got, tc.end)
			}
			requireProgressOutcome(t, s, f, op, tc.state)
		})
	}
}

func TestDeployRenewsDeadlineOnlyForMoreCoverage(t *testing.T) {
	s := openStore(t)
	f := &budgetEngine{boundFake: &boundFake{}, samples: map[string][]healthSample{"new-1": {
		{after: 40 * time.Second, slots: 1},
		{after: 45 * time.Second, slots: 1},
		{after: 50 * time.Second, slots: 1, oldReplica: true},
		{after: 55 * time.Second},
		{after: 57 * time.Second, slots: 1},
		{after: time.Hour},
	}}}
	op, _ := budgetDeploy(t, f, s, Request{RequestID: "coverage-progress", App: "web", Version: digestOfLetter("a"), Manifest: progressManifest(4, false)})
	if got := f.healthStopped.Sub(f.start); got != 100*time.Second {
		t.Errorf("health wait expired at %v, want 100s", got)
	}
	requireProgressOutcome(t, s, f, op, "failed")
}

func TestVerifiedRoleStartsTheNextProgressWindow(t *testing.T) {
	s := openStore(t)
	firstHealthy := []healthSample{{after: 40 * time.Second, slots: 1}}
	f := &budgetEngine{boundFake: &boundFake{}, samples: map[string][]healthSample{"new-1": firstHealthy, "new-2": firstHealthy}}
	op, intents := budgetDeploy(t, f, s, Request{RequestID: "role-progress", App: "web", Version: digestOfLetter("a"), Manifest: progressManifest(1, true)})
	if got := f.secondStarted.Sub(f.start); got != 45*time.Second {
		t.Errorf("second role started at %v, want 45s", got)
	}
	if !f.healthStopped.IsZero() {
		t.Errorf("second role health wait expired at %v", f.healthStopped.Sub(f.start))
	}
	requireProgressOutcome(t, s, f, op, "succeeded")
	verified := map[string]bool{}
	for _, intent := range intents {
		if intent.Phase == "verified" {
			verified[intent.Role] = true
		}
	}
	if !verified["a"] || !verified["b"] {
		t.Fatalf("verified roles = %v, want a and b", verified)
	}
}

type verifiedDeadlineJournal struct {
	journalFake
	started, deadline time.Time
	phase             string
	events            []state.Event
}

func (j *verifiedDeadlineJournal) MarkStep(ctx context.Context, op string, ordinal int, phase string, receipt json.RawMessage, events ...state.Event) error {
	if phase == "verified" {
		j.started, j.phase, j.events = time.Now(), phase, events
		j.deadline, _ = ctx.Deadline()
		return ctx.Err()
	}
	return j.journalFake.MarkStep(ctx, op, ordinal, phase, receipt, events...)
}

func TestCreateRoleWritesVerifiedWithRenewedDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f := &budgetEngine{boundFake: &boundFake{}, samples: map[string][]healthSample{
			"new-1": fourReplicaProgress(healthSample{after: 160 * time.Second, slots: 4}),
		}}
		j := &verifiedDeadlineJournal{}
		spec := json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Labels":{"berthy.operation":"op"}}}}`)
		timing := rolloutTiming{window: time.Minute, cutoff: start.Add(time.Minute), hold: rolloutMonitor}
		_, err := createRole(ctx, j, f, "op", state.Intent{Kind: "create", Role: "web", Name: "svc", Generation: "op", Desired: spec}, nil, 4, startBudget{}, timing)
		synctest.Wait()
		if err != nil {
			t.Fatalf("create failed before verified write at %v: %v", time.Since(start), err)
		}
		if got := j.started.Sub(start); got != 165*time.Second {
			t.Errorf("verified write started at %v, want 165s", got)
		}
		if want := start.Add(240 * time.Second); !j.deadline.Equal(want) {
			t.Errorf("verified write deadline = %v, want %v", j.deadline, want)
		}
		ready := rolloutMilestone("web", "ready")
		if j.phase != "verified" || len(j.events) != 1 || j.events[0].Kind != ready.Kind || string(j.events[0].Data) != string(ready.Data) {
			t.Fatalf("journal phase %q with events %+v, want verified with ready milestone", j.phase, j.events)
		}
	})
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
		f := &budgetEngine{boundFake: &boundFake{}, nativeSlow: true, secondReady: 27 * time.Second}
		seedBudgetUpdate(t, s, f)
		op, _ := budgetDeploy(t, f, s, Request{RequestID: "native", App: "web", Version: digestOfLetter("a"), Manifest: budgetManifest()})
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
		if got := f.secondStarted.Sub(f.start); got != 16*time.Second {
			t.Fatalf("second role started at %v, want 16s", got)
		}
		if got := f.secondStopped.Sub(f.secondStarted); got != 10*time.Second {
			t.Fatalf("second role startup lasted %v, want 10s", got)
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
		for role, want := range map[string]time.Duration{"a": 10 * time.Second, "b": 15 * time.Second, "barrier": 47 * time.Second} {
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
		f := &budgetEngine{boundFake: &boundFake{}, secondReady: 14500 * time.Millisecond, dropAfter: 25500 * time.Millisecond}
		op, _ := budgetDeploy(t, f, s, Request{RequestID: "lost", App: "web", Version: digestOfLetter("a"), Manifest: budgetManifest()})
		requireBudgetStarted(t, f, op)
		if op.State != "failed" || op.Reason != "deploy deadline exceeded before the task was healthy" {
			t.Fatalf("operation %s %q", op.State, op.Reason)
		}
		if f.lostAt.IsZero() && !f.secondStopped.IsZero() {
			t.Fatalf("startup read stopped at %v before coverage loss at 25.5s", f.secondStopped.Sub(f.start))
		}
		if got := f.lostAt.Sub(f.start); got != f.dropAfter {
			t.Fatalf("coverage lost at %v", got)
		}
		if !f.finished.Equal(f.lostAt) {
			t.Fatalf("rollout continued until %v after losing coverage at %v", f.finished.Sub(f.start), f.lostAt.Sub(f.start))
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
				if i.Role == "a" && i.Deadline.Sub(f.start) != 10*time.Second {
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
