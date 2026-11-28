package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// diagFake is positiveEngine with the tasks, containers and one-off commands of a failing deploy. Its tasks follow the
// service spec that is live, like Swarm's, so evidence read after the service was deleted is gone.
type diagFake struct {
	*positiveEngine
	fmu      sync.Mutex
	calls    []string
	tasksFor func(generation, image string) []engine.ServiceTask
	diagnose map[string]engine.ContainerDiagnosis
	tails    map[string]string
	exits    []int
	versions []string
	waitErr  error
}

func (f *diagFake) record(call string) {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	f.calls = append(f.calls, call)
}
func (f *diagFake) callIndex(call string) int {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	for i, c := range f.calls {
		if c == call {
			return i
		}
	}
	return -1
}
func (f *diagFake) lastIndexWithPrefix(prefix string) int {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	n := -1
	for i, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n = i
		}
	}
	return n
}
func (f *diagFake) ServiceTasks(context.Context, string) ([]engine.ServiceTask, error) {
	f.positiveEngine.mu.Lock()
	spec := append(json.RawMessage(nil), f.positiveEngine.spec...)
	f.positiveEngine.mu.Unlock()
	if spec == nil {
		return nil, nil
	}
	var s struct {
		TaskTemplate struct {
			ContainerSpec struct {
				Image  string
				Labels map[string]string
			}
		}
	}
	_ = json.Unmarshal(spec, &s)
	generation := s.TaskTemplate.ContainerSpec.Labels["berthy.operation"]
	f.record("ServiceTasks")
	if f.tasksFor == nil {
		return []engine.ServiceTask{{ID: "healthy", ServiceID: "id", ContainerID: "healthy-c", State: "running", DesiredState: "running", Slot: 1, Labels: map[string]string{"berthy.operation": generation}, Image: s.TaskTemplate.ContainerSpec.Image}}, nil
	}
	tasks := f.tasksFor(generation, s.TaskTemplate.ContainerSpec.Image)
	for i := range tasks {
		if tasks[i].ServiceID == "" {
			tasks[i].ServiceID = "id"
		}
	}
	return tasks, nil
}
func (f *diagFake) InspectContainerHealth(_ context.Context, id string) (string, error) {
	if d, ok := f.diagnose[id]; ok && d.Health != "" {
		return d.Health, nil
	}
	return "healthy", nil
}
func (f *diagFake) DeleteService(ctx context.Context, id string) error {
	f.record("DeleteService")
	return f.positiveEngine.DeleteService(ctx, id)
}
func (f *diagFake) DiagnoseContainer(_ context.Context, id string) (engine.ContainerDiagnosis, error) {
	f.record("Diagnose:" + id)
	d, ok := f.diagnose[id]
	if !ok {
		return engine.ContainerDiagnosis{}, errors.New("no such container")
	}
	return d, nil
}
func (f *diagFake) LogTail(_ context.Context, id string, lines, maxBytes int) (string, bool, error) {
	f.record("LogTail:" + id)
	if lines != 200 || maxBytes != 64<<10 {
		return "", false, errors.New("unexpected tail bounds")
	}
	text, ok := f.tails[id]
	if !ok {
		return "", false, errors.New("no logs")
	}
	return text, false, nil
}
func (f *diagFake) CreateContainer(_ context.Context, name string, _ json.RawMessage) (string, error) {
	f.record("CreateContainer:" + name)
	return name, nil
}
func (f *diagFake) StartContainer(context.Context, string) error { return nil }
func (f *diagFake) WaitContainer(context.Context, string) (int, error) {
	if f.waitErr != nil {
		return 0, f.waitErr
	}
	f.fmu.Lock()
	defer f.fmu.Unlock()
	n := f.exits[0]
	f.exits = f.exits[1:]
	return n, nil
}
func (f *diagFake) ContainerLogs(context.Context, string) ([]byte, error) {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	v := f.versions[0]
	f.versions = f.versions[1:]
	return []byte(v), nil
}
func (f *diagFake) InspectContainer(context.Context, string) (engine.ContainerSnapshot, error) {
	return engine.ContainerSnapshot{}, &engine.StatusError{Code: 404}
}
func (f *diagFake) StopContainer(_ context.Context, id string, _ int) error {
	f.record("StopContainer:" + id)
	return nil
}

func exitCode(n int) *int { return &n }

// failedTasks returns n failed tasks of the generation, newest last in list order: t1 is the oldest.
func failedTasks(generation string, n int) []engine.ServiceTask {
	var out []engine.ServiceTask
	base := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	for i := 1; i <= n; i++ {
		out = append(out, engine.ServiceTask{ID: fmt.Sprintf("t%d", i), ContainerID: fmt.Sprintf("c%d", i), State: "failed", DesiredState: "shutdown", Slot: uint64(i), Version: uint64(i), Timestamp: base.Add(time.Duration(i) * time.Second), Err: "task: non-zero exit (1)", Message: "started", ExitCode: exitCode(1), Labels: map[string]string{"berthy.operation": generation}})
	}
	// A task of another generation is no evidence of this operation.
	out = append(out, engine.ServiceTask{ID: "other", ContainerID: "c-other", State: "failed", Timestamp: base.Add(time.Hour), Labels: map[string]string{"berthy.operation": "someone-else"}})
	return out
}

func exitedContainers(n int, health string) map[string]engine.ContainerDiagnosis {
	out := map[string]engine.ContainerDiagnosis{}
	for i := 1; i <= n; i++ {
		out[fmt.Sprintf("c%d", i)] = engine.ContainerDiagnosis{State: "exited", ExitCode: 1, Health: health}
	}
	return out
}

func diagController(t *testing.T, f *diagFake) (*Controller, *state.Store) {
	t.Helper()
	s := openStore(t)
	c := &Controller{Engine: f, Store: s, Environment: "test", rolloutHold: new(time.Duration)}
	return c, s
}

func newDiagFake() *diagFake { return &diagFake{positiveEngine: &positiveEngine{}} }

func webManifest(timeout string, hooks func(*manifest.App)) manifest.Manifest {
	m := dockerManifest(nil)
	app := m.Apps["web"]
	app.Deploy.DeployTimeout = timeout
	if hooks != nil {
		hooks(&app)
	}
	m.Apps["web"] = app
	return m
}

func deployWeb(t *testing.T, c *Controller, s *state.Store, request, digest string, m manifest.Manifest) state.Operation {
	t.Helper()
	op, _, err := admit(t, c, context.Background(), Actor{ID: "admin"}, Request{RequestID: request, App: "web", Version: "sha256:" + strings.Repeat(digest, 64), Manifest: m})
	if err != nil {
		t.Fatal(err)
	}
	return waitOperation(t, s, op.ID)
}

