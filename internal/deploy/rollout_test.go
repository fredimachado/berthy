package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

type journalFake struct {
	fail  bool
	saved []state.Intent
}

func (j *journalFake) RecordIntent(_ context.Context, _ string, i state.Intent) error {
	if j.fail {
		return errors.New("disk failed")
	}
	j.saved = append(j.saved, i)
	return nil
}
func (j *journalFake) MarkStep(context.Context, string, int, string, json.RawMessage, ...state.Event) error {
	return nil
}

type mutationFake struct {
	creates       int
	updateVersion uint64
	live          engine.ServiceSnapshot
	inspectErr    error
	tasks         []engine.ServiceTask
}

func (m *mutationFake) InspectService(context.Context, string) (engine.ServiceSnapshot, error) {
	return m.live, m.inspectErr
}
func (m *mutationFake) CreateService(context.Context, json.RawMessage, *engine.RegistryAuth) (string, error) {
	m.creates++
	return "id", nil
}
func (m *mutationFake) UpdateService(_ context.Context, _ string, version uint64, spec json.RawMessage, _ *engine.RegistryAuth) error {
	m.updateVersion = version
	m.live.Spec = spec
	m.live.UpdateStatus.State = "completed"
	return nil
}
func (m *mutationFake) DeleteService(context.Context, string) error { return nil }
func TestJournalFailurePreventsCreate(t *testing.T) {
	j := &journalFake{fail: true}
	m := &mutationFake{}
	err := applyIntent(context.Background(), j, m, "op", state.Intent{Kind: "create", Name: "svc", Desired: json.RawMessage(`{"Name":"svc"}`)}, nil)
	if err == nil || m.creates != 0 {
		t.Fatalf("journal error %v, creates %d", err, m.creates)
	}
}

func TestUpdateRejectsForeignDriftAfterJournal(t *testing.T) {
	j := &journalFake{}
	m := &mutationFake{live: engine.ServiceSnapshot{ID: "id", Version: 4, Spec: json.RawMessage(`{"Name":"changed"}`)}}
	err := applyIntent(context.Background(), j, m, "op", state.Intent{Kind: "update", Name: "svc", ServiceID: "id", Before: json.RawMessage(`{"Name":"svc"}`), Desired: json.RawMessage(`{"Name":"new"}`)}, nil)
	if err == nil || len(j.saved) != 1 {
		t.Fatalf("drift error %v, intents %d", err, len(j.saved))
	}
}

func TestCompensationRefusesForeignCreate(t *testing.T) {
	m := &mutationFake{live: engine.ServiceSnapshot{ID: "other", Spec: json.RawMessage(`{"Name":"other"}`)}}
	err := compensate(context.Background(), m, state.Intent{Kind: "create", Name: "svc", Desired: json.RawMessage(`{"Name":"owned"}`)}, nil)
	if err == nil {
		t.Fatal("foreign service accepted")
	}
}

func TestNewServiceUsesStartFirstAndPinnedImage(t *testing.T) {
	one := 1
	r := manifest.Role{Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}
	b, err := serviceSpec("env", "proj", "app", "web", (&manifest.Manifest{Project: "proj"}).ServiceName("env", "app", "web"), "operation", "network", "registry/image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", func(v string) string { return (&manifest.Manifest{Project: "proj"}).VolumeName("env", v) }, nil, r, "start-first", "10s", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err = json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	update := v["UpdateConfig"].(map[string]any)
	if update["Order"] != "start-first" {
		t.Fatalf("update config: %s", b)
	}
}

type healthFake struct {
	tasks  []engine.ServiceTask
	status string
}

func (h healthFake) ServiceTasks(context.Context, string) ([]engine.ServiceTask, error) {
	return h.tasks, nil
}
func (h healthFake) InspectContainerHealth(context.Context, string) (string, error) {
	return h.status, nil
}
func TestHealthRejectsStartingContainer(t *testing.T) {
	e := healthFake{tasks: []engine.ServiceTask{{ID: "t", ServiceID: "s", ContainerID: "c", State: "running", DesiredState: "running", Slot: 1, Labels: map[string]string{"berthy.operation": "op"}}}, status: "starting"}
	if healthyOnce(context.Background(), e, "s", "op", 1) == nil {
		t.Fatal("starting task counted healthy")
	}
}

func TestHealthRejectsOldGeneration(t *testing.T) {
	e := healthFake{tasks: []engine.ServiceTask{{ID: "t", ServiceID: "s", ContainerID: "c", State: "running", DesiredState: "running", Slot: 1, Labels: map[string]string{"berthy.operation": "old"}}}, status: "healthy"}
	if healthyOnce(context.Background(), e, "s", "new", 1) == nil {
		t.Fatal("old task counted healthy")
	}
}

func TestHealthWaitHonorsDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := waitHealthy(ctx, healthFake{}, "s", "op", 1, startBudget{}, 0); err == nil {
		t.Fatal("unhealthy service passed")
	}
}

