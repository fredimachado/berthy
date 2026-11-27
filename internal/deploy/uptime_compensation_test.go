package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
}

func (f *uptimeRestoreEngine) UpdateService(_ context.Context, id string, _ uint64, spec json.RawMessage, _ *engine.RegistryAuth) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, live := range f.services {
		if live.ID != id {
			continue
		}
		f.services[i].Spec = spec
		if strings.Contains(string(spec), digestOfLetter("a")) {
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
	return tasks, err
}

func (f *uptimeRestoreEngine) InspectContainerHealth(_ context.Context, container string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if at, ok := f.restoredAt[strings.TrimPrefix(container, "c-")]; ok && time.Since(at) < 3*time.Minute {
		return "starting", nil
	}
	return "healthy", nil
}

func TestStopFirstRestoresLongUptimePredecessor(t *testing.T) {
	for _, roles := range [][]string{{"web"}, {"aux", "web"}} {
		t.Run(fmt.Sprint(len(roles)), func(t *testing.T) {
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
				t.Fatalf("state %s, error %q, restored %v", op.State, op.Err, f.restored)
			}
			if len(f.restored) != len(roles) || f.restored[0] != "web" {
				t.Fatalf("restored %v, want %v in reverse", f.restored, roles)
			}
			if len(roles) > 1 && f.restoredAt["aux"].Sub(f.restoredAt["web"]) < 3*time.Minute {
				t.Fatal("second restore did not wait for its predecessor")
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
