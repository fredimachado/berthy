package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/engine"
	"berthy/internal/state"
)

type confirmationEngine struct {
	mutationFake
	start            time.Time
	sawStopped       atomic.Bool
	healthy, stopped string
}

func (f *confirmationEngine) ServiceTasks(context.Context, string) ([]engine.ServiceTask, error) {
	if f.start.IsZero() {
		f.start = time.Now()
	}
	if time.Since(f.start) >= 6*time.Second {
		return onService("id", failedTask("candidate", "failed", "op")), nil
	}
	return onService("id", runningTask("candidate", "op")), nil
}

func (f *confirmationEngine) InspectContainerHealth(context.Context, string) (string, error) {
	if time.Since(f.start) >= 5*time.Second {
		f.sawStopped.Store(true)
		return f.stopped, nil
	}
	return f.healthy, nil
}

func confirmationHealthVerdicts(t *testing.T) (string, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "berthy-confirm-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	var running atomic.Bool
	var calls atomic.Int32
	running.Store(true)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/v1.44/containers/candidate/json" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprintf(w, `{"State":{"Running":%t,"Health":{"Status":"healthy"}}}`, running.Load())
	})}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	defer func() { _ = server.Close(); <-done; _ = os.RemoveAll(dir) }()
	docker := engine.New(path)
	healthy, err := docker.InspectContainerHealth(context.Background(), "candidate")
	if err != nil {
		t.Fatal(err)
	}
	running.Store(false)
	stopped, err := docker.InspectContainerHealth(context.Background(), "candidate")
	if err != nil {
		t.Fatal(err)
	}
	if healthy != "healthy" || calls.Load() != 2 {
		t.Fatalf("initial health %q with %d inspections", healthy, calls.Load())
	}
	return healthy, stopped
}

type confirmationJournal struct {
	journalFake
	verified atomic.Bool
	ready    atomic.Bool
}

func (j *confirmationJournal) MarkStep(_ context.Context, _ string, _ int, phase string, _ json.RawMessage, events ...state.Event) error {
	if phase == "verified" {
		j.verified.Store(true)
	}
	for _, event := range events {
		var data struct {
			Status string `json:"status"`
		}
		if event.Kind == "rollout" && json.Unmarshal(event.Data, &data) == nil && data.Status == "ready" {
			j.ready.Store(true)
		}
	}
	return nil
}