func storedDiagnostics(t *testing.T, s *state.Store, id string) []Diagnostic {
	t.Helper()
	raw, err := s.Diagnostics(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []Diagnostic
	for _, r := range raw {
		var d Diagnostic
		if err = json.Unmarshal(r, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func TestFailedCreatedRoleKeepsTaskEvidenceBeforeTheServiceIsDeleted(t *testing.T) {
	f := newDiagFake()
	f.tasksFor = func(generation, _ string) []engine.ServiceTask { return failedTasks(generation, 3) }
	f.diagnose = exitedContainers(3, "starting")
	f.tails = map[string]string{
		"c1": "2026-11-16T10:00:01Z stderr config: DATABASE_URL is required\n",
		"c2": "2026-11-16T10:00:02Z stderr config: DATABASE_URL is required\n",
		"c3": "2026-11-16T10:00:03Z stderr config: DATABASE_URL is required password=hunter2-secret\n",
	}
	c, s := diagController(t, f)
	if err := s.PutSecret(context.Background(), state.Actor{ID: "admin"}, state.SecretInfo{Name: "DB_PASSWORD"}, []byte("hunter2-secret")); err != nil {
		t.Fatal(err)
	}
	op := deployWeb(t, c, s, "first", "a", webManifest(testDeployTimeout, nil))
	want := "role web: task exited with code 1 (gave up after 3 failed starts)"
	if op.State != "failed" || op.Err != "rollout failed and was cleaned up" || op.Reason != want {
		t.Fatalf("operation %s %q reason %q", op.State, op.Err, op.Reason)
	}
	got := storedDiagnostics(t, s, op.ID)
	if len(got) != 3 || got[0].TaskID != "t3" || got[1].TaskID != "t2" || got[2].TaskID != "t1" {
		t.Fatalf("diagnostics are not the three tasks of the operation, newest first: %+v", got)
	}
	d := got[0]
	if d.Kind != "task" || d.Role != "web" || d.Name != "proj-test-web-web" || d.State != "failed" || d.ExitCode == nil || *d.ExitCode != 1 || d.Container != "c3" || d.Captured.IsZero() {
		t.Fatalf("diagnostic %+v", d)
	}
	if !strings.Contains(d.Logs, "DATABASE_URL is required") || strings.Contains(d.Logs, "hunter2-secret") || !strings.Contains(d.Logs, "[redacted]") {
		t.Fatalf("logs were not redacted: %q", d.Logs)
	}
	deleted, lastRead := f.callIndex("DeleteService"), f.lastIndexWithPrefix("LogTail:")
	if deleted < 0 || lastRead < 0 || lastRead > deleted {
		t.Fatalf("evidence was read after the service was deleted: delete at %d, last log read at %d, calls %v", deleted, lastRead, f.calls)
	}
	if strings.Contains(op.Reason, "DATABASE_URL") {
		t.Fatal("log text leaked into the public reason")
	}
	events, err := s.EventsAfter(context.Background(), op.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	var data map[string]string
	if err = json.Unmarshal(last.Data, &data); err != nil || last.Kind != "failed" || data["reason"] != want || data["state"] != "failed" {
		t.Fatalf("final event %s %s", last.Kind, last.Data)
	}
}

func TestEvidenceKeepsFiveNewestTasksPerRole(t *testing.T) {
	f := newDiagFake()
	f.tasksFor = func(generation, _ string) []engine.ServiceTask { return failedTasks(generation, 8) }
	f.diagnose = exitedContainers(8, "starting")
	f.tails = map[string]string{}
	for i := 1; i <= 8; i++ {
		f.tails[fmt.Sprintf("c%d", i)] = fmt.Sprintf("line %d\n", i)
	}
	c, s := diagController(t, f)
	op := deployWeb(t, c, s, "first", "a", webManifest(testDeployTimeout, nil))
	got := storedDiagnostics(t, s, op.ID)
	if len(got) != 5 || got[0].TaskID != "t8" || got[4].TaskID != "t4" {
		t.Fatalf("kept %d diagnostics: %+v", len(got), got)
	}
}

func TestUpdatedRoleThatNeverBecameHealthyRecordsItsHealthLog(t *testing.T) {
	f := newDiagFake()
	f.tasksFor = func(generation, image string) []engine.ServiceTask {
		labels := map[string]string{"berthy.operation": generation}
		if strings.Contains(image, strings.Repeat("b", 64)) {
			return []engine.ServiceTask{{ID: "bad", ContainerID: "c-bad", State: "running", DesiredState: "running", Slot: 1, Labels: labels, Image: image, Timestamp: time.Now()}}
		}
		return []engine.ServiceTask{{ID: "good", ContainerID: "c-good", State: "running", DesiredState: "running", Slot: 1, Labels: labels, Image: image}}
	}
	probe := func(sec int) engine.HealthProbe {
		return engine.HealthProbe{Start: time.Date(2026, 11, 16, 10, 0, sec, 0, time.UTC), ExitCode: 1, Output: "cannot connect to hunter2-secret@db"}
	}
	f.diagnose = map[string]engine.ContainerDiagnosis{"c-good": {State: "running", Running: true, Health: "healthy"}, "c-bad": {State: "running", Health: "starting", HealthLog: []engine.HealthProbe{probe(1), probe(2), probe(3)}}}
	f.tails = map[string]string{"c-bad": "2026-11-16T10:00:01Z stdout waiting for the database\n"}
	c, s := diagController(t, f)
	if err := s.PutSecret(context.Background(), state.Actor{ID: "admin"}, state.SecretInfo{Name: "DB_PASSWORD"}, []byte("hunter2-secret")); err != nil {
		t.Fatal(err)
	}
	if op := deployWeb(t, c, s, "first", "a", webManifest(testDeployTimeout, nil)); op.State != "succeeded" {
		t.Fatalf("first deploy %s %s", op.State, op.Err)
	}
	op := deployWeb(t, c, s, "second", "b", webManifest("300ms", nil))
	if op.State != "rolled_back" || op.Reason != "role web: container never became healthy (last check: exit 1)" {
		t.Fatalf("operation %s %q reason %q", op.State, op.Err, op.Reason)
	}
	got := storedDiagnostics(t, s, op.ID)
	if len(got) != 1 || got[0].TaskID != "bad" || got[0].Health != "starting" || len(got[0].HealthLog) != 3 {
		t.Fatalf("diagnostics %+v", got)
	}
	if strings.Contains(got[0].HealthLog[0].Output, "hunter2-secret") || !strings.Contains(got[0].HealthLog[0].Output, "[redacted]") {
		t.Fatalf("health output was not redacted: %q", got[0].HealthLog[0].Output)
	}
	if strings.Contains(op.Reason, "cannot connect") {
		t.Fatal("health-check output leaked into the public reason")
	}
}

func TestReasonForOutOfMemoryAndForATaskStillStarting(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		tasks        func(string) []engine.ServiceTask
		diagnose     map[string]engine.ContainerDiagnosis
	}{
		{"oom", "role web: task was killed: out of memory", func(g string) []engine.ServiceTask { return failedTasks(g, 1)[:1] }, map[string]engine.ContainerDiagnosis{"c1": {State: "exited", ExitCode: 137, OOMKilled: true}}},
		{"starting", "role web: deploy deadline exceeded before the task was healthy", func(g string) []engine.ServiceTask {
			return []engine.ServiceTask{{ID: "s1", ContainerID: "cs", State: "running", DesiredState: "running", Slot: 1, Labels: map[string]string{"berthy.operation": g}}}
		}, map[string]engine.ContainerDiagnosis{"cs": {State: "running", Health: "starting"}}},
		{"rejected", "role web: task failed: No such image: example/web", func(g string) []engine.ServiceTask {
			return []engine.ServiceTask{{ID: "r1", State: "rejected", DesiredState: "shutdown", Slot: 1, Err: "No such image: example/web", Labels: map[string]string{"berthy.operation": g}}}
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDiagFake()
			f.tasksFor = func(generation, _ string) []engine.ServiceTask { return tc.tasks(generation) }
			f.diagnose = tc.diagnose
			f.tails = map[string]string{"c1": "Killed\n", "cs": "booting\n"}
			c, s := diagController(t, f)
			op := deployWeb(t, c, s, "first", "a", webManifest("200ms", nil))
			if op.State != "failed" || op.Reason != tc.reason {
				t.Fatalf("operation %s %q reason %q", op.State, op.Err, op.Reason)
			}
			if len(storedDiagnostics(t, s, op.ID)) != 1 {
				t.Fatal("evidence not stored")
			}
		})
	}
}

func TestUnreadableEvidenceIsNotedNotFatal(t *testing.T) {
	f := newDiagFake()
	f.tasksFor = func(generation, _ string) []engine.ServiceTask { return failedTasks(generation, 1)[:1] }
	f.diagnose = exitedContainers(1, "starting") // no log tail for c1
	c, s := diagController(t, f)
	op := deployWeb(t, c, s, "first", "a", webManifest("200ms", nil))
	got := storedDiagnostics(t, s, op.ID)
	if len(got) != 1 || got[0].Logs != "" || !strings.Contains(got[0].Error, "logs unavailable") || got[0].ExitCode == nil {
		t.Fatalf("diagnostics %+v", got)
	}
	if op.Reason != "role web: task exited with code 1" {
		t.Fatalf("reason %q", op.Reason)
	}
}

func TestDiagnosticsTotalStaysWithinOneMiB(t *testing.T) {
	var d []Diagnostic
	for i := 0; i < 20; i++ {
		d = append(d, Diagnostic{Kind: "task", TaskID: fmt.Sprintf("t%d", i), Logs: strings.Repeat("x", 64<<10)})
	}
	d = capDiagnostics(d)
	total := 0
	for i, item := range d {
		b, _ := json.Marshal(item)
		total += len(b)
		if (item.Logs == "") != item.Truncated {
			t.Fatalf("diagnostic %d: logs %d bytes, truncated %v", i, len(item.Logs), item.Truncated)
		}
	}
	if total > diagnosticTotalBytes || d[0].Logs == "" || d[19].Logs != "" || !d[19].Truncated {
		t.Fatalf("total %d, newest keeps logs %v, oldest dropped %v", total, d[0].Logs != "", d[19].Logs == "")
	}
}

func TestFailureReasonIsPublicAndBounded(t *testing.T) {
	two := 2
	for _, tc := range []struct {
		name     string
		d        []Diagnostic
		fallback string
		want     string
	}{
		{"fallback", nil, "rollout failed", "rollout failed"},
		{"two roles", []Diagnostic{
			{Kind: "task", Role: "web", State: "failed", ExitCode: &two},
			{Kind: "task", Role: "worker", State: "failed", OOMKilled: true},
		}, "x", "role web: task exited with code 2; role worker: task was killed: out of memory"},
		{"unhealthy exit wins over the kill exit code", []Diagnostic{{Kind: "task", Role: "web", State: "failed", ExitCode: exitCode(137), Health: "unhealthy", HealthLog: []engine.HealthProbe{{ExitCode: 7, Output: "SECRET"}}}}, "x", "role web: container never became healthy (last check: exit 7)"},
		{"a crash before any check ran is an exit, though Docker calls it unhealthy", []Diagnostic{{Kind: "task", Role: "web", State: "failed", ExitCode: exitCode(1), Health: "unhealthy"}}, "x", "role web: task exited with code 1"},
		{"checks that passed before a crash are not a failed start", []Diagnostic{{Kind: "task", Role: "web", State: "failed", ExitCode: exitCode(1), Health: "healthy", HealthLog: []engine.HealthProbe{{ExitCode: 0}}}}, "x", "role web: task exited with code 1"},
		{"swarm error is sanitized", []Diagnostic{{Kind: "task", Role: "web", State: "rejected", Error: "bad\x00image é \n" + strings.Repeat("e", 300)}}, "x", ""},
		{"pre hook", []Diagnostic{{Kind: "oneoff", Name: "pre_hook", Index: 2, ExitCode: exitCode(3), Logs: "SECRET"}}, "x", "pre-deploy command 2 exited with code 3"},
		{"post hook", []Diagnostic{{Kind: "oneoff", Name: "post_hook", Index: 1, ExitCode: exitCode(1)}}, "x", "post-deploy command 1 exited with code 1"},
		{"migration", []Diagnostic{{Kind: "oneoff", Name: "migration_apply", ExitCode: exitCode(2)}}, "x", "migration apply exited with code 2"},
		{"timeout", []Diagnostic{{Kind: "oneoff", Name: "pre_hook", Index: 1, Error: errTimedOut + " after 5m0s"}}, "x", "pre-deploy command 1 timed out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := failureReason(tc.d, tc.fallback)
			if tc.want != "" && got != tc.want {
				t.Fatalf("reason %q, want %q", got, tc.want)
			}
			if len(got) > maxReasonBytes || strings.Contains(got, "SECRET") || strings.IndexFunc(got, func(r rune) bool { return r < 0x20 || r > 0x7e }) >= 0 {
				t.Fatalf("reason is not public: %q", got)
			}
		})
	}
}

func TestFailedPreDeployCommandStoresItsOutputAndReason(t *testing.T) {
	f := newDiagFake()
	f.exits = []int{3}
	f.diagnose = map[string]engine.ContainerDiagnosis{}
	c, s := diagController(t, f)
	op := deployWeb(t, c, s, "first", "a", webManifest(testDeployTimeout, func(a *manifest.App) {
		a.Hooks.PreDeploy = []manifest.Hook{{Run: []string{"/bin/migrate"}}}
	}))
	f.fmu.Lock()
	name := ""
	for _, call := range f.calls {
		if strings.HasPrefix(call, "CreateContainer:") {
			name = strings.TrimPrefix(call, "CreateContainer:")
		}
	}
	f.fmu.Unlock()
	if name == "" {
		t.Fatal("no one-off ran")
	}
	// The container is only known after it ran; a second deploy attempt reuses the fake with output in place.
	if op.State != "failed" || op.Err != "pre-deploy command failed" || op.Reason != "pre-deploy command 1 exited with code 3" {
		t.Fatalf("operation %s %q reason %q", op.State, op.Err, op.Reason)
	}
	if got := storedDiagnostics(t, s, op.ID); len(got) != 1 || got[0].Kind != "oneoff" || got[0].Name != "pre_hook" || got[0].Index != 1 || got[0].Container != name {
		t.Fatalf("diagnostics %+v", got)
	}
}

func TestFailedHookDiagnosticCarriesExitCodeAndOutput(t *testing.T) {
	f := newDiagFake()
	f.exits = []int{3}
	c, s := diagController(t, f)
	op, _, err := s.Admit(context.Background(), state.Admission{RequestID: "r", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	p := preparedPlan{Image: "registry/app@sha256:pinned", NetworkID: "overlay", Request: Request{App: "web", Manifest: webManifest(testDeployTimeout, nil)}}
	name := fmt.Sprintf("berthy-pre_hook-.test-%s-10000", op.ID)
	f.diagnose = map[string]engine.ContainerDiagnosis{name: {State: "exited", ExitCode: 3}}
	f.tails = map[string]string{name: "2026-11-16T10:00:01Z stderr migration 0042 failed: column exists\n"}
	_, err = c.runOneOff(context.Background(), op.ID, p, oneOff{Ordinal: 10000, Kind: "pre_hook", Argv: []string{"migrate"}, Timeout: time.Minute})
	d := failureDiagnostics(err)
	if err == nil || len(d) != 1 || d[0].ExitCode == nil || *d[0].ExitCode != 3 || !strings.Contains(d[0].Logs, "migration 0042 failed") || d[0].Index != 1 {
		t.Fatalf("error %v diagnostics %+v", err, d)
	}
	if d[0].State != "exited" {
		t.Fatalf("a one-off has no task state, so it shows its container state: %q", d[0].State)
	}
	if err.Error() != "pre_hook failed with exit code 3" {
		t.Fatalf("error text changed: %v", err)
	}
}

func TestFailedMigrationApplyStoresOutputAndNamesTheMigration(t *testing.T) {
	f := newDiagFake()
	f.exits = []int{0, 2, 0}
	f.versions = []string{"v41\n", "v41\n"}
	c, s := diagController(t, f)
	op := deployWeb(t, c, s, "first", "a", webManifest(testDeployTimeout, func(a *manifest.App) {
		a.Migrations = &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}}
	}))
	if op.State != "failed" || op.Err != "pre-deploy command failed" || op.Reason != "migration apply exited with code 2" {
		t.Fatalf("operation %s %q reason %q", op.State, op.Err, op.Reason)
	}
	if op.SchemaBefore != "v41" {
		t.Fatalf("schema states changed: %q %q", op.SchemaBefore, op.SchemaAfter)
	}
	got := storedDiagnostics(t, s, op.ID)
	if len(got) != 1 || got[0].Name != "migration_apply" || got[0].ExitCode == nil || *got[0].ExitCode != 2 {
		t.Fatalf("diagnostics %+v", got)
	}
}

func TestTimedOutOneOffKeepsItsOutputAndIsNotStopped(t *testing.T) {
	f := newDiagFake()
	f.waitErr = context.DeadlineExceeded
	c, s := diagController(t, f)
	op, _, err := s.Admit(context.Background(), state.Admission{RequestID: "r", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	p := preparedPlan{Image: "registry/app@sha256:pinned", NetworkID: "overlay", Request: Request{App: "web", Manifest: webManifest(testDeployTimeout, nil)}}
	name := fmt.Sprintf("berthy-migration_apply-.test-%s-10001", op.ID)
	f.diagnose = map[string]engine.ContainerDiagnosis{name: {State: "running"}}
	f.tails = map[string]string{name: "2026-11-16T10:00:01Z stdout still migrating\n"}
	_, err = c.runOneOff(context.Background(), op.ID, p, oneOff{Ordinal: 10001, Kind: "migration_apply", Argv: []string{"apply"}, Timeout: 5 * time.Minute})
	d := failureDiagnostics(err)
	if !errors.Is(err, ErrOneOffUncertain) || len(d) != 1 || !strings.HasPrefix(d[0].Error, errTimedOut) || !strings.Contains(d[0].Logs, "still migrating") || d[0].ExitCode != nil {
		t.Fatalf("error %v diagnostics %+v", err, d)
	}
	if failureReason(d, "x") != "migration apply timed out" {
		t.Fatalf("reason %q", failureReason(d, "x"))
	}
	if f.callIndex("StopContainer:"+name) >= 0 {
		t.Fatal("capture stopped the container; recovery owns that")
	}
}

func TestRecoveryCapturesEvidenceBeforeCompensating(t *testing.T) {
	f := newDiagFake()
	f.tasksFor = func(generation, _ string) []engine.ServiceTask { return failedTasks(generation, 2) }
	f.diagnose = exitedContainers(2, "starting")
	f.tails = map[string]string{"c1": "boom one\n", "c2": "boom two\n"}
	c, s := diagController(t, f)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "r", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	spec := json.RawMessage(fmt.Sprintf(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Image":"example/web","Labels":{"berthy.operation":%q}}}}`, op.ID))
	if err = s.RecordIntent(ctx, op.ID, state.Intent{Kind: "create", Role: "web", Name: "svc", Generation: op.ID, Desired: spec}); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkStep(ctx, op.ID, 0, "observed", json.RawMessage(fmt.Sprintf(`{"id":"id","spec":%s}`, spec))); err != nil {
		t.Fatal(err)
	}
	f.positiveEngine.spec = spec
	if err = c.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.Operation(ctx, op.ID)
	if err != nil || got.State != "interrupted" || got.Reason != "role web: task exited with code 1 (2 failed tasks)" {
		t.Fatalf("operation %s reason %q: %v", got.State, got.Reason, err)
	}
	if d := storedDiagnostics(t, s, op.ID); len(d) != 2 {
		t.Fatalf("diagnostics %+v", d)
	}
	if f.lastIndexWithPrefix("LogTail:") > f.callIndex("DeleteService") {
		t.Fatalf("recovery read evidence after deleting: %v", f.calls)
	}
}

func TestRecoveryKeepsDiagnosticsAnOperationAlreadyHas(t *testing.T) {
	f := newDiagFake()
	f.tasksFor = func(generation, _ string) []engine.ServiceTask { return failedTasks(generation, 2) }
	f.diagnose = exitedContainers(2, "starting")
	f.tails = map[string]string{"c1": "x\n", "c2": "y\n"}
	c, s := diagController(t, f)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "r", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	spec := json.RawMessage(fmt.Sprintf(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Image":"example/web","Labels":{"berthy.operation":%q}}}}`, op.ID))
	if err = s.RecordIntent(ctx, op.ID, state.Intent{Kind: "create", Role: "web", Name: "svc", Generation: op.ID, Desired: spec}); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkStep(ctx, op.ID, 0, "observed", json.RawMessage(fmt.Sprintf(`{"id":"id","spec":%s}`, spec))); err != nil {
		t.Fatal(err)
	}
	if err = s.PutDiagnostics(ctx, op.ID, []json.RawMessage{json.RawMessage(`{"kind":"oneoff","name":"pre_hook","captured":"2026-11-16T10:00:00Z"}`)}); err != nil {
		t.Fatal(err)
	}
	f.positiveEngine.spec = spec
	if err = c.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if d := storedDiagnostics(t, s, op.ID); len(d) != 1 || d[0].Kind != "oneoff" {
		t.Fatalf("diagnostics %+v", d)
	}
}

func TestPruneIncludesFinishedMigrationAndTimedOutOneOffs(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "p", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	for i, step := range []struct{ role, phase, receipt string }{
		{"schema_before", "finished", `{"id":"c-before","exit":0,"version":"v1"}`},
		{"migration_apply", "finished", `{"id":"c-apply","exit":2}`},
		{"pre_hook", "started", `{"id":"c-slow"}`},
	} {
		if err = s.RecordIntent(ctx, op.ID, state.Intent{Ordinal: 10000 + i, Kind: "oneoff", Role: step.role, Name: "task", Desired: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
		if err = s.MarkStep(ctx, op.ID, 10000+i, step.phase, json.RawMessage(step.receipt)); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "failed"}); err != nil {
		t.Fatal(err)
	}
	f := &perContainerPrune{op: op.ID, running: map[string]bool{"c-slow": true}, roles: map[string]string{"c-before": "schema_before", "c-apply": "migration_apply", "c-slow": "pre_hook"}}
	c := Controller{Store: s, Engine: f, Environment: "stage"}
	hooks, err := c.prunableHooks(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, h := range hooks {
		ids = append(ids, h.ID)
	}
	if strings.Join(ids, ",") != "c-before,c-apply" {
		t.Fatalf("prunable %v: finished migration containers qualify, a running one does not", ids)
	}
}

type perContainerPrune struct {
	Engine
	op      string
	running map[string]bool
	roles   map[string]string
}

func (p *perContainerPrune) InspectContainer(_ context.Context, id string) (engine.ContainerSnapshot, error) {
	return engine.ContainerSnapshot{ID: id, Running: p.running[id], Labels: map[string]string{"berthy.environment": "stage", "berthy.operation": p.op, "berthy.oneoff": p.roles[id]}}, nil
}
func (p *perContainerPrune) RemoveContainer(context.Context, string) error { return nil }

func withStartAttempts(n int) func(*manifest.App) {
	return func(a *manifest.App) { a.Deploy.StartAttempts = &n }
}

func TestCreatedRoleGivesUpAfterItsStartAttempts(t *testing.T) {
	f := newDiagFake()
	f.tasksFor = func(generation, _ string) []engine.ServiceTask { return failedTasks(generation, 2) }
	f.diagnose = exitedContainers(2, "starting")
	f.tails = map[string]string{"c1": "boom\n", "c2": "boom\n"}
	c, s := diagController(t, f)
	start := time.Now()
	// A long deadline: the deploy must end because the attempts are used up, not because the time is.
	op := deployWeb(t, c, s, "first", "a", webManifest("1m", withStartAttempts(2)))
	if time.Since(start) > 20*time.Second {
		t.Fatalf("took %v: waited for the deploy timeout", time.Since(start))
	}
	if op.State != "failed" || op.Err != "rollout failed and was cleaned up" || op.Reason != "role web: task exited with code 1 (gave up after 2 failed starts)" {
		t.Fatalf("operation %s %q reason %q", op.State, op.Err, op.Reason)
	}
	if f.callIndex("DeleteService") < 0 {
		t.Fatal("the service was not removed")
	}
	if got := storedDiagnostics(t, s, op.ID); len(got) != 2 || got[0].GaveUpAfter != 2 {
		t.Fatalf("diagnostics %+v", got)
	}
}

func TestUpdatedRoleGivesUpAfterItsStartAttemptsAndKeepsThePreviousVersion(t *testing.T) {
	f := newDiagFake()
	f.tasksFor = func(generation, image string) []engine.ServiceTask {
		if strings.Contains(image, strings.Repeat("b", 64)) {
			return failedTasks(generation, 2)
		}
		return []engine.ServiceTask{{ID: "good", ContainerID: "c-good", State: "running", DesiredState: "running", Slot: 1, Labels: map[string]string{"berthy.operation": generation}, Image: image}}
	}
	f.diagnose = exitedContainers(2, "starting")
	f.tails = map[string]string{"c1": "boom\n", "c2": "boom\n"}
	c, s := diagController(t, f)
	if op := deployWeb(t, c, s, "first", "a", webManifest(testDeployTimeout, nil)); op.State != "succeeded" {
		t.Fatalf("first deploy %s %s", op.State, op.Err)
	}
	start := time.Now()
	op := deployWeb(t, c, s, "second", "b", webManifest("1m", withStartAttempts(2)))
	if time.Since(start) > 20*time.Second {
		t.Fatalf("took %v: waited for the deploy timeout", time.Since(start))
	}
	if op.State != "rolled_back" || op.Reason != "role web: task exited with code 1 (gave up after 2 failed starts)" {
		t.Fatalf("operation %s %q reason %q", op.State, op.Err, op.Reason)
	}
	f.positiveEngine.mu.Lock()
	live := string(f.positiveEngine.spec)
	f.positiveEngine.mu.Unlock()
	if !strings.Contains(live, strings.Repeat("a", 64)) || strings.Contains(live, strings.Repeat("b", 64)) {
		t.Fatalf("the previous version was not restored: %s", live)
	}
}

func TestRoleThatStartsOnItsLastAttemptSucceeds(t *testing.T) {
	for _, tc := range []struct {
		attempts int
		state    string
	}{{3, "succeeded"}, {2, "failed"}} {
		t.Run(fmt.Sprint(tc.attempts), func(t *testing.T) {
			f := newDiagFake()
			var polls atomic.Int32
			f.tasksFor = func(generation, image string) []engine.ServiceTask {
				// The role fails twice, then runs: Swarm replaced the failed task, and the third one is healthy.
				if polls.Add(1) <= 2 {
					return failedTasks(generation, int(polls.Load()))
				}
				tasks := failedTasks(generation, 2)
				return append(tasks, engine.ServiceTask{ID: "good", ContainerID: "c-good", State: "running", DesiredState: "running", Slot: 1, Labels: map[string]string{"berthy.operation": generation}, Image: image})
			}
			f.diagnose = exitedContainers(2, "starting")
			c, s := diagController(t, f)
			op := deployWeb(t, c, s, "first", "a", webManifest("1m", withStartAttempts(tc.attempts)))
			if op.State != tc.state {
				t.Fatalf("operation %s %q reason %q", op.State, op.Err, op.Reason)
			}
		})
	}
}

func TestRecoveryRestoresTheSpecOfAnUpdateThatBerthyWasWatching(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	prior, _, err := s.Admit(ctx, state.Admission{RequestID: "prior", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, prior.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "web", Digest: "docker.io/example/web@sha256:" + strings.Repeat("a", 64), Manifest: testReleaseManifest(t)}}); err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "update", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	before := withDockerDefaults(json.RawMessage(`{"Name":"svc","Labels":{"berthy.operation":"old"},"TaskTemplate":{"ContainerSpec":{"Image":"docker.io/example/web@sha256:` + strings.Repeat("a", 64) + `","Labels":{"berthy.operation":"old"}}},"Mode":{"Replicated":{"Replicas":1}},"UpdateConfig":{"FailureAction":"rollback","Order":"start-first"}}`))
	desired := json.RawMessage(`{"Name":"svc","Labels":{"berthy.operation":"new"},"TaskTemplate":{"ContainerSpec":{"Image":"docker.io/example/web@sha256:` + strings.Repeat("b", 64) + `","Labels":{"berthy.operation":"new"}},"RestartPolicy":{"Condition":"any","Delay":5000000000}},"Mode":{"Replicated":{"Replicas":1}},"UpdateConfig":{"FailureAction":"continue","Order":"start-first"}}`)
	if err = s.RecordIntent(ctx, op.ID, state.Intent{Kind: "update", Role: "web", Name: "svc", ServiceID: "id", Generation: "new", Before: before, Desired: desired}); err != nil {
		t.Fatal(err)
	}
	// The agent stopped while the update ran and Berthy was still waiting for the new version: Swarm keeps replacing its
	// failing tasks, and the service carries the journaled target.
	e := dockerSpecs{&positiveEngine{spec: withDockerDefaults(storedAsDocker(desired))}}
	c := Controller{Engine: e, Store: s}
	if err = c.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.Operation(ctx, op.ID)
	if err != nil || got.State != "interrupted" {
		t.Fatalf("operation %+v: %v", got, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !sameSpec(e.spec, before) {
		t.Fatalf("service not restored: %s", e.spec)
	}
}

func TestStartAttemptWordingOfTheReason(t *testing.T) {
	if got := rolloutFallback(&errStartAttempts{Role: "web", Failed: 3, Limit: 3}); got != "role web: gave up after 3 failed starts" {
		t.Fatalf("fallback %q", got)
	}
	if got := rolloutFallback(fmt.Errorf("wrapped: %w", &errStartAttempts{Role: "web", Limit: 2})); got != "role web: gave up after 2 failed starts" {
		t.Fatalf("wrapped fallback %q", got)
	}
	tasks := []Diagnostic{{Kind: "task", Role: "web", State: "failed", ExitCode: exitCode(1), GaveUpAfter: 3}, {Kind: "task", Role: "web", State: "failed", ExitCode: exitCode(1), GaveUpAfter: 3}}
	if got := failureReason(tasks, "x"); got != "role web: task exited with code 1 (gave up after 3 failed starts)" {
		t.Fatalf("reason %q", got)
	}
	retried := []Diagnostic{{Kind: "oneoff", Name: "migration_apply", ExitCode: exitCode(2), Attempts: 3}}
	if got := failureReason(retried, "x"); got != "migration apply exited with code 2 (after 3 attempts)" {
		t.Fatalf("reason %q", got)
	}
}

func TestFinalRolloutFallbackIsPublic(t *testing.T) {
	for _, reason := range []string{
		"final verification unavailable",
		"final health verification failed",
		"final drain verification failed",
		"service update did not complete",
	} {
		t.Run(reason, func(t *testing.T) {
			cause := finalRolloutFailure(reason)
			for _, err := range []error{cause, fmt.Errorf("wrapped: %w", cause)} {
				if got := rolloutFallback(err); got != reason {
					t.Errorf("fallback for %v = %q, want %q", err, got, reason)
				}
			}
		})
	}
	if got := rolloutFallback(errors.New("service health deadline exceeded")); got != "deploy deadline exceeded before the task was healthy" {
		t.Fatalf("startup deadline fallback = %q", got)
	}
}

type failingRestoreEngine struct{ *diagFake }

func (f failingRestoreEngine) UpdateService(ctx context.Context, id string, version uint64, spec json.RawMessage, auth *engine.RegistryAuth) error {
	if strings.Contains(string(spec), strings.Repeat("a", 64)) {
		return errors.New("private docker error")
	}
	return f.diagFake.UpdateService(ctx, id, version, spec, auth)
}

func TestFailedCompensationRecordsCategoryAndKeepsLock(t *testing.T) {
	f := newDiagFake()
	f.tasksFor = func(generation, image string) []engine.ServiceTask {
		if strings.Contains(image, strings.Repeat("b", 64)) {
			return failedTasks(generation, 2)
		}
		return []engine.ServiceTask{{ID: "good", ContainerID: "c-good", State: "running", DesiredState: "running", Slot: 1, Labels: map[string]string{"berthy.operation": generation}, Image: image}}
	}
	f.diagnose = exitedContainers(2, "starting")
	c, s := diagController(t, f)
	if op := deployWeb(t, c, s, "first", "a", webManifest(testDeployTimeout, nil)); op.State != "succeeded" {
		t.Fatalf("first: %+v", op)
	}
	c.Engine = failingRestoreEngine{f}
	var logMu sync.Mutex
	var logs []string
	c.Log = func(message string) { logMu.Lock(); defer logMu.Unlock(); logs = append(logs, message) }
	op := deployWeb(t, c, s, "second", "b", webManifest("1m", withStartAttempts(2)))
	if op.State != "recovery_required" || op.Reason != "role web: task exited with code 1 (gave up after 2 failed starts)" {
		t.Fatalf("operation: %+v", op)
	}
	lock, err := s.Lock(context.Background())
	if err != nil || lock.OperationID != op.ID {
		t.Fatalf("lock: %+v, %v", lock, err)
	}
	const want = "compensation: restore update failed"
	logMu.Lock()
	logged := strings.Join(logs, "\n")
	logMu.Unlock()
	diagnostics := storedDiagnostics(t, s, op.ID)
	public, err := json.Marshal(struct {
		Operation   state.Operation
		Diagnostics []Diagnostic
	}{op, diagnostics})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logged, "private docker error") || strings.Contains(string(public), "private docker error") {
		t.Fatalf("private Docker error leaked: log %q, public %s", logged, public)
	}
	if !strings.Contains(logged, want) {
		t.Fatalf("missing compensation category in log: %q", logged)
	}
	found := false
	for _, d := range diagnostics {
		if d.Error == want {
			found = true
		}
	}
	if !found || !PublicMessage(want) {
		t.Fatalf("missing public compensation category in diagnostics: %+v", storedDiagnostics(t, s, op.ID))
	}
}

func TestCompensationReportsPreviousHealthDeadline(t *testing.T) {
	before := json.RawMessage(`{"Name":"svc","Mode":{"Replicated":{"Replicas":1}},"TaskTemplate":{"ContainerSpec":{"Labels":{"berthy.operation":"old"}}}}`)
	s := openStore(t)
	seedUptimeRelease(t, s, dockerManifest(nil))
	c := Controller{Engine: &positiveEngine{spec: before, unhealthy: true}, Store: s}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		err := c.restoreUpdate(ctx, "web", state.Intent{Role: "web", Name: "svc", ServiceID: "id", Before: before})
		if got := compensationMessages(err); len(got) != 1 || got[0] != "compensation: previous release did not become healthy before the deadline" {
			t.Fatalf("category %v, error %v", got, err)
		}
	})
}

func TestCompensationReportsReplacedService(t *testing.T) {
	live := json.RawMessage(`{"Name":"svc","version":"replacement"}`)
	e := &positiveEngine{spec: live}
	c := Controller{Engine: e}
	err := c.restoreUpdate(context.Background(), "web", state.Intent{Name: "svc", ServiceID: "old-id", Before: json.RawMessage(`{"Name":"svc","version":"old"}`)})
	if got := compensationMessages(err); len(got) != 1 || got[0] != "compensation: service changed outside Berthy" {
		t.Fatalf("category %v, error %v", got, err)
	}
	if string(e.spec) != string(live) {
		t.Fatalf("replaced service was modified: %s", e.spec)
	}
}

func TestRolloutFailureOmitsReadyUptimeSibling(t *testing.T) {
	f := newDiagFake()
	c, s := diagController(t, f)
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		op, _, err := s.Admit(ctx, state.Admission{RequestID: "uptime-failure", ActorID: "admin", Kind: "deploy", App: "web", PublicRequest: json.RawMessage("{}")})
		if err != nil {
			t.Fatal(err)
		}
		f.spec, err = json.Marshal(map[string]any{"TaskTemplate": map[string]any{"ContainerSpec": map[string]any{"Labels": map[string]string{"berthy.operation": op.ID}}}})
		if err != nil {
			t.Fatal(err)
		}
		const threshold = 20 * time.Second
		ready := runningTask("ready", op.ID)
		ready.RunningSince = time.Now().Add(-threshold)
		failed := failedTasks(op.ID, 1)[0]
		failed.ServiceID = "id"
		f.tasksFor = func(_, _ string) []engine.ServiceTask { return []engine.ServiceTask{ready, failed} }
		f.diagnose = exitedContainers(1, "none")
		f.diagnose[ready.ContainerID] = engine.ContainerDiagnosis{State: "running", Health: "none"}
		f.tails = map[string]string{ready.ContainerID: "worker running", failed.ContainerID: "worker exited"}
		work := []roleWork{
			{intent: state.Intent{Role: "ready", Name: "ready", ServiceID: ready.ServiceID, Uptime: threshold}},
			{intent: state.Intent{Role: "failed", Name: "failed", ServiceID: failed.ServiceID, Uptime: threshold}},
		}
		reason := c.recordRolloutFailure(ctx, op.ID, work, "rollout failed", nil)
		evidence := storedDiagnostics(t, s, op.ID)
		if reason != "role failed: task exited with code 1" || len(evidence) != 1 || evidence[0].Role != "failed" ||
			f.callIndex("Diagnose:"+ready.ContainerID) != -1 || f.callIndex("LogTail:"+ready.ContainerID) != -1 {
			t.Fatalf("reason %q, evidence %+v, calls %v", reason, evidence, f.calls)
		}
	})
}

type interruptedDiagnosticsEngine struct {
	*boundFake
	diagnostics *diagFake
	tasks       map[string][]engine.ServiceTask
}

func (f *interruptedDiagnosticsEngine) ServiceTasks(_ context.Context, id string) ([]engine.ServiceTask, error) {
	return f.tasks[id], nil
}
func (f *interruptedDiagnosticsEngine) DiagnoseContainer(ctx context.Context, id string) (engine.ContainerDiagnosis, error) {
	return f.diagnostics.DiagnoseContainer(ctx, id)
}
func (f *interruptedDiagnosticsEngine) LogTail(ctx context.Context, id string, lines, maxBytes int) (string, bool, error) {
	return f.diagnostics.LogTail(ctx, id, lines, maxBytes)
}

func TestInterruptedRolloutOmitsReadyUptimeSibling(t *testing.T) {
	s := openStore(t)
	docker := &interruptedDiagnosticsEngine{boundFake: &boundFake{}, diagnostics: newDiagFake()}
	c := Controller{Engine: docker, Store: s, Environment: "test"}
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		op, _, err := s.Admit(ctx, state.Admission{RequestID: "interrupted", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage("{}")})
		if err != nil {
			t.Fatal(err)
		}
		m := uptimeManifest("20s", "1m")
		m.Apps["web"].Roles["worker"] = m.Apps["web"].Roles["web"]
		req := Request{App: "web", Manifest: m}
		d := deployRun{opID: op.ID, prepared: preparedPlan{Request: req}}
		for ordinal, role := range []string{"worker", "web"} {
			if !c.planRoleWork(ctx, &d, engine.Image{}, nil, ordinal, role) {
				t.Fatalf("plan %s: %s", role, d.outcome.Err)
			}
			intent := d.work[ordinal].intent
			if err := s.RecordIntent(ctx, op.ID, intent); err != nil {
				t.Fatal(err)
			}
			docker.add(role, intent.Desired)
			receipt := json.RawMessage(fmt.Sprintf(`{"id":%q,"spec":%s}`, role, intent.Desired))
			phase := "observed"
			if role == "worker" {
				phase = "verified"
			}
			if err := s.MarkStep(ctx, op.ID, ordinal, phase, receipt); err != nil {
				t.Fatal(err)
			}
		}
		worker := runningTask("worker", op.ID)
		worker.RunningSince = time.Now().Add(-20 * time.Second)
		failed := failedTasks(op.ID, 1)[0]
		docker.tasks = map[string][]engine.ServiceTask{
			"worker": onService("worker", worker),
			"web":    onService("web", failed),
		}
		logs := docker.diagnostics
		logs.diagnose = exitedContainers(1, "none")
		logs.diagnose[worker.ContainerID] = engine.ContainerDiagnosis{State: "running", Health: "none"}
		logs.tails = map[string]string{worker.ContainerID: "worker running", failed.ContainerID: "failed sibling output"}

		if err := c.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		got, err := s.Operation(ctx, op.ID)
		evidence := storedDiagnostics(t, s, op.ID)
		if err != nil || got.State != "interrupted" || got.Reason != "role web: task exited with code 1" ||
			len(evidence) != 1 || evidence[0].Role != "web" || evidence[0].Logs != "failed sibling output" ||
			logs.callIndex("LogTail:"+failed.ContainerID) < 0 || logs.callIndex("LogTail:"+worker.ContainerID) >= 0 {
			t.Fatalf("recovery %s reason %q (%v), evidence %+v, calls %v", got.State, got.Reason, err, evidence, logs.calls)
		}
	})
}

func TestRolloutDiagnosticsKeepExitedContainerWithRetainedHealth(t *testing.T) {
	f := newDiagFake()
	c, s := diagController(t, f)
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		op, _, err := s.Admit(ctx, state.Admission{RequestID: "retained-health", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage("{}")})
		if err != nil {
			t.Fatal(err)
		}
		f.spec = json.RawMessage(fmt.Sprintf(`{"TaskTemplate":{"ContainerSpec":{"Labels":{"berthy.operation":%q}}}}`, op.ID))
		task := runningTask("web", op.ID)
		f.tasksFor = func(_, _ string) []engine.ServiceTask { return onService("id", task) }
		f.diagnose = map[string]engine.ContainerDiagnosis{task.ContainerID: {State: "exited", Health: "healthy", ExitCode: 1}}
		f.tails = map[string]string{task.ContainerID: "exited after passing its check"}

		work := []roleWork{{intent: state.Intent{Role: "web", Name: "web", ServiceID: "id"}}}
		reason := c.recordRolloutFailure(ctx, op.ID, work, "rollout failed", nil)
		evidence := storedDiagnostics(t, s, op.ID)
		if reason != "role web: task exited with code 1" || len(evidence) != 1 || evidence[0].Logs != f.tails[task.ContainerID] ||
			evidence[0].ExitCode == nil || *evidence[0].ExitCode != 1 {
			t.Fatalf("reason %q, evidence %+v, calls %v", reason, evidence, f.calls)
		}
	})
}

func TestRolloutFailureKeepsHistoryWithoutBlamingRecoveredRole(t *testing.T) {
	for _, tc := range []struct {
		name string
		slot uint64
		want string
	}{
		{"full coverage", 7, "role web: task exited with code 1"},
		{"only one distinct slot", 2, "role worker: task exited with code 1; role web: task exited with code 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDiagFake()
			c, s := diagController(t, f)
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				op, _, err := s.Admit(ctx, state.Admission{RequestID: "recovered", ActorID: "admin", Kind: "deploy", App: "web", PublicRequest: json.RawMessage("{}")})
				if err != nil {
					t.Fatal(err)
				}
				f.spec = json.RawMessage(fmt.Sprintf(`{"TaskTemplate":{"ContainerSpec":{"Labels":{"berthy.operation":%q}}}}`, op.ID))
				const threshold = 20 * time.Second
				first, second := runningTask("replacement", op.ID), runningTask("second", op.ID)
				first.Slot, second.Slot = 2, tc.slot
				first.RunningSince = time.Now().Add(-threshold)
				second.RunningSince = first.RunningSince
				exits := failedTasks(op.ID, 2)
				exits[0].Slot = first.Slot
				f.tasksFor = func(_, _ string) []engine.ServiceTask {
					return append(onService("worker", first, second, exits[0]), onService("web", exits[1])...)
				}
				f.diagnose = exitedContainers(2, "none")
				f.tails = map[string]string{"c1": "worker's earlier exit", "c2": "failing sibling output"}
				work := []roleWork{
					{intent: state.Intent{Role: "worker", Name: "worker", ServiceID: "worker", Uptime: threshold, Desired: json.RawMessage(`{"Mode":{"Replicated":{"Replicas":2}}}`)}, replicas: 2},
					{intent: state.Intent{Role: "web", Name: "web", ServiceID: "web", Uptime: threshold, Desired: json.RawMessage(`{"Mode":{"Replicated":{"Replicas":1}}}`)}, replicas: 1},
				}

				reason := c.recordRolloutFailure(ctx, op.ID, work, "rollout failed", nil)
				evidence := storedDiagnostics(t, s, op.ID)
				if len(evidence) != 2 || evidence[0].Role != "worker" || evidence[0].Logs != f.tails["c1"] ||
					evidence[0].ExitCode == nil || *evidence[0].ExitCode != 1 || evidence[1].Role != "web" || evidence[1].Logs != f.tails["c2"] {
					t.Fatalf("missing exit evidence: %+v", evidence)
				}
				if reason != tc.want {
					t.Fatalf("reason %q, want %q", reason, tc.want)
				}
			})
		})
	}
}