func TestCreatedSpecRejectsHostPrivileges(t *testing.T) {
	desired := json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Image":"x","Labels":{"berthy.operation":"op"}}}}`)
	live := json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Image":"x","Labels":{"berthy.operation":"op"},"Privileges":{"CredentialSpec":{"File":"evil"}}}}}`)
	if matchesStoredSpec(desired, live) {
		t.Fatal("host privilege accepted")
	}
}

func TestCreatedSpecAcceptsEngineDefaults(t *testing.T) {
	d := json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Image":"x"}}}`)
	l := json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Image":"x","Isolation":"default"},"Runtime":"container","ForceUpdate":0}}`)
	if !matchesStoredSpec(d, l) {
		t.Fatal("engine defaults differ")
	}
}

func TestServiceSpecUsesNamedVolumesAndResourceLimits(t *testing.T) {
	one := 1
	r := manifest.Role{Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}, Volumes: []string{"data:/data:ro"}, Resources: &manifest.Resources{CPU: "0.5", Memory: "64MiB"}, Port: 8080}
	b, err := serviceSpec("env", "proj", "app", "web", (&manifest.Manifest{Project: "proj"}).ServiceName("env", "app", "web"), "op", "network", "image", func(v string) string { return (&manifest.Manifest{Project: "proj"}).VolumeName("env", v) }, nil, r, "start-first", "10s", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err = json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	task := spec["TaskTemplate"].(map[string]any)
	container := task["ContainerSpec"].(map[string]any)
	mount := container["Mounts"].([]any)[0].(map[string]any)
	if mount["Type"] != "volume" || mount["ReadOnly"] != true {
		t.Fatalf("mount: %v", mount)
	}
	limits := task["Resources"].(map[string]any)["Limits"].(map[string]any)
	if limits["NanoCPUs"] != float64(500000000) || limits["MemoryBytes"] != float64(67108864) {
		t.Fatalf("limits: %v", limits)
	}
}

func TestServiceSpecInheritsImageHealth(t *testing.T) {
	b, err := serviceSpec("env", "proj", "app", "web", (&manifest.Manifest{Project: "proj"}).ServiceName("env", "app", "web"), "op", "network", "image", func(v string) string { return (&manifest.Manifest{Project: "proj"}).VolumeName("env", v) }, nil, manifest.Role{}, "start-first", "10s", json.RawMessage(`{"Test":["CMD","true"],"Interval":1000000000}`), "", "")
	if err != nil || !strings.Contains(string(b), `"Healthcheck"`) {
		t.Fatalf("image health: %s %v", b, err)
	}
}

func TestCreateRoleJournalsAndVerifiesHealthyGeneration(t *testing.T) {
	desired := json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Image":"x","Labels":{"berthy.operation":"op"}}}}`)
	m := &mutationFake{live: engine.ServiceSnapshot{ID: "id", Spec: desired}, tasks: []engine.ServiceTask{{ServiceID: "id", ContainerID: "container", State: "running", DesiredState: "running", Slot: 1, Labels: map[string]string{"berthy.operation": "op"}}}}
	j := &journalFake{}
	id, err := createRole(context.Background(), j, m, "op", state.Intent{Kind: "create", Name: "svc", Generation: "op", Desired: desired}, nil, 1, startBudget{limit: 3}, 0)
	if err != nil || id != "id" || len(j.saved) != 1 || m.creates != 1 {
		t.Fatalf("id=%q err=%v intents=%d creates=%d", id, err, len(j.saved), m.creates)
	}
}

