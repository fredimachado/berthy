package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

const probeBindJSON = `{"Type":"bind","Source":"/usr/local/bin/berthy","Target":"/.berthy/berthy","ReadOnly":true}`

func uptimeRuntimeSpec(generation, check, mounts string) json.RawMessage {
	if check != "" {
		mounts = `,"Healthcheck":{"Test":` + check + `}` + mounts
	}
	return json.RawMessage(`{"Name":"existing","TaskTemplate":{"Networks":[{"Target":"net"}],"ContainerSpec":{"Image":"docker.io/example/web@` + digestOfLetter("a") + `","Labels":{"berthy.operation":"` + generation + `"}` + mounts + `}},"Mode":{"Replicated":{"Replicas":1}}}`)
}

type uptimeRuntimeEngine struct {
	runtimeEngine
	taskReads, execs  int
	updated, deadline time.Time
	startup           time.Duration
	replicaStarts     []time.Duration
	uptime            time.Duration
	tasks             []engine.ServiceTask
	health            func() string
	execContainer     string
}

func (f *uptimeRuntimeEngine) UpdateService(ctx context.Context, id string, version uint64, spec json.RawMessage, auth *engine.RegistryAuth) error {
	f.updated = time.Now()
	f.deadline, _ = ctx.Deadline()
	return f.runtimeEngine.UpdateService(ctx, id, version, spec, auth)
}

func (f *uptimeRuntimeEngine) generation() string {
	var spec struct {
		TaskTemplate struct {
			ContainerSpec struct{ Labels map[string]string }
		}
	}
	_ = json.Unmarshal(f.spec, &spec)
	return spec.TaskTemplate.ContainerSpec.Labels["berthy.operation"]
}

func (f *uptimeRuntimeEngine) ServiceTasks(context.Context, string) ([]engine.ServiceTask, error) {
	f.taskReads++
	if f.tasks != nil {
		return append([]engine.ServiceTask(nil), f.tasks...), nil
	}
	if len(f.replicaStarts) != 0 {
		var tasks []engine.ServiceTask
		for i := range f.replicaStarts {
			task := runningTask(strconv.Itoa(i+1), f.generation())
			task.ServiceID, task.Slot, task.ContainerID = "id", uint64(i+1), task.ID
			if f.uptime > 0 {
				task.RunningSince = f.updated.Add(f.replicaStarts[i] - f.uptime)
				if time.Now().Before(task.RunningSince) {
					task.State = "starting"
				}
			}
			tasks = append(tasks, task)
		}
		return tasks, nil
	}
	task := runningTask("task", f.generation())
	task.ServiceID, task.ContainerID, task.RunningSince = "id", "container", f.updated
	return []engine.ServiceTask{task}, nil
}

func (f *uptimeRuntimeEngine) InspectContainerHealth(_ context.Context, id string) (string, error) {
	if f.health != nil {
		return f.health(), nil
	}
	if f.uptime > 0 {
		return "none", nil
	}
	startup := f.startup
	if len(f.replicaStarts) != 0 {
		slot, err := strconv.Atoi(id)
		if err != nil || slot < 1 || slot > len(f.replicaStarts) {
			return "", errors.New("unexpected container")
		}
		startup = f.replicaStarts[slot-1]
	}
	if time.Since(f.updated) < startup {
		return "starting", nil
	}
	return "healthy", nil
}

