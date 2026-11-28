package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	uptime     bool
	restoredAt map[string]time.Time
	ready      map[string][]time.Duration
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
	if f.uptime {
		f.mu.Lock()
		restoredAt := f.restoredAt[id]
		f.mu.Unlock()
		for i := range tasks {
			task := &tasks[i]
			if task.State != "running" {
				continue
			}
			task.ID = task.ContainerID
			if restoredAt.IsZero() {
				task.RunningSince = time.Now().Add(-time.Second)
				continue
			}
			started := restoredAt.Add(time.Duration(i) * (3*time.Minute + time.Second))
			if time.Now().Before(started) {
				task.State = "starting"
			} else {
				task.RunningSince = started
			}
		}
	}
	return tasks, err
}

func (f *uptimeRestoreEngine) InspectContainerHealth(_ context.Context, container string) (string, error) {
	if f.uptime {
		return "none", nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	service, slot, _ := strings.Cut(strings.TrimPrefix(container, "c-"), "/")
	n, timeToHealth := 1, 3*time.Minute
	if slot != "" {
		var err error
		n, err = strconv.Atoi(slot)
		if err != nil || n < 1 {
			return "", errors.New("unexpected replica slot")
		}
		timeToHealth = time.Duration(n) * (3*time.Minute + time.Second)
	}
	if ready := f.ready[service]; len(ready) != 0 {
		if n > len(ready) {
			return "", errors.New("unexpected replica slot")
		}
		timeToHealth = ready[n-1]
	}
	if at, ok := f.restoredAt[service]; ok && time.Since(at) < timeToHealth {
		return "starting", nil
	}
	return "healthy", nil
}

func TestCompensationAllowsAllTouchedCommandHealthRoles(t *testing.T) {
	s := openStore(t)
	f := &uptimeRestoreEngine{
		restoredAt: map[string]time.Time{},
		ready: map[string][]time.Duration{
			"aux": {50 * time.Second, 50 * time.Second, 50 * time.Second, 50 * time.Second},
			"web": {50 * time.Second, 50 * time.Second, 100 * time.Second},
		},
	}
	c := &Controller{Store: s, Engine: f, Environment: "test", rolloutHold: zeroTestRolloutHold()}
	prior, candidate := dockerManifest(nil), dockerManifest(nil)
	one, three, four := 1, 3, 4
	for _, tc := range []struct {
		m       manifest.Manifest
		timeout string
		aux     *int
	}{
		{prior, "60s", &four}, {candidate, "10s", &one},
	} {
		app := tc.m.Apps["web"]
		app.Deploy.DeployTimeout = tc.timeout
		withStartAttempts(1)(&app)
		main := app.Roles["web"]
		main.Replicas = &three
		app.Roles["web"] = main
		aux := main
		aux.Replicas = tc.aux
		app.Roles["aux"] = aux
		tc.m.Apps["web"] = app
	}
	bindings := []state.Binding{
		{App: "web", Role: "aux", Name: prior.ServiceName("test", "web", "aux"), ServiceID: "aux"},
		boundBinding(prior.ServiceName("test", "web", "web"), "web"),
	}
	appliedManifest := prior.Defaulted()
	// Adoption records the requested uptime policy and keeps the live command check. A later restart adds a generation.
	appliedAux := appliedManifest.Apps["web"].Roles["aux"]
	appliedAux.Health = uptimeManifest("20s", "60s").Apps["web"].Roles["web"].Health
	appliedManifest.Apps["web"].Roles["aux"] = appliedAux
	old := seedUptimeRelease(t, s, appliedManifest, bindings...)
	before := make(map[string]json.RawMessage)
	oldImage := "docker.io/example/web@" + digestOfLetter("a")
	for _, b := range bindings {
		spec, err := serviceSpec("test", "proj", "web", b.Role, b.Name, old.ID, "net", oldImage, func(v string) string { return v },
			nil, prior.Apps["web"].Roles[b.Role], "start-first", "1s", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if b.Role == "web" {
			prefix, update, found := strings.Cut(string(spec), `"UpdateConfig":`)
			if !found || !strings.Contains(update, `"Parallelism":1`) {
				t.Fatal("predecessor spec lacks serial update config")
			}
			spec = json.RawMessage(prefix + `"UpdateConfig":` + strings.Replace(update, `"Parallelism":1`, `"Parallelism":2`, 1))
		}
		before[b.Role] = spec
		f.add(b.ServiceID, spec)
	}
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		op := runUptimeRequest(t, c, Request{RequestID: "command-health-candidate", App: "web", Version: digestOfLetter("b"), Manifest: candidate})
		// Two cleanup allowances and three startup windows: two for web and one for aux.
		if want := 7 * time.Minute; f.deadline.Sub(f.restoredAt["web"]) != want {
			t.Errorf("shared restore allowance %s, want %s", f.deadline.Sub(f.restoredAt["web"]), want)
		}
		if op.State != "rolled_back" {
			t.Fatalf("state %s, error %q, restored %v", op.State, op.Err, f.restored)
		}
		if !slices.Equal(f.restored, []string{"web", "aux"}) ||
			f.restoredAt["aux"].Sub(f.restoredAt["web"]) != 100*time.Second ||
			time.Since(f.restoredAt["web"]) != 150*time.Second {
			t.Fatalf("restore order %v, aux started at +%s, finished at +%s", f.restored,
				f.restoredAt["aux"].Sub(f.restoredAt["web"]), time.Since(f.restoredAt["web"]))
		}
		intents, err := s.Intents(ctx, op.ID)
		if err != nil || len(intents) != 2 || intents[0].Role != "aux" || intents[0].Phase != "verified" {
			t.Fatalf("candidate intents do not show aux verification before web failed: %v", err)
		}
		for _, b := range bindings {
			live, err := f.InspectService(ctx, b.ServiceID)
			if err != nil || !sameSpec(live.Spec, before[b.Role]) {
				t.Fatalf("%s predecessor spec not restored: %v", b.Role, err)
			}
			if err := healthyOnce(ctx, f, b.ServiceID, old.ID, *prior.Apps["web"].Roles[b.Role].Replicas, 0); err != nil {
				t.Fatalf("%s predecessor not healthy: %v", b.Role, err)
			}
		}
		applied, err := s.Applied(ctx, "web")
		if err != nil || applied.Digest != oldImage {
			t.Fatalf("applied digest %q, error %v", applied.Digest, err)
		}
		if _, err := s.Lock(ctx); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("lock: %v", err)
		}
	})
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
			f := &uptimeRestoreEngine{uptime: true, restoredAt: map[string]time.Time{}}
			c := &Controller{Store: s, Engine: f, Environment: "test", rolloutHold: zeroTestRolloutHold()}
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
				spec, err := serviceSpec("test", "proj", "web", b.Role, b.Name, oldOperation.ID, "net", oldImage, func(v string) string { return v }, nil, prior.Apps["web"].Roles[b.Role], manifest.StopFirst, "1s", nil, "")
				if err != nil {
					t.Fatal(err)
				}
				before[b.Role] = spec
				f.add(b.ServiceID, spec)
			}
			var op state.Operation
			synctest.Test(t, func(t *testing.T) {
				op = runUptimeRequest(t, c, Request{RequestID: "candidate", App: "web", Version: digestOfLetter("b"), Manifest: candidate})
				if op.State == "rolled_back" {
					last := f.restoredAt[f.restored[len(f.restored)-1]]
					want := 3*time.Minute + time.Duration(max(0, tc.auxReplicas-1))*(3*time.Minute+time.Second)
					if got := time.Since(last); got != want {
						t.Fatalf("previous replicas verified after %s, want %s", got, want)
					}
				}
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
			if tc.auxReplicas == 3 && f.deadline.Sub(f.restoredAt["web"]) != 24*time.Minute {
				t.Fatalf("shared restore allowance %s, want 24m", f.deadline.Sub(f.restoredAt["web"]))
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