func (m *mutationFake) ServiceTasks(context.Context, string) ([]engine.ServiceTask, error) {
	return m.tasks, nil
}
func (m *mutationFake) InspectContainerHealth(context.Context, string) (string, error) {
	return "healthy", nil
}

func TestSpecComparisonDetectsLargeIntegerDrift(t *testing.T) {
	if sameSpec(json.RawMessage(`{"value":9007199254740992}`), json.RawMessage(`{"value":9007199254740993}`)) {
		t.Fatal("integer drift hidden")
	}
}

func TestAdoptionRefusesAHostMount(t *testing.T) {
	live := json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Image":"old","Mounts":[{"Type":"bind","Source":"/","Target":"/host"}]},"Networks":[{"Target":"net"}]},"Mode":{"Replicated":{"Replicas":1}}}`)
	if err := adoptableService(live, "net"); err == nil {
		t.Fatal("host bind accepted")
	}
}

func TestUpdateRoleUsesCurrentVersion(t *testing.T) {
	before := json.RawMessage(`{"Name":"svc","version":"old"}`)
	desired := json.RawMessage(`{"Name":"svc","version":"new"}`)
	m := &mutationFake{live: engine.ServiceSnapshot{ID: "id", Version: 9, Spec: before}, tasks: []engine.ServiceTask{{ServiceID: "id", ContainerID: "c", State: "running", DesiredState: "running", Slot: 1, Labels: map[string]string{"berthy.operation": "op"}}}}
	j := &journalFake{}
	err := updateRole(context.Background(), j, m, "op", state.Intent{Kind: "update", Name: "svc", ServiceID: "id", Generation: "op", Before: before, Desired: desired}, nil, 1, startBudget{limit: 3}, 0)
	if err != nil || m.updateVersion != 9 {
		t.Fatalf("update version %d: %v", m.updateVersion, err)
	}
}

func TestDrainWaitsForRetiringTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	e := healthFake{tasks: []engine.ServiceTask{{ServiceID: "s", State: "running", Labels: map[string]string{"berthy.operation": "old"}}}}
	if waitDrain(ctx, e, "s", "new") == nil {
		t.Fatal("old task still running")
	}
}

func TestRejectsInheritedCapabilities(t *testing.T) {
	raw := json.RawMessage(`{"TaskTemplate":{"Runtime":"container","ContainerSpec":{"CapabilityAdd":["SYS_ADMIN"]}}}`)
	if validateInheritedRuntime(raw, nil) == nil {
		t.Fatal("host capabilities accepted")
	}
}

func TestUpdateWaitsForNativeCompletion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	e := &mutationFake{live: engine.ServiceSnapshot{ID: "id", UpdateStatus: engine.UpdateStatus{State: "updating"}}}
	if waitUpdate(ctx, e, "svc", "id") == nil {
		t.Fatal("unfinished update accepted")
	}
}

func TestUpdateHasTasks(t *testing.T) {
	tests := []struct {
		name, before string
		want         bool
	}{
		{"stopped at zero replicas", `{"Mode":{"Replicated":{"Replicas":0}}}`, false},
		{"running replicas", `{"Mode":{"Replicated":{"Replicas":1}}}`, true},
		{"negative replicas", `{"Mode":{"Replicated":{"Replicas":-1}}}`, true},
		{"no mode", `{"Name":"x"}`, true},
		{"global mode", `{"Mode":{"Global":{}}}`, true},
		{"unreadable spec", `not json`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := updateHasTasks(json.RawMessage(tt.before)); got != tt.want {
				t.Fatalf("updateHasTasks(%s) = %v, want %v", tt.before, got, tt.want)
			}
		})
	}
}

func TestAdoptedHealthRequiresPinnedTaskImage(t *testing.T) {
	e := healthFake{tasks: []engine.ServiceTask{{ServiceID: "s", ContainerID: "c", State: "running", DesiredState: "running", Slot: 1, Image: "repo@sha256:old"}}, status: "healthy"}
	if healthyImageOnce(context.Background(), e, "s", "repo@sha256:new", 1) == nil {
		t.Fatal("wrong predecessor image accepted")
	}
}