func TestUptimeRestartAllowsAllReplacementBatches(t *testing.T) {
	const startup = 3*time.Minute + time.Second
	for _, tc := range []struct {
		kind        string
		parallelism int
		ready       []time.Duration
		allowance   time.Duration
	}{
		{"restart", 1, []time.Duration{startup, 2 * startup, 3 * startup}, 17 * time.Minute},
		{"restart", 2, []time.Duration{startup, startup, 2 * startup}, 12 * time.Minute},
		{"restart", 0, []time.Duration{startup, startup, startup}, 7 * time.Minute},
		{"scale", 1, []time.Duration{startup, 2 * startup, 3 * startup, startup}, 17 * time.Minute},
	} {
		t.Run(tc.kind+"/"+strconv.Itoa(tc.parallelism), func(t *testing.T) {
			s := openStore(t)
			m := uptimeManifest("3m", "5m")
			prior := seedUptimeRelease(t, s, m, boundBinding("existing", "id"))
			spec := uptimeRuntimeSpec(prior.ID, `["NONE"]`, "")
			spec = json.RawMessage(strings.Replace(string(spec), `"Replicas":1}}}`, `"Replicas":3}},"UpdateConfig":{"Parallelism":`+strconv.Itoa(tc.parallelism)+`}}`, 1))
			f := &uptimeRuntimeEngine{runtimeEngine: runtimeEngine{Engine: &boundFake{}, store: s, spec: spec}, replicaStarts: tc.ready, uptime: 3 * time.Minute}
			c := &Controller{Store: s, Engine: f, Environment: "test"}
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				start := time.Now()
				target := len(tc.ready)
				op, out := finishUptimeOperation(t, ctx, c, f, Request{Kind: tc.kind, App: "web", Replicas: &target})
				if out.State != "succeeded" || !f.deadline.Equal(start.Add(tc.allowance)) {
					t.Fatalf("%s %s (%s), deadline +%s, want succeeded at allowance %s", tc.kind, out.State, out.Err, f.deadline.Sub(start), tc.allowance)
				}
				var spec replacementSpec
				if err := json.Unmarshal(f.spec, &spec); err != nil || spec.Mode.Replicated == nil || spec.Mode.Replicated.Replicas != target || time.Since(start) != slices.Max(tc.ready) {
					t.Fatalf("replica mode %+v after %s, want %d at %s: %v", spec.Mode.Replicated, time.Since(start), target, slices.Max(tc.ready), err)
				}
				intents, err := s.Intents(ctx, op.ID)
				if err != nil || len(intents) != 1 || !intents[0].Deadline.Equal(f.deadline) {
					t.Fatalf("journal deadline does not match operation deadline: %v", err)
				}
				if _, err := s.Lock(ctx); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("lock: %v", err)
				}
			})
		})
	}
}

func TestCommandHealthOperationsAllowReplacementBatches(t *testing.T) {
	for _, tc := range []struct {
		kind  string
		ready []time.Duration
	}{
		{"restart", []time.Duration{45 * time.Second, 90 * time.Second, 135 * time.Second}},
		{"scale", []time.Duration{45 * time.Second, 90 * time.Second, 135 * time.Second, 45 * time.Second}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			s := openStore(t)
			m := dockerManifest(nil)
			app := m.Apps["web"]
			app.Deploy.DeployTimeout = "60s"
			role := app.Roles["web"]
			three, target := 3, len(tc.ready)
			role.Replicas = &three
			app.Roles["web"] = role
			m.Apps["web"] = app
			prior := seedUptimeRelease(t, s, m, boundBinding("existing", "id"))
			spec, err := serviceSpec("test", "proj", "web", "web", "existing", prior.ID, "net",
				"docker.io/example/web@"+digestOfLetter("a"), func(v string) string { return v }, nil, role, "start-first", "1s", nil, "")
			if err != nil {
				t.Fatal(err)
			}
			f := &uptimeRuntimeEngine{runtimeEngine: runtimeEngine{Engine: &boundFake{}, store: s, spec: spec}, replicaStarts: tc.ready}
			c := &Controller{Store: s, Engine: f, Environment: "test"}
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				start := time.Now()
				op, out := finishUptimeOperation(t, ctx, c, f, Request{Kind: tc.kind, App: "web", Replicas: &target})
				want := start.Add(5 * time.Minute)
				if out.State != "succeeded" || !f.deadline.Equal(want) {
					t.Fatalf("%s %s (%s), deadline +%s, want succeeded with deadline +5m", tc.kind, out.State, out.Err, f.deadline.Sub(start))
				}
				var live struct {
					Mode struct{ Replicated struct{ Replicas int } }
				}
				if err := json.Unmarshal(f.spec, &live); err != nil || live.Mode.Replicated.Replicas != target {
					t.Fatalf("replicas %d, want %d: %v", live.Mode.Replicated.Replicas, target, err)
				}
				intents, err := s.Intents(ctx, op.ID)
				if err != nil || len(intents) != 1 || !intents[0].Deadline.Equal(f.deadline) {
					t.Fatalf("journal deadline does not match operation deadline: %v", err)
				}
				if _, err := s.Lock(ctx); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("lock: %v", err)
				}
			})
		})
	}
}

func (f *uptimeRuntimeEngine) InspectContainer(_ context.Context, id string) (engine.ContainerSnapshot, error) {
	return engine.ContainerSnapshot{ID: id, Running: true, Labels: map[string]string{"berthy.operation": f.generation()}}, nil
}

