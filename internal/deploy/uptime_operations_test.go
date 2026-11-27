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
	"berthy/internal/state"
)

const probeBindJSON = `{"Type":"bind","Source":"/usr/local/bin/berthy","Target":"/.berthy/berthy","ReadOnly":true}`

func managedUptimeSpec(generation, duration, mounts string) json.RawMessage {
	return json.RawMessage(`{"Name":"existing","TaskTemplate":{"Networks":[{"Target":"net"}],"ContainerSpec":{"Image":"docker.io/example/web@` + digestOfLetter("a") + `","Labels":{"berthy.operation":"` + generation + `"},"Healthcheck":{"Test":["CMD","/.berthy/berthy","probe","uptime","` + duration + `"]}` + mounts + `}},"Mode":{"Replicated":{"Replicas":1}}}`)
}

type uptimeRuntimeEngine struct {
	runtimeEngine
	taskReads, execs  int
	updated, deadline time.Time
	startup           time.Duration
	replicaStarts     []time.Duration
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
	if len(f.replicaStarts) != 0 {
		var tasks []engine.ServiceTask
		for i := range f.replicaStarts {
			task := runningTask(strconv.Itoa(i+1), f.generation())
			task.ServiceID, task.Slot, task.ContainerID = "id", uint64(i+1), task.ID
			tasks = append(tasks, task)
		}
		return tasks, nil
	}
	return []engine.ServiceTask{{ServiceID: "id", State: "running", DesiredState: "running", Slot: 1, ContainerID: "container", Labels: map[string]string{"berthy.operation": f.generation()}}}, nil
}

func (f *uptimeRuntimeEngine) InspectContainerHealth(_ context.Context, id string) (string, error) {
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
		parallelism int
		ready       []time.Duration
		allowance   time.Duration
	}{
		{1, []time.Duration{startup, 2 * startup, 3 * startup}, 17 * time.Minute},
		{2, []time.Duration{startup, startup, 2 * startup}, 12 * time.Minute},
		{0, []time.Duration{startup, startup, startup}, 7 * time.Minute},
	} {
		t.Run(strconv.Itoa(tc.parallelism), func(t *testing.T) {
			s := openStore(t)
			m := uptimeManifest("3m", "5m")
			prior := seedUptimeRelease(t, s, m, boundBinding("existing", "id"))
			spec := managedUptimeSpec(prior.ID, "3m0s", `,"Mounts":[`+probeBindJSON+`]`)
			spec = json.RawMessage(strings.Replace(string(spec), `"Replicas":1}}}`, `"Replicas":3}},"UpdateConfig":{"Parallelism":`+strconv.Itoa(tc.parallelism)+`}}`, 1))
			f := &uptimeRuntimeEngine{runtimeEngine: runtimeEngine{Engine: &boundFake{}, store: s, spec: spec}, replicaStarts: tc.ready}
			c := &Controller{Store: s, Engine: f, Environment: "test", ProbeSource: testProbeSource}
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				start := time.Now()
				op, out := finishUptimeOperation(t, ctx, c, f, Request{Kind: "restart", App: "web"})
				if out.State != "succeeded" || !f.deadline.Equal(start.Add(tc.allowance)) {
					t.Fatalf("restart %s (%s), deadline +%s, want succeeded at allowance %s", out.State, out.Err, f.deadline.Sub(start), tc.allowance)
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

func (f *uptimeRuntimeEngine) InspectContainer(context.Context, string) (engine.ContainerSnapshot, error) {
	return engine.ContainerSnapshot{ID: "container", Running: true, Labels: map[string]string{"berthy.operation": f.generation()}}, nil
}

func (f *uptimeRuntimeEngine) Exec(context.Context, string, []string) (engine.ExecResult, error) {
	f.execs++
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
		{"exact", "20s", probeBindJSON, true},
		{"wrong duration", "0s", probeBindJSON, false},
		{"missing", "20s", "", false},
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
				f := &uptimeRuntimeEngine{runtimeEngine: runtimeEngine{Engine: &boundFake{}, store: s, spec: managedUptimeSpec(prior.ID, tc.duration, mounts)}}
				c := &Controller{Store: s, Engine: f, Environment: "test", ProbeSource: testProbeSource}
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
		f := &uptimeRuntimeEngine{runtimeEngine: runtimeEngine{Engine: &boundFake{}, spec: managedUptimeSpec("old", "20s", `,"Mounts":[`+probeBindJSON+`]`)}}
		c := &Controller{Engine: f, Environment: "test", ProbeSource: testProbeSource}
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
			f := &uptimeRuntimeEngine{runtimeEngine: runtimeEngine{Engine: &boundFake{}, store: s, spec: managedUptimeSpec(prior.ID, "3m0s", `,"Mounts":[`+probeBindJSON+`]`)}, startup: 3 * time.Minute}
			c := &Controller{Store: s, Engine: f, Environment: "test", ProbeSource: testProbeSource}
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