func TestRuntimeValidationRejectsUnknownSecuritySettings(t *testing.T) {
	for _, raw := range []string{`{"TaskTemplate":{"ContainerSpec":{"Mounts":[{"Type":"bind","Source":"/","Target":"/host"}]}}}`, `{"TaskTemplate":{"ContainerSpec":{"UnexpectedHostAccess":true}}}`} {
		if validateInheritedRuntime(json.RawMessage(raw), nil) == nil {
			t.Fatalf("unsafe runtime accepted: %s", raw)
		}
	}
}

// seqHealth is a healthReader whose task list changes with every poll, like a Swarm that keeps replacing a failing task and
// forgets the oldest ones.
type seqHealth struct {
	mu    sync.Mutex
	polls int
	step  func(poll int) []engine.ServiceTask
}

func (s *seqHealth) ServiceTasks(context.Context, string) ([]engine.ServiceTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.polls++
	return s.step(s.polls), nil
}
func (s *seqHealth) InspectContainerHealth(context.Context, string) (string, error) {
	return "healthy", nil
}

func failedTask(id, state, generation string) engine.ServiceTask {
	return engine.ServiceTask{ID: id, ServiceID: "s", State: state, DesiredState: "shutdown", Slot: 1, Labels: map[string]string{"berthy.operation": generation}}
}

// onService returns the tasks as tasks of the service with the given ID.
func onService(id string, tasks ...engine.ServiceTask) []engine.ServiceTask {
	for i := range tasks {
		tasks[i].ServiceID = id
	}
	return tasks
}

func runningTask(id, generation string) engine.ServiceTask {
	return engine.ServiceTask{ID: id, ServiceID: "s", ContainerID: "c-" + id, State: "running", DesiredState: "running", Slot: 1, Labels: map[string]string{"berthy.operation": generation}}
}

func TestRolloutRequiresAFullHealthyMonitor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		one, two := runningTask("one", "op"), runningTask("two", "op")
		two.Slot = 2
		e := &monitorHealth{eventedHealth: newEventedHealth(), tasks: []engine.ServiceTask{one, two}}
		ctx, cancel := context.WithCancel(context.Background())
		done, exited := make(chan error, 1), make(chan struct{})
		go func() {
			defer close(exited)
			done <- waitHealthy(ctx, e, "s", "op", 2, startBudget{limit: 3}, 5*time.Second)
		}()
		defer func() { cancel(); <-exited }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("returned before the monitor interval: %v", err)
		default:
		}
		time.Sleep(4200 * time.Millisecond)
		e.send(healthStatus("s"))
		synctest.Wait()
		time.Sleep(800*time.Millisecond - time.Millisecond)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("returned before the monitor interval ended: %v", err)
		default:
		}
		polls := e.polls.Load()
		time.Sleep(time.Millisecond)
		synctest.Wait()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("did not confirm after the monitor interval")
		}
		if e.polls.Load() <= polls {
			t.Fatal("did not make a fresh confirmation observation")
		}
	})
}

type monitorHealth struct {
	*eventedHealth
	tasks              []engine.ServiceTask
	taskErr, healthErr error
}

func (e *monitorHealth) ServiceTasks(context.Context, string) ([]engine.ServiceTask, error) {
	e.polls.Add(1)
	return e.tasks, e.taskErr
}

func (e *monitorHealth) InspectContainerHealth(context.Context, string) (string, error) {
	return "healthy", e.healthErr
}