func TestRolloutRejectsAStoppedContainerAtConfirmation(t *testing.T) {
	healthy, stopped := confirmationHealthVerdicts(t)
	synctest.Test(t, func(t *testing.T) {
		spec := json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Labels":{"berthy.operation":"op"}}}}`)
		f := &confirmationEngine{mutationFake: mutationFake{live: engine.ServiceSnapshot{ID: "id", Spec: spec}}, healthy: healthy, stopped: stopped}
		j := &confirmationJournal{}
		ctx, cancel := context.WithCancel(context.Background())
		done, exited := make(chan error, 1), make(chan struct{})
		go func() {
			defer close(exited)
			_, err := createRole(ctx, j, f, "op", state.Intent{Kind: "create", Role: "web", Name: "svc", Generation: "op", Desired: spec}, nil, 1, startBudget{limit: 1}, rolloutTiming{hold: rolloutMonitor})
			done <- err
		}()
		defer func() { cancel(); <-exited }()
		synctest.Wait()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if !f.sawStopped.Load() {
			t.Fatal("confirmation did not inspect the stopped container")
		}
		select {
		case err := <-done:
			t.Fatalf("role ended at the stopped confirmation: %v", err)
		default:
		}
		if j.verified.Load() || j.ready.Load() {
			t.Fatal("stopped container was verified or marked ready")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		select {
		case err := <-done:
			var gave *errStartAttempts
			if !errors.As(err, &gave) || gave.Failed != 1 || gave.Limit != 1 || gave.Role != "web" {
				t.Fatalf("error = %v", err)
			}
		default:
			t.Fatal("failed task did not exhaust the start limit")
		}
		if j.verified.Load() || j.ready.Load() {
			t.Fatal("failed role was verified or marked ready")
		}
	})
}

type recoveryObservationEngine struct {
	*positiveEngine
	healthReads, creates, updates, deletes int
}

func (f *recoveryObservationEngine) InspectContainerHealth(context.Context, string) (string, error) {
	f.healthReads++
	return "unhealthy", nil
}

func (f *recoveryObservationEngine) CreateService(ctx context.Context, spec json.RawMessage, auth *engine.RegistryAuth) (string, error) {
	f.creates++
	return f.positiveEngine.CreateService(ctx, spec, auth)
}

func (f *recoveryObservationEngine) UpdateService(ctx context.Context, id string, version uint64, spec json.RawMessage, auth *engine.RegistryAuth) error {
	f.updates++
	return f.positiveEngine.UpdateService(ctx, id, version, spec, auth)
}

func (f *recoveryObservationEngine) DeleteService(ctx context.Context, id string) error {
	f.deletes++
	return f.positiveEngine.DeleteService(ctx, id)
}

func TestRecoveryKeepsUnhealthyCandidateAfterRecordedBarrier(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	prior, _, err := s.Admit(ctx, state.Admission{RequestID: "prior", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, prior.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "web", Digest: "previous-digest", Manifest: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	previous, err := s.Applied(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "candidate", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := json.Marshal(map[string]any{
		"Name": "svc", "Labels": map[string]string{"berthy.operation": op.ID},
		"TaskTemplate": map[string]any{"ContainerSpec": map[string]any{"Labels": map[string]string{"berthy.operation": op.ID}}},
		"Mode":         map[string]any{"Replicated": map[string]int{"Replicas": 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	intent := state.Intent{Kind: "update", Role: "web", Name: "svc", ServiceID: "id", Generation: op.ID, Before: json.RawMessage(`{"Name":"previous"}`), Desired: spec}
	if err := s.RecordIntent(ctx, op.ID, intent); err != nil {
		t.Fatal(err)
	}
	receipt, err := json.Marshal(struct {
		ID   string          `json:"id"`
		Spec json.RawMessage `json:"spec"`
	}{"id", spec})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkStep(ctx, op.ID, 0, "verified", receipt); err != nil {
		t.Fatal(err)
	}
	out := state.Outcome{State: "succeeded", Release: &state.Release{App: "web", Digest: "candidate-digest", Manifest: testReleaseManifest(t), Bindings: []state.Binding{{App: "web", Role: "web", Name: "svc", ServiceID: "id"}}}}
	barrier, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordIntent(ctx, op.ID, state.Intent{Ordinal: 9999, Kind: "barrier", Desired: barrier, NoReturn: reasonSwitched}); err != nil {
		t.Fatal(err)
	}
	f := &recoveryObservationEngine{positiveEngine: &positiveEngine{spec: spec}}
	c := &Controller{Engine: f, Store: s, Environment: "test"}
	if err := c.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if f.healthReads == 0 {
		t.Fatal("recovery did not inspect candidate health")
	}
	got, err := s.Operation(ctx, op.ID)
	if err != nil || got.State != "recovery_required" {
		t.Fatalf("candidate recovery = %+v, %v; want recovery_required", got, err)
	}
	lock, err := s.Lock(ctx)
	if err != nil || lock.OperationID != op.ID {
		t.Fatalf("candidate lock = %+v, %v", lock, err)
	}
	if f.creates != 0 || f.updates != 0 || f.deletes != 0 {
		t.Fatalf("recovery mutated services: create=%d update=%d delete=%d", f.creates, f.updates, f.deletes)
	}
	applied, err := s.Applied(ctx, "web")
	if err != nil || applied.ID != previous.ID || applied.Digest != previous.Digest {
		t.Fatalf("previous applied release = %+v, %v; want ID %d digest %q", applied, err, previous.ID, previous.Digest)
	}
}

func TestBarrierRecoveryUsesCandidateUptime(t *testing.T) {
	const generation = "candidate"
	spec := json.RawMessage(`{"Name":"svc","Mode":{"Replicated":{"Replicas":1}},"TaskTemplate":{"ContainerSpec":{"Healthcheck":{"Test":["NONE"]},"Labels":{"berthy.operation":"candidate"}}}}`)
	receipt, err := json.Marshal(map[string]any{"id": "id", "spec": spec})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(uptimeManifest("20s", "1m"))
	if err != nil {
		t.Fatal(err)
	}
	barrier, err := json.Marshal(state.Outcome{State: "succeeded", Release: &state.Release{App: "web", Manifest: raw}})
	if err != nil {
		t.Fatal(err)
	}
	intents := []state.Intent{{Kind: "create", Role: "web", Name: "svc", Phase: "verified", Generation: generation, Receipt: receipt}}
	for _, tc := range []struct {
		name string
		age  time.Duration
		want string
	}{
		{"qualified", 20 * time.Second, "post-deploy completion was interrupted"},
		{"young replacement", 19 * time.Second, "final health verification failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newDiagFake()
				f.spec = spec
				task := runningTask("latest", generation)
				task.ServiceID, task.RunningSince = "id", time.Now().Add(-tc.age)
				f.tasksFor = func(_, _ string) []engine.ServiceTask { return []engine.ServiceTask{task} }
				f.diagnose = map[string]engine.ContainerDiagnosis{task.ContainerID: {State: "running", Health: "none"}}
				c := Controller{Engine: f}
				out, err := c.recoverBarrier(context.Background(), intents, state.Intent{Desired: barrier})
				if tc.name == "qualified" {
					if err != nil || out.State != "failed" || out.Err != tc.want || out.Release == nil {
						t.Fatalf("qualified recovery = %+v, %v", out, err)
					}
				} else if err == nil || err.Error() != tc.want || out.Release != nil {
					t.Fatalf("young recovery = %+v, %v", out, err)
				}
			})
		})
	}
}