func (f *uptimeRuntimeEngine) Exec(_ context.Context, id string, _ []string) (engine.ExecResult, error) {
	f.execs++
	f.execContainer = id
	return engine.ExecResult{}, nil
}

func finishUptimeOperation(t *testing.T, ctx context.Context, c *Controller, f *uptimeRuntimeEngine, req Request) (state.Operation, state.Outcome) {
	t.Helper()
	s := c.Store
	op, _, err := s.Admit(ctx, state.Admission{RequestID: req.Kind, ActorID: "admin", Kind: req.Kind, App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	f.op = op.ID
	out := c.runOperation(ctx, op, Actor{ID: "admin", Admin: true}, req)
	if err := s.Finish(context.Background(), op.ID, out); err != nil {
		t.Fatal(err)
	}
	return op, out
}

func TestManagedUptimeMountPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, duration, mounts string
		valid                  bool
	}{
		{"exact", "20s", probeBindJSON, false},
		{"wrong duration", "0s", probeBindJSON, false},
		{"no bind", "20s", "", true},
		{"named volume", "20s", `{"Type":"volume","Source":"data","Target":"/.berthy"}`, true},
		{"duplicate", "20s", probeBindJSON + "," + probeBindJSON, false},
		{"foreign options", "20s", strings.TrimSuffix(probeBindJSON, "}") + `,"BindOptions":{"Propagation":"shared"}}`, false},
		{"different source", "20s", strings.Replace(probeBindJSON, "/usr/local/bin/berthy", "/other", 1), false},
		{"different target", "20s", strings.Replace(probeBindJSON, "/.berthy/berthy", "/other", 1), false},
		{"writable", "20s", strings.Replace(probeBindJSON, "true", "false", 1), false},
		{"extra bind", "20s", probeBindJSON + `,{"Type":"bind","Source":"/foreign","Target":"/data"}`, false},
		{"volume shadows probe", "20s", probeBindJSON + `,{"Type":"volume","Source":"foreign","Target":"/.berthy/berthy"}`, false},
	} {
		for _, kind := range []string{"restart", "scale", "exec"} {
			t.Run(tc.name+"/"+kind, func(t *testing.T) {
				s := openStore(t)
				m := uptimeManifest("20s", "1m")
				prior := seedUptimeRelease(t, s, m, boundBinding("existing", "id"))
				mounts := ""
				if tc.mounts != "" {
					mounts = `,"Mounts":[` + tc.mounts + `]`
				}
				check := `["CMD","true"]`
				if !tc.valid {
					check = `["CMD","/.berthy/berthy","probe","uptime","` + tc.duration + `"]`
				}
				f := &uptimeRuntimeEngine{runtimeEngine: runtimeEngine{Engine: &boundFake{}, store: s, spec: uptimeRuntimeSpec(prior.ID, check, mounts)}}
				c := &Controller{Store: s, Engine: f, Environment: "test"}
				one := 1
				var op state.Operation
				var out state.Outcome
				synctest.Test(t, func(t *testing.T) {
					op, out = finishUptimeOperation(t, context.Background(), c, f, Request{Kind: kind, App: "web", Replicas: &one, Command: []string{"true"}})
				})
				if tc.valid {
					if out.State != "succeeded" || kind == "exec" && f.execs != 1 || kind != "exec" && f.updates != 1 {
						t.Fatalf("state %s, error %q, updates %d, execs %d", out.State, out.Err, f.updates, f.execs)
					}
				} else {
					intents, err := s.Intents(context.Background(), op.ID)
					if err != nil || out.State != "failed" || len(intents) != 0 || f.updates != 0 || f.execs != 0 || f.taskReads != 0 {
						t.Fatalf("state %s, intents %d (%v), task reads %d, updates %d, execs %d", out.State, len(intents), err, f.taskReads, f.updates, f.execs)
					}
				}
			})
		}
	}
	t.Run("adoption refuses even the exact bind", func(t *testing.T) {
		m := uptimeManifest("20s", "1m")
		app := m.Apps["web"]
		role := app.Roles["web"]
		role.Service = "existing"
		app.Roles["web"] = role
		f := &uptimeRuntimeEngine{runtimeEngine: runtimeEngine{Engine: &boundFake{}, spec: uptimeRuntimeSpec("old", `["CMD","/.berthy/berthy","probe","uptime","20s"]`, `,"Mounts":[`+probeBindJSON+`]`)}}
		c := &Controller{Engine: f, Environment: "test"}
		_, err := c.Plan(context.Background(), Actor{Admin: true}, Request{Adopt: true, App: "web", Manifest: m})
		if err == nil || err.Error() != "unsafe or incompatible adoption service" || f.taskReads != 0 || f.updates != 0 || f.execs != 0 {
			t.Fatalf("adoption returned %v, task reads %d, updates %d, execs %d", err, f.taskReads, f.updates, f.execs)
		}
	})
}

