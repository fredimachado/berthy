package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// runFake is the Docker side of a run: the one-off container methods over the positive engine's services and images.
type runFake struct {
	*positiveEngine
	mu      sync.Mutex
	calls   []string
	specs   []json.RawMessage
	exit    int
	logs    []byte
	block   bool // WaitContainer returns only when its context ends
	running map[string]bool
	labels  map[string]map[string]string
}

func newRunFake() *runFake {
	return &runFake{positiveEngine: &positiveEngine{}, running: map[string]bool{}, labels: map[string]map[string]string{}}
}
func (f *runFake) log(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}
func (f *runFake) CreateContainer(_ context.Context, name string, spec json.RawMessage) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "create")
	f.specs = append(f.specs, spec)
	var s struct{ Labels map[string]string }
	_ = json.Unmarshal(spec, &s)
	f.labels["ctr-"+name] = s.Labels
	return "ctr-" + name, nil
}
func (f *runFake) StartContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "start")
	f.running[id] = true
	return nil
}
func (f *runFake) WaitContainer(ctx context.Context, id string) (int, error) {
	f.log("wait")
	if f.block {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running[id] = false
	return f.exit, nil
}
func (f *runFake) ContainerLogs(context.Context, string) ([]byte, error) {
	f.log("logs")
	return f.logs, nil
}
func (f *runFake) InspectContainer(_ context.Context, id string) (engine.ContainerSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	labels, ok := f.labels[id]
	if !ok {
		return engine.ContainerSnapshot{}, &engine.StatusError{Code: 404}
	}
	return engine.ContainerSnapshot{ID: id, Labels: labels, Running: f.running[id]}, nil
}
func (f *runFake) StopContainer(_ context.Context, id string, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "stop")
	f.running[id] = false
	return nil
}
func (f *runFake) RemoveContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "remove")
	delete(f.labels, id)
	delete(f.running, id)
	return nil
}
func (f *runFake) callList() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, ",")
}

const runDigest = "example/web@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func runManifest(roles ...string) manifest.Manifest {
	one := 1
	m := manifest.Manifest{Project: "proj", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Env: manifest.Env{Plain: map[string]string{"GREETING": "hi"}, Secret: []manifest.SecretVar{{Var: "TOKEN", Secret: "TOKEN"}}}, Roles: map[string]manifest.Role{}}}}
	for _, r := range roles {
		m.Apps["web"].Roles[r] = manifest.Role{Replicas: &one, Resources: &manifest.Resources{CPU: "0.5", Memory: "64M"}}
	}
	return m
}

// runFixture is a controller whose app web runs release v1 with the given manifest. Nothing is applied when m has no apps.
func runFixture(t *testing.T, m manifest.Manifest) (*Controller, *runFake, *state.Store) {
	t.Helper()
	s := openStore(t)
	ctx := context.Background()
	if err := s.PutSecret(ctx, state.Actor{ID: "a"}, state.SecretInfo{Name: "TOKEN"}, []byte("s3cret")); err != nil {
		t.Fatal(err)
	}
	if len(m.Apps) > 0 {
		op, _, err := s.Admit(ctx, state.Admission{RequestID: "seed", ActorID: "admin", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(m)
		if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "web", Digest: runDigest, Tag: "v1", Manifest: raw}}); err != nil {
			t.Fatal(err)
		}
	}
	f := newRunFake()
	return &Controller{Engine: f, Store: s, Environment: "test"}, f, s
}

var runAdmin = Actor{ID: "admin", Admin: true}

func runRequest(id string, command ...string) Request {
	return Request{RequestID: id, App: "web", Kind: "run", Command: command}
}

// runOutput is the output receipt of a finished run operation.
func runOutput(t *testing.T, s *state.Store, op string) (output string, exit int) {
	t.Helper()
	steps, err := s.Intents(context.Background(), op)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range steps {
		if i.Kind == "exec" && i.Phase == "finished" {
			var r struct {
				Output   string `json:"output"`
				ExitCode int    `json:"exit_code"`
			}
			if err = json.Unmarshal(i.Receipt, &r); err != nil {
				t.Fatal(err)
			}
			return r.Output, r.ExitCode
		}
	}
	t.Fatal("no output was recorded")
	return "", 0
}