func TestRolloutRestartsTheHealthyMonitor(t *testing.T) {
	for _, cause := range []string{"missing slot", "task list", "container inspect"} {
		t.Run(cause, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				one, two := runningTask("one", "op"), runningTask("two", "op")
				two.Slot = 2
				e := &monitorHealth{eventedHealth: newEventedHealth(), tasks: []engine.ServiceTask{one, two}}
				ctx, cancel := context.WithCancel(context.Background())
				done, exited := make(chan error, 1), make(chan struct{})
				go func() {
					defer close(exited)
					done <- waitHealthy(ctx, e, "s", "op", 2, startBudget{limit: 3}, 5*time.Second)
				}()
				defer func() { cancel(); <-exited }()
				synctest.Wait()
				select {
				case err := <-done:
					t.Fatalf("returned before coverage was interrupted: %v", err)
				default:
				}
				time.Sleep(4 * time.Second)
				switch cause {
				case "missing slot":
					e.tasks = []engine.ServiceTask{one}
				case "task list":
					e.taskErr = errors.New("unavailable")
				case "container inspect":
					e.healthErr = errors.New("unavailable")
				}
				e.send(healthStatus("s"))
				synctest.Wait()
				e.tasks, e.taskErr, e.healthErr = []engine.ServiceTask{one, two}, nil, nil
				e.send(healthStatus("s"))
				synctest.Wait()
				time.Sleep(5*time.Second - time.Millisecond)
				synctest.Wait()
				select {
				case err := <-done:
					t.Fatalf("returned before replacement interval: %v", err)
				default:
				}
				time.Sleep(time.Millisecond)
				synctest.Wait()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatal("did not confirm restored coverage")
				}
			})
		})
	}
}

func TestRolloutCountsFailuresDuringTheMonitor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		running := runningTask("running", "op")
		e := &monitorHealth{eventedHealth: newEventedHealth(), tasks: []engine.ServiceTask{running}}
		ctx, cancel := context.WithCancel(context.Background())
		done, exited := make(chan error, 1), make(chan struct{})
		go func() {
			defer close(exited)
			done <- withRole(waitHealthy(ctx, e, "s", "op", 1, startBudget{limit: 2}, 5*time.Second), "web")
		}()
		defer func() { cancel(); <-exited }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("returned before failures: %v", err)
		default:
		}
		time.Sleep(2 * time.Second)
		e.tasks = []engine.ServiceTask{running, failedTask("one", "failed", "op"), failedTask("old", "failed", "old")}
		e.send(healthStatus("s"))
		synctest.Wait()
		time.Sleep(2 * time.Second)
		e.send(healthStatus("s"))
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("returned before replacement interval: %v", err)
		default:
		}
		e.tasks = []engine.ServiceTask{running, failedTask("one", "failed", "op"), failedTask("two", "rejected", "op")}
		e.send(healthStatus("s"))
		synctest.Wait()
		select {
		case err := <-done:
			var gave *errStartAttempts
			if !errors.As(err, &gave) || gave.Failed != 2 || gave.Limit != 2 || gave.Role != "web" {
				t.Fatalf("failure = %v", err)
			}
		default:
			t.Fatal("did not count failed starts with a healthy replacement")
		}
	})
	t.Run("repeated failure does not reset", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			running := runningTask("running", "op")
			e := &monitorHealth{eventedHealth: newEventedHealth(), tasks: []engine.ServiceTask{running}}
			ctx, cancel := context.WithCancel(context.Background())
			done, exited := make(chan error, 1), make(chan struct{})
			go func() {
				defer close(exited)
				done <- waitHealthy(ctx, e, "s", "op", 1, startBudget{limit: 2}, 5*time.Second)
			}()
			defer func() { cancel(); <-exited }()
			synctest.Wait()
			time.Sleep(time.Second)
			e.tasks = []engine.ServiceTask{running, failedTask("one", "failed", "op")}
			e.send(healthStatus("s"))
			synctest.Wait()
			time.Sleep(3 * time.Second)
			e.send(healthStatus("s"))
			synctest.Wait()
			time.Sleep(2*time.Second - time.Millisecond)
			synctest.Wait()
			select {
			case err := <-done:
				t.Fatalf("returned before replacement interval: %v", err)
			default:
			}
			time.Sleep(time.Millisecond)
			synctest.Wait()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("repeated ID reset the interval")
			}
		})
	})
}