func TestUptimeRestartUsesAppliedStartupBudget(t *testing.T) {
	for _, caller := range []time.Duration{0, 4 * time.Minute} {
		t.Run(caller.String(), func(t *testing.T) {
			s := openStore(t)
			prior := seedUptimeRelease(t, s, uptimeManifest("3m", "5m"), boundBinding("existing", "id"))
			f := &uptimeRuntimeEngine{runtimeEngine: runtimeEngine{Engine: &boundFake{}, store: s, spec: uptimeRuntimeSpec(prior.ID, `["NONE"]`, "")}, uptime: 3 * time.Minute}
			c := &Controller{Store: s, Engine: f, Environment: "test"}
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				want := 7 * time.Minute
				if caller != 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, caller)
					defer cancel()
					want = caller
				}
				start := time.Now()
				op, out := finishUptimeOperation(t, ctx, c, f, Request{Kind: "restart", App: "web"})
				intents, err := s.Intents(context.Background(), op.ID)
				if out.State != "succeeded" || err != nil || len(intents) != 1 {
					t.Fatalf("state %s, error %q, context deadline +%s, intents %d (%v), want +%s", out.State, out.Err, f.deadline.Sub(start), len(intents), err, want)
				}
				if !intents[0].Deadline.Equal(f.deadline) || !f.deadline.Equal(start.Add(want)) {
					t.Fatalf("execution deadline +%s, journal deadline +%s, want +%s", f.deadline.Sub(start), intents[0].Deadline.Sub(start), want)
				}
				if _, err := s.Lock(context.Background()); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("lock: %v", err)
				}
			})
		})
	}
}

func TestRestartAfterAdoptionWaitsForRetainedDockerHealth(t *testing.T) {
	s := openStore(t)
	role := adoptionRole()
	spec := changedByHand(t, role, func(service, container, _ map[string]any) {
		delete(service, "Labels")
		delete(container, "Labels")
	})
	f := &uptimeRuntimeEngine{runtimeEngine: runtimeEngine{Engine: &boundFake{}, store: s, spec: spec}}
	c := &Controller{Store: s, Engine: f, Environment: "test"}
	threshold := "20s"
	role.Health = &manifest.Health{Uptime: &threshold}
	req := adoptionRequest(role)
	req.RequestID = "adopt"
	adopted := runUptimeRequest(t, c, req)
	release, err := s.Applied(context.Background(), "web")
	var applied manifest.Manifest
	if adopted.State != "succeeded" || err != nil || json.Unmarshal(release.Manifest, &applied) != nil || !sameSpec(f.spec, spec) {
		t.Fatalf("adoption %s (%s), applied release %d (%v), live spec %s", adopted.State, adopted.Err, release.ID, err, f.spec)
	}
	if got, err := applied.Apps["web"].Roles["web"].Health.UptimeThreshold(); err != nil || got != 20*time.Second {
		t.Fatalf("applied threshold %s: %v", got, err)
	}

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		op, _, err := s.Admit(ctx, state.Admission{RequestID: "restart", ActorID: "admin", Kind: "restart", App: "web", PublicRequest: json.RawMessage("{}")})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		replacement := runningTask("replacement", op.ID)
		replacement.ServiceID = "id"
		replacement.RunningSince = time.Now().Add(-21 * time.Second)
		f.op, f.tasks = op.ID, []engine.ServiceTask{replacement}
		polled, ready := make(chan struct{}, 1), make(chan struct{})
		f.health = func() string {
			select {
			case polled <- struct{}{}:
			default:
			}
			select {
			case <-ready:
				return "healthy"
			default:
				return "starting"
			}
		}
		result, done := make(chan state.Outcome, 1), make(chan struct{})
		go func() {
			defer close(done)
			result <- c.runOperation(ctx, op, Actor{Admin: true}, Request{Kind: "restart", App: "web"})
		}()
		defer func() { cancel(); <-done }()
		synctest.Wait()
		select {
		case out := <-result:
			t.Fatalf("restart returned before Docker health was ready: %+v", out)
		case <-polled:
		default:
			t.Fatal("restart did not check Docker health")
		}

		close(ready)
		out := <-result
		if out.State != "succeeded" || f.updates != 1 || !strings.Contains(string(f.spec), `"Test":["CMD","true"]`) {
			t.Fatalf("restart %s (%s), updates %d, spec %s", out.State, out.Err, f.updates, f.spec)
		}
		if err := s.Finish(ctx, op.ID, out); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Lock(ctx); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("lock: %v", err)
		}
	})
}