func TestRunExecutesInANewContainerOfTheAppliedRelease(t *testing.T) {
	c, f, s := runFixture(t, runManifest("main"))
	f.logs = []byte("hi main v1 s3cret\n")
	op, _, err := admit(t, c, context.Background(), runAdmin, runRequest("r1", "sh", "-c", "echo $GREETING"))
	if err != nil {
		t.Fatal(err)
	}
	if got := waitOperation(t, s, op.ID); got.State != "succeeded" {
		t.Fatalf("state %s: %s", got.State, got.Err)
	}
	if len(f.specs) != 1 {
		t.Fatalf("%d containers created", len(f.specs))
	}
	var spec struct {
		Image      string
		Entrypoint []string
		Cmd        []string
		Env        []string
		Labels     map[string]string
		HostConfig struct {
			NanoCpus int64
			Memory   int64
		}
		NetworkingConfig struct{ EndpointsConfig map[string]any }
	}
	if err = json.Unmarshal(f.specs[0], &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Image != runDigest || spec.Entrypoint[0] != "sh" || strings.Join(spec.Cmd, " ") != "-c echo $GREETING" {
		t.Fatalf("image or command: %+v", spec)
	}
	for _, want := range []string{"GREETING=hi", "TOKEN=s3cret", "BERTHY_ROLE=main", "BERTHY_VERSION=v1", "BERTHY_APP=web", "BERTHY_IMAGE_DIGEST=sha256:" + strings.Repeat("a", 64)} {
		if !contains(spec.Env, want) {
			t.Errorf("environment lacks %s: %v", want, spec.Env)
		}
	}
	if spec.HostConfig.Memory != 64*1024*1024 || spec.HostConfig.NanoCpus != 500000000 {
		t.Errorf("limits: %+v", spec.HostConfig)
	}
	if spec.Labels["berthy.oneoff"] != "run" || spec.Labels["berthy.operation"] != op.ID {
		t.Errorf("labels: %v", spec.Labels)
	}
	if _, ok := spec.NetworkingConfig.EndpointsConfig["net"]; !ok {
		t.Errorf("not on the shared network: %+v", spec.NetworkingConfig)
	}
	output, exit := runOutput(t, s, op.ID)
	if exit != 0 || strings.Contains(output, "s3cret") || !strings.Contains(output, "hi main v1 [redacted]") {
		t.Errorf("output %q exit %d", output, exit)
	}
	if got := f.callList(); got != "create,start,wait,logs,remove" {
		t.Errorf("container calls %s: the container is removed once its output is recorded", got)
	}
	if _, err = s.Active(context.Background()); err == nil {
		t.Error("the deploy lock is still held")
	}
}

func TestRunRemovesTheContainerAndFailsOnANonzeroExit(t *testing.T) {
	c, f, s := runFixture(t, runManifest("main"))
	f.exit, f.logs = 3, []byte("boom\n")
	op, _, err := admit(t, c, context.Background(), runAdmin, runRequest("r1", "false"))
	if err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, s, op.ID)
	if got.State != "failed" || got.Err != "run returned nonzero" {
		t.Fatalf("outcome %s %q", got.State, got.Err)
	}
	if output, exit := runOutput(t, s, op.ID); output != "boom\n" || exit != 3 {
		t.Errorf("output %q exit %d", output, exit)
	}
	if !strings.HasSuffix(f.callList(), "logs,remove") || strings.Count(f.callList(), "remove") != 1 {
		t.Errorf("calls %s", f.callList())
	}
	if !PublicMessage(got.Err) {
		t.Error("the failure is not shown to the person")
	}
}

func TestRunTimeoutStopsAndRemovesTheContainerAndReleasesTheLock(t *testing.T) {
	c, f, s := runFixture(t, runManifest("main"))
	f.block, f.logs = true, []byte("partial\n")
	req := runRequest("r1", "sleep", "30")
	req.Timeout = "1s"
	op, _, err := admit(t, c, context.Background(), runAdmin, req)
	if err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, s, op.ID)
	if got.State != "failed" || got.Err != "run timed out after 1s" || !PublicMessage(got.Err) {
		t.Fatalf("outcome %s %q", got.State, got.Err)
	}
	calls := f.callList()
	if !strings.Contains(calls, "stop") || !strings.HasSuffix(calls, "logs,remove") {
		t.Errorf("calls %s", calls)
	}
	if output, _ := runOutput(t, s, op.ID); output != "partial\n" {
		t.Errorf("output %q", output)
	}
	if _, err = s.Active(context.Background()); err == nil {
		t.Error("the deploy lock is still held")
	}
}