func TestWaitHealthyGivesUpAtTheStartAttemptLimit(t *testing.T) {
	// A long deadline: the wait must end because of the failures, not the clock.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := healthFake{tasks: []engine.ServiceTask{failedTask("t1", "failed", "op"), failedTask("t2", "rejected", "op"), failedTask("t3", "failed", "op"), runningTask("t4", "op")}, status: "starting"}
	start := time.Now()
	err := waitHealthy(ctx, e, "s", "op", 1, startBudget{limit: 3}, 0)
	var gave *errStartAttempts
	if !errors.As(err, &gave) || gave.Failed != 3 || gave.Limit != 3 {
		t.Fatalf("error %v", err)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatalf("took %v: waited for the deadline", time.Since(start))
	}
}

func TestWaitHealthyIgnoresFailuresOfOtherGenerationsAndBelowTheLimit(t *testing.T) {
	e := healthFake{tasks: []engine.ServiceTask{failedTask("o1", "failed", "other"), failedTask("o2", "failed", "other"), failedTask("o3", "failed", "other"), failedTask("o4", "failed", "other"), failedTask("t1", "failed", "op"), failedTask("t2", "failed", "op")}, status: "starting"}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	err := waitHealthy(ctx, e, "s", "op", 1, startBudget{limit: 3}, 0)
	var gave *errStartAttempts
	if err == nil || errors.As(err, &gave) {
		t.Fatalf("error %v, want the deadline", err)
	}
}