func TestWaitUsesAppliedRoleUptime(t *testing.T) {
	const threshold = 20 * time.Second
	for _, tc := range []struct {
		name, role, check string
		wantDelay         time.Duration
	}{
		{"uptime", "web", `["NONE"]`, threshold},
		{"adopted image check", "web", "", time.Second},
		{"command sibling", "aux", `["CMD","true"]`, time.Second},
		{"missing applied role", "missing", `["CMD","true"]`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			m := uptimeManifest("20s", "1m")
			m.Apps["web"].Roles["aux"] = dockerManifest(nil).Apps["web"].Roles["web"]
			prior := seedUptimeRelease(t, s, m, state.Binding{App: "web", Role: tc.role, Name: "existing", ServiceID: "id"})
			spec := uptimeRuntimeSpec(prior.ID, tc.check, "")
			f := &uptimeRuntimeEngine{runtimeEngine: runtimeEngine{Engine: &boundFake{}, store: s, spec: spec}}
			c := &Controller{Store: s, Engine: f, Environment: "test"}
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				task := runningTask("worker", prior.ID)
				task.ServiceID, task.RunningSince = "id", start.Add(-threshold)
				if tc.check == `["NONE"]` {
					task.RunningSince = start
				}
				f.tasks = []engine.ServiceTask{task}
				f.health = func() string {
					switch {
					case tc.check == `["NONE"]`:
						return "none"
					case tc.wantDelay > 0 && time.Since(start) < tc.wantDelay:
						return "starting"
					default:
						return "healthy"
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				err := c.Wait(ctx, "web", tc.role)
				if tc.role == "missing" {
					if err == nil || err.Error() != "applied role unavailable" || f.taskReads != 0 {
						t.Fatalf("missing role: wait returned %v after %d task reads", err, f.taskReads)
					}
				} else if err != nil || time.Since(start) != tc.wantDelay {
					t.Fatalf("wait after %s: %v, want success at %s", time.Since(start), err, tc.wantDelay)
				}
			})
		})
	}
}

func TestExecSelectsMatureUptimeTask(t *testing.T) {
	for _, tc := range []struct{ name, check, health, wantState, wantContainer string }{
		{"uptime", `["NONE"]`, "none", "succeeded", "c-mature"},
		{"retained command check", `["CMD","true"]`, "starting", "failed", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			prior := seedUptimeRelease(t, s, uptimeManifest("20s", "1m"), boundBinding("existing", "id"))
			f := &uptimeRuntimeEngine{
				runtimeEngine: runtimeEngine{Engine: &boundFake{}, store: s, spec: uptimeRuntimeSpec(prior.ID, tc.check, "")},
				health:        func() string { return tc.health },
			}
			c := &Controller{Store: s, Engine: f, Environment: "test"}
			synctest.Test(t, func(t *testing.T) {
				now := time.Now()
				young, mature := runningTask("young", prior.ID), runningTask("mature", prior.ID)
				young.Slot, young.RunningSince = 1, now.Add(-19*time.Second)
				mature.Slot, mature.RunningSince = 2, now.Add(-20*time.Second)
				f.tasks = onService("id", mature, young)
				_, out := finishUptimeOperation(t, context.Background(), c, f, Request{Kind: "exec", App: "web", Command: []string{"true"}})
				if out.State != tc.wantState || f.execContainer != tc.wantContainer {
					t.Fatalf("exec %s (%s) in %q, want %s in %q", out.State, out.Err, f.execContainer, tc.wantState, tc.wantContainer)
				}
			})
		})
	}
}
