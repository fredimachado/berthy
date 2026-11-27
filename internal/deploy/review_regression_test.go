package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/engine"
	"berthy/internal/state"
)

type straddlingHealth struct {
	*eventedHealth
	tasks         []engine.ServiceTask
	started       chan struct{}
	release       chan struct{}
	slow, stopped bool
}

func (f *straddlingHealth) ServiceTasks(context.Context, string) ([]engine.ServiceTask, error) {
	f.polls.Add(1)
	return f.tasks, nil
}
func (f *straddlingHealth) InspectContainerHealth(ctx context.Context, id string) (string, error) {
	if id == "c-two" && f.slow {
		f.slow = false
		close(f.started)
		select {
		case <-f.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if id == "c-one" && f.stopped {
		return "unhealthy", nil
	}
	return "healthy", nil
}
func TestRolloutPollMustStartAfterHoldBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		one, two := runningTask("one", "op"), runningTask("two", "op")
		two.Slot = 2
		f := &straddlingHealth{eventedHealth: newEventedHealth(), tasks: []engine.ServiceTask{one, two}, started: make(chan struct{}), release: make(chan struct{})}
		ctx, cancel := context.WithCancel(context.Background())
		done, exited := make(chan error, 1), make(chan struct{})
		go func() { defer close(exited); done <- waitHealthy(ctx, f, "s", "op", 2, 1, 5*time.Second) }()
		defer func() { cancel(); <-exited }()
		synctest.Wait()
		time.Sleep(4800 * time.Millisecond)
		f.slow = true
		f.send(healthStatus("s"))
		synctest.Wait()
		select {
		case <-f.started:
		default:
			t.Fatal("slow poll did not start")
		}
		time.Sleep(100 * time.Millisecond)
		f.stopped = true
		time.Sleep(200 * time.Millisecond)
		close(f.release)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("straddling poll confirmed stopped replica: %v", err)
		default:
		}
		if f.polls.Load() < 3 {
			t.Fatal("no fresh poll after boundary")
		}
		f.stopped = false
		f.send(healthStatus("s"))
		synctest.Wait()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("healthy replacement interval did not confirm")
		}
	})
}

type blockedReceiptEngine struct {
	*budgetEngine
	armed              bool
	blockedAt, endedAt time.Time
}

func (f *blockedReceiptEngine) CreateService(ctx context.Context, spec json.RawMessage, auth *engine.RegistryAuth) (string, error) {
	id, err := f.budgetEngine.CreateService(ctx, spec, auth)
	if err == nil {
		f.armed = true
	}
	return id, err
}
func (f *blockedReceiptEngine) UpdateService(ctx context.Context, id string, version uint64, spec json.RawMessage, auth *engine.RegistryAuth) error {
	err := f.budgetEngine.UpdateService(ctx, id, version, spec, auth)
	if err == nil && f.blockedAt.IsZero() {
		f.armed = true
	}
	return err
}
func (f *blockedReceiptEngine) InspectService(ctx context.Context, ref string) (engine.ServiceSnapshot, error) {
	if f.armed {
		f.armed = false
		f.blockedAt = time.Now()
		<-ctx.Done()
		f.endedAt = time.Now()
		return engine.ServiceSnapshot{}, ctx.Err()
	}
	return f.budgetEngine.InspectService(ctx, ref)
}
func TestReceiptInspectionStopsAtStartupCutoff(t *testing.T) {
	for _, kind := range []string{"create", "update"} {
		t.Run(kind, func(t *testing.T) {
			s := openStore(t)
			base := &budgetEngine{boundFake: &boundFake{}}
			if kind == "update" {
				seedBudgetUpdate(t, s, base)
			}
			f := &blockedReceiptEngine{budgetEngine: base}
			m := budgetManifest()
			app := m.Apps["web"]
			delete(app.Roles, "b")
			m.Apps["web"] = app
			ctx := context.Background()
			op, _, err := s.Admit(ctx, state.Admission{RequestID: "receipt", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			c := &Controller{Store: s, Engine: f, Environment: "test"}
			synctest.Test(t, func(t *testing.T) {
				c.execute(ctx, op, Actor{ID: "a", Admin: true}, Request{RequestID: "receipt", App: "web", Version: digestOfLetter("a"), Manifest: m})
			})
			if f.blockedAt.IsZero() {
				t.Fatal("receipt inspection never started after mutation")
			}
			if got := f.endedAt.Sub(f.start); got != 10*time.Second {
				t.Fatalf("receipt inspection ended at %v, want startup cutoff 10s", got)
			}
			op, err = s.Operation(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := "failed"
			if kind == "update" {
				want = "rolled_back"
			}
			if op.State != want || op.Reason != "deploy deadline exceeded before the task was healthy" {
				t.Fatalf("operation %s reason %q", op.State, op.Reason)
			}
			if _, err := s.Lock(ctx); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("lock after compensation: %v", err)
			}
			live, err := base.boundFake.InspectService(ctx, "proj-test-web-a")
			if kind == "create" {
				if err == nil {
					t.Fatalf("created service remains: %+v", live)
				}
			} else if err != nil || !matchesStoredSpec(json.RawMessage(`{"Name":"proj-test-web-a","TaskTemplate":{"ContainerSpec":{"Image":"example/web@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","Labels":{"berthy.operation":"previous"}}},"Mode":{"Replicated":{"Replicas":1}}}`), live.Spec) {
				t.Fatalf("previous service not restored: %+v %v", live, err)
			}
		})
	}
}

type blockedObservedJournal struct {
	journalFake
	endedAt time.Time
}

func (j *blockedObservedJournal) MarkStep(ctx context.Context, op string, ordinal int, phase string, receipt json.RawMessage, events ...state.Event) error {
	if phase == "observed" {
		<-ctx.Done()
		j.endedAt = time.Now()
		return ctx.Err()
	}
	return j.journalFake.MarkStep(ctx, op, ordinal, phase, receipt, events...)
}
func TestObservedReceiptWriteStopsAtStartupCutoff(t *testing.T) {
	for _, kind := range []string{"create", "update"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				before, desired := json.RawMessage(`{"Name":"svc","version":"old"}`), json.RawMessage(`{"Name":"svc","version":"new"}`)
				m := &mutationFake{live: engine.ServiceSnapshot{ID: "id", Version: 9, Spec: before}}
				if kind == "create" {
					m.live.Spec = desired
				}
				j := &blockedObservedJournal{}
				i := state.Intent{Kind: kind, Name: "svc", ServiceID: "id", Before: before, Desired: desired, Generation: "op"}
				var err error
				if kind == "create" {
					_, err = createRole(ctx, j, m, "op", i, nil, 1, 1, 5*time.Second)
				} else {
					err = updateRole(ctx, j, m, "op", i, nil, 1, 1, 5*time.Second)
				}
				if j.endedAt.IsZero() {
					t.Fatalf("observed write never started: %v", err)
				}
				if got := j.endedAt.Sub(start); got != 10*time.Second {
					t.Fatalf("observed write ended at %v, want 10s", got)
				}
				if err == nil || err.Error() != "service health deadline exceeded" {
					t.Fatalf("error = %v", err)
				}
			})
		})
	}
}