func TestRunIsRefusedBeforeItIsJournaled(t *testing.T) {
	long := runRequest("long", "true")
	long.Timeout = "25h"
	bad := runRequest("bad", "true")
	bad.Timeout = "soon"
	role := runRequest("role", "true")
	role.Role = "nope"
	for name, tc := range map[string]struct {
		manifest manifest.Manifest
		req      Request
		want     Refusal
	}{
		"several roles": {runManifest("a", "b"), runRequest("x", "true"), errRunRoles},
		"unknown role":  {runManifest("a"), role, errRunRoleUnknown},
		"no command":    {runManifest("a"), runRequest("x"), errRunCommand},
		"empty command": {runManifest("a"), runRequest("x", ""), errRunCommand},
		"long timeout":  {runManifest("a"), long, errRunTimeout},
		"bad timeout":   {runManifest("a"), bad, errRunTimeout},
		"not deployed":  {manifest.Manifest{}, runRequest("x", "true"), errRunNotDeployed},
		"compose app":   {manifest.Manifest{Project: "p", Apps: map[string]manifest.App{"web": {Kind: "compose"}}}, runRequest("x", "true"), errRunCompose},
	} {
		t.Run(name, func(t *testing.T) {
			c, f, s := runFixture(t, tc.manifest)
			_, _, err := admit(t, c, context.Background(), runAdmin, tc.req)
			var r Refusal
			if !errors.As(err, &r) || r != tc.want || !PublicMessage(string(r)) {
				t.Fatalf("admit: %v, want %q", err, tc.want)
			}
			plan := tc.req
			plan.RequestID = ""
			if _, err = c.Plan(context.Background(), runAdmin, plan); !errors.As(err, &r) || r != tc.want {
				t.Fatalf("plan: %v, want %q", err, tc.want)
			}
			if _, err = s.Active(context.Background()); err == nil {
				t.Error("an operation was journaled")
			}
			if len(f.specs) != 0 {
				t.Error("a container was created")
			}
		})
	}
	if string(errRunCompose) != "run is for image apps; use exec --service" {
		t.Errorf("compose refusal: %q", errRunCompose)
	}
}

func TestRunNeedsAnAdministrator(t *testing.T) {
	c, f, s := runFixture(t, runManifest("main"))
	op, _, err := admit(t, c, context.Background(), Actor{ID: "dev"}, runRequest("r1", "true"))
	if err != nil {
		t.Fatal(err)
	}
	if got := waitOperation(t, s, op.ID); got.State != "failed" || len(f.specs) != 0 {
		t.Fatalf("outcome %s with %d containers", got.State, len(f.specs))
	}
	if _, err = c.Plan(context.Background(), Actor{ID: "dev"}, Request{App: "web", Kind: "run", Command: []string{"true"}}); err == nil {
		t.Fatal("a plan was given to a non-administrator")
	}
}

func TestRunPlanNamesTheImageAndRoleAndIsStable(t *testing.T) {
	c, f, _ := runFixture(t, runManifest("main"))
	req := Request{App: "web", Kind: "run", Command: []string{"rake", "db:seed"}}
	first, err := c.Plan(context.Background(), runAdmin, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Plan(context.Background(), runAdmin, req)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Fatalf("plans differ:\n%s\n%s", a, b)
	}
	if first.App != "web" || first.Image != runDigest || first.Tag != "v1" || strings.Join(first.Roles, ",") != "main" || strings.Join(first.Command, " ") != "rake db:seed" {
		t.Fatalf("plan %s", a)
	}
	if len(f.specs) != 0 || f.callList() != "" {
		t.Error("a dry run touched Docker")
	}
}

func TestRecoveryStopsTheContainerOfACrashedRun(t *testing.T) {
	c, f, s := runFixture(t, runManifest("main"))
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "crash", ActorID: "admin", Kind: "run", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	name := "berthy-" + op.ID + "-30100"
	if err = s.RecordIntent(ctx, op.ID, state.Intent{Ordinal: runOrdinal, Kind: "oneoff", Role: "run", Name: name, Desired: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	f.labels[name] = map[string]string{"berthy.environment": "test", "berthy.operation": op.ID, "berthy.oneoff": "run"}
	f.running[name] = true
	if err = s.MarkStep(ctx, op.ID, runOrdinal, "started", json.RawMessage(`{"id":"`+name+`"}`)); err != nil {
		t.Fatal(err)
	}
	if err = c.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.Operation(ctx, op.ID)
	if err != nil || got.State != "interrupted" {
		t.Fatalf("state %s %q: %v", got.State, got.Err, err)
	}
	if f.running[name] || !strings.Contains(f.callList(), "stop") {
		t.Errorf("the run's container still runs (calls %s)", f.callList())
	}
}

func TestOneOffsOfHooksHaveNoLimitsAndNoRole(t *testing.T) {
	c, f, id, p := hookFixture(t)
	f.exits = []int{0}
	if _, err := c.runOneOff(context.Background(), id, p, oneOff{Ordinal: 1, Kind: "pre_hook", Argv: []string{"/tool"}, Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	var spec struct {
		HostConfig *json.RawMessage
		Env        []string
	}
	if err := json.Unmarshal(f.specs[0], &spec); err != nil {
		t.Fatal(err)
	}
	if spec.HostConfig != nil || !contains(spec.Env, "BERTHY_ROLE=") {
		t.Fatalf("spec %s", f.specs[0])
	}
}