func TestWaitHealthyWithoutALimitNeverGivesUp(t *testing.T) {
	var tasks []engine.ServiceTask
	for i := 0; i < 20; i++ {
		tasks = append(tasks, failedTask(fmt.Sprintf("t%d", i), "failed", "op"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	err := waitHealthy(ctx, healthFake{tasks: tasks, status: "starting"}, "s", "op", 1, startBudget{}, 0)
	var gave *errStartAttempts
	if err == nil || errors.As(err, &gave) {
		t.Fatalf("error %v, want the deadline", err)
	}
}

// Swarm keeps few old tasks, so the count must not depend on seeing every failure at once.
func TestWaitHealthyCountsFailuresAcrossPolls(t *testing.T) {
	e := &seqHealth{step: func(poll int) []engine.ServiceTask {
		return []engine.ServiceTask{failedTask(fmt.Sprintf("t%d", poll), "failed", "op")} // only the newest failure is ever listed
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := waitHealthy(ctx, e, "s", "op", 1, startBudget{limit: 3}, 0)
	var gave *errStartAttempts
	if !errors.As(err, &gave) || gave.Failed != 3 || e.polls != 3 {
		t.Fatalf("error %v after %d polls", err, e.polls)
	}
}

func TestWaitHealthySucceedsAfterFailuresBelowTheLimit(t *testing.T) {
	e := &seqHealth{step: func(poll int) []engine.ServiceTask {
		if poll <= 2 {
			return []engine.ServiceTask{failedTask(fmt.Sprintf("t%d", poll), "failed", "op")}
		}
		return []engine.ServiceTask{failedTask("t1", "failed", "op"), failedTask("t2", "failed", "op"), runningTask("t3", "op")}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := waitHealthy(ctx, e, "s", "op", 1, startBudget{limit: 3}, 0); err != nil {
		t.Fatalf("a role that started on its third try failed: %v", err)
	}
}

func TestCreateRoleStopsAtTheStartAttemptLimit(t *testing.T) {
	desired := json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Image":"x","Labels":{"berthy.operation":"op"}}}}`)
	m := &mutationFake{live: engine.ServiceSnapshot{ID: "id", Spec: desired}, tasks: onService("id", failedTask("t1", "failed", "op"), failedTask("t2", "failed", "op"))}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := createRole(ctx, &journalFake{}, m, "op", state.Intent{Kind: "create", Role: "web", Name: "svc", Generation: "op", Desired: desired}, nil, 1, startBudget{limit: 2}, 0)
	var gave *errStartAttempts
	if !errors.As(err, &gave) || gave.Role != "web" || gave.Failed != 2 || gave.Limit != 2 {
		t.Fatalf("error %v", err)
	}
}

func TestUpdateRoleStopsAtTheStartAttemptLimit(t *testing.T) {
	before := json.RawMessage(`{"Name":"svc","version":"old"}`)
	desired := json.RawMessage(`{"Name":"svc","version":"new"}`)
	m := &mutationFake{live: engine.ServiceSnapshot{ID: "id", Version: 9, Spec: before}, tasks: onService("id", failedTask("t1", "failed", "op"), runningTask("t0", "old"))}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := updateRole(ctx, &journalFake{}, m, "op", state.Intent{Kind: "update", Role: "web", Name: "svc", ServiceID: "id", Generation: "op", Before: before, Desired: desired}, nil, 1, startBudget{limit: 1}, 0)
	var gave *errStartAttempts
	if !errors.As(err, &gave) || gave.Role != "web" || gave.Failed != 1 {
		t.Fatalf("error %v", err)
	}
}

func TestServiceSpecLetsBerthyDecideWhenAnUpdateFails(t *testing.T) {
	one := 1
	b, err := serviceSpec("env", "proj", "app", "web", (&manifest.Manifest{Project: "proj"}).ServiceName("env", "app", "web"), "op", "network", "image", func(v string) string { return (&manifest.Manifest{Project: "proj"}).VolumeName("env", v) }, nil, manifest.Role{Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}, "start-first", "10s", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		UpdateConfig, RollbackConfig map[string]any
		TaskTemplate                 struct{ RestartPolicy map[string]any }
	}
	if err = json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	// Swarm keeps replacing a failing new task; Berthy counts them and compensates when its attempts are used up.
	if v.UpdateConfig["FailureAction"] != "continue" || v.UpdateConfig["Order"] != "start-first" {
		t.Fatalf("update config %v", v.UpdateConfig)
	}
	if v.RollbackConfig["FailureAction"] != "rollback" || v.RollbackConfig["Order"] != "start-first" {
		t.Fatalf("rollback config %v", v.RollbackConfig)
	}
	if v.UpdateConfig["Monitor"] != float64(5*time.Second) || v.RollbackConfig["Monitor"] != float64(5*time.Second) {
		t.Fatalf("monitor durations: update %v, rollback %v", v.UpdateConfig["Monitor"], v.RollbackConfig["Monitor"])
	}
	rp := v.TaskTemplate.RestartPolicy
	if rp["Condition"] != "any" || rp["Delay"] != float64(5*time.Second) || rp["MaxAttempts"] != float64(0) {
		t.Fatalf("restart policy %v", rp)
	}
}

// dockerStoredContainer is the shape docker service create leaves in a ContainerSpec: settings Berthy does not use are
// present, with empty values.
const dockerStoredContainer = `{"Image":"old","Init":false,"Hosts":[],"DNSConfig":{},"Isolation":"default","Privileges":null,"Sysctls":{},"Secrets":[],"Configs":[]}`

func inheritedSpec(container string) json.RawMessage {
	return json.RawMessage(`{"Name":"svc","TaskTemplate":{"Runtime":"container","ContainerSpec":` + container + `,"Networks":[{"Target":"net"}]},"Mode":{"Replicated":{"Replicas":1}}}`)
}

func TestInheritedSettingsAreEmptyWhenDockerStoredThemEmpty(t *testing.T) {
	live := inheritedSpec(dockerStoredContainer)
	if err := validateInheritedRuntime(live, nil); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if err := adoptableService(live, "net"); err != nil {
		t.Fatalf("adopt: %v", err)
	}
}

func TestInheritedSettingsWithAValueAreStillRefused(t *testing.T) {
	for name, container := range map[string]string{
		"nameservers": `{"Image":"old","DNSConfig":{"Nameservers":["1.1.1.1"]}}`,
		"init":        `{"Image":"old","Init":true}`,
		"hosts":       `{"Image":"old","Hosts":["10.0.0.1 evil"]}`,
		"sysctls":     `{"Image":"old","Sysctls":{"net.ipv4.ip_forward":"1"}}`,
		"privileges":  `{"Image":"old","Privileges":{"NoNewPrivileges":false,"SELinuxContext":{"Disable":true}}}`,
		"isolation":   `{"Image":"old","Isolation":"hyperv"}`,
	} {
		live := inheritedSpec(container)
		if adoptableService(live, "net") == nil {
			t.Errorf("%s: adoption accepted it", name)
		}
		if name != "isolation" && validateInheritedRuntime(live, nil) == nil {
			t.Errorf("%s: validate accepted it", name)
		}
	}
}
