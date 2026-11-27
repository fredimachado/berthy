package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

type uptimeRestoreEngine struct {
	boundFake
	restoredAt map[string]time.Time
	restored   []string
	deadline   time.Time
}

func (f *uptimeRestoreEngine) UpdateService(ctx context.Context, id string, _ uint64, spec json.RawMessage, _ *engine.RegistryAuth) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, live := range f.services {
		if live.ID != id {
			continue
		}
		f.services[i].Spec = spec
		if strings.Contains(string(spec), digestOfLetter("a")) {
			f.deadline, _ = ctx.Deadline()
			f.restoredAt[id] = time.Now()
			f.restored = append(f.restored, id)
		}
		return nil
	}
	return errors.New("unexpected service update")
}

func (f *uptimeRestoreEngine) ServiceTasks(ctx context.Context, id string) ([]engine.ServiceTask, error) {
	live, err := f.InspectService(ctx, id)
	if err != nil {
		return nil, err
	}
	tasks, err := f.boundFake.ServiceTasks(ctx, id)
	if err == nil && len(tasks) == 1 && id == "web" && strings.Contains(string(live.Spec), digestOfLetter("b")) {
		tasks[0].ID = "candidate"
		tasks[0].State, tasks[0].DesiredState = "failed", "shutdown"
	}
	var spec struct {
		Mode struct{ Replicated struct{ Replicas int } }
	}
	if err == nil && len(tasks) == 1 && json.Unmarshal(live.Spec, &spec) == nil && spec.Mode.Replicated.Replicas > 1 {
		first := tasks[0]
		for i := 1; i < spec.Mode.Replicated.Replicas; i++ {
			task := first
			task.ContainerID += "/" + strconv.Itoa(i+1)
			task.Slot = uint64(i + 1)
			tasks = append(tasks, task)
		}
	}
	return tasks, err
}

func (f *uptimeRestoreEngine) InspectContainerHealth(_ context.Context, container string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	service, slot, _ := strings.Cut(strings.TrimPrefix(container, "c-"), "/")
	timeToHealth := 3 * time.Minute
	if n, err := strconv.Atoi(slot); err == nil {
		timeToHealth = time.Duration(n) * (3*time.Minute + time.Second)
	}
	if at, ok := f.restoredAt[service]; ok && time.Since(at) < timeToHealth {
		return "starting", nil
	}
	return "healthy", nil
}

func TestStopFirstRestoresLongUptimePredecessor(t *testing.T) {
	for _, tc := range []struct {
		roles       []string
		auxReplicas int
	}{
		{[]string{"web"}, 0}, {[]string{"aux", "web"}, 1}, {[]string{"aux", "web"}, 3},
	} {
		roles := tc.roles
		t.Run(fmt.Sprintf("%d-roles-%d-aux", len(roles), tc.auxReplicas), func(t *testing.T) {
			s := openStore(t)
			f := &uptimeRestoreEngine{restoredAt: map[string]time.Time{}}
			c := &Controller{Store: s, Engine: f, Environment: "test", ProbeSource: testProbeSource, rolloutHold: zeroTestRolloutHold()}
			prior := uptimeManifest("3m", "5m")
			candidate := uptimeManifest("1s", "21s")
			for _, m := range []manifest.Manifest{prior, candidate} {
				app := m.Apps["web"]
				app.Deploy.Order = manifest.StopFirst
				withStartAttempts(1)(&app)
				for _, role := range roles {
					app.Roles[role] = app.Roles["web"]
				}
				if tc.auxReplicas != 0 {
					aux := app.Roles["aux"]
					aux.Replicas = &tc.auxReplicas
					app.Roles["aux"] = aux
				}
				m.Apps["web"] = app
			}
			var bindings []state.Binding
			for _, role := range roles {
				bindings = append(bindings, state.Binding{App: "web", Role: role, Name: prior.ServiceName("test", "web", role), ServiceID: role})
			}
			oldOperation := seedUptimeRelease(t, s, prior, bindings...)
			before := make(map[string]json.RawMessage)
			oldImage := "docker.io/example/web@" + digestOfLetter("a")
			for _, b := range bindings {
				spec, err := serviceSpec("test", "proj", "web", b.Role, b.Name, oldOperation.ID, "net", oldImage, func(v string) string { return v }, nil, prior.Apps["web"].Roles[b.Role], manifest.StopFirst, "1s", nil, "", fixtureProbeSource)
				if err != nil {
					t.Fatal(err)
				}
				before[b.Role] = spec
				f.add(b.ServiceID, spec)
			}
			var op state.Operation
			synctest.Test(t, func(t *testing.T) {
				op = runUptimeRequest(t, c, Request{RequestID: "candidate", App: "web", Version: digestOfLetter("b"), Manifest: candidate})
			})
			if op.State != "rolled_back" {
				t.Fatalf("state %s, error %q, restored %v, allowance %s", op.State, op.Err, f.restored, f.deadline.Sub(f.restoredAt["web"]))
			}
			if len(f.restored) != len(roles) || f.restored[0] != "web" {
				t.Fatalf("restored %v, want %v in reverse", f.restored, roles)
			}
			if len(roles) > 1 && f.restoredAt["aux"].Sub(f.restoredAt["web"]) < 3*time.Minute {
				t.Fatal("second restore did not wait for its predecessor")
			}
			if tc.auxReplicas == 3 && f.deadline.Sub(f.restoredAt["web"]) != 22*time.Minute {
				t.Fatalf("shared restore allowance %s, want 22m", f.deadline.Sub(f.restoredAt["web"]))
			}
			ctx := context.Background()
			applied, err := s.Applied(ctx, "web")
			if err != nil || applied.Digest != oldImage {
				t.Fatalf("applied digest %q, error %v", applied.Digest, err)
			}
			for _, role := range roles {
				live, err := f.InspectService(ctx, role)
				if err != nil || !sameSpec(live.Spec, before[role]) {
					t.Fatalf("%s predecessor spec not restored: %v", role, err)
				}
			}
			if _, err := s.Lock(ctx); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("lock: %v", err)
			}
		})
	}
}
