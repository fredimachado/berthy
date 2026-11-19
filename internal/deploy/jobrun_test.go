package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"berthy/internal/engine"
	"berthy/internal/jobs"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// jobFake is the Docker side of a job: the one-off container calls of a run, the tail of a container's logs, and for a Compose
// app the containers of its project and the tail of an exec.
type jobFake struct {
	*runFake
	tail       string // what LogTail returns
	composeOf  []engine.ComposeContainer
	execExit   int
	execTail   string
	execErr    error
	execCmd    []string
	execTarget string
	streamOut  string // what the command writes to its stdout when it is streamed
	streamIn   []byte // what the command read of its stdin
}

func (f *jobFake) DiagnoseContainer(context.Context, string) (engine.ContainerDiagnosis, error) {
	return engine.ContainerDiagnosis{}, nil
}
func (f *jobFake) LogTail(_ context.Context, id string, lines, maxBytes int) (string, bool, error) {
	f.log("tail")
	if lines != diagnosticLogLines || maxBytes != diagnosticLogBytes {
		return "", false, errors.New("unexpected limits")
	}
	return f.tail, false, nil
}
func (f *jobFake) ComposeContainers(context.Context, string) ([]engine.ComposeContainer, error) {
	return f.composeOf, nil
}
func (f *jobFake) ExecStream(_ context.Context, id string, command []string, stdin io.Reader, stdout io.Writer, maxBytes int) (int, string, error) {
	f.log("exec")
	f.execTarget, f.execCmd = id, command
	f.stream(stdin, stdout)
	return f.execExit, f.execTail, f.execErr
}

// AttachStream is the attach of a streamed one-off container: it starts the container, gives it its input and takes its output.
func (f *jobFake) AttachStream(ctx context.Context, id string, stdin io.Reader, stdout io.Writer, maxBytes int, start func() error) (string, error, error) {
	f.log("attach")
	if err := start(); err != nil {
		return "", nil, err
	}
	if f.block {
		<-ctx.Done()
		return f.execTail, nil, ctx.Err()
	}
	f.stream(stdin, stdout)
	return f.execTail, nil, f.execErr
}

// stream plays the command: it reads all of its input and writes its output.
func (f *jobFake) stream(stdin io.Reader, stdout io.Writer) {
	if stdin != nil {
		f.streamIn, _ = io.ReadAll(stdin)
	}
	if stdout != nil {
		io.WriteString(stdout, f.streamOut)
	}
}

func jobFixture(t *testing.T) (*Controller, *jobFake) {
	t.Helper()
	m := runManifest("web", "worker")
	web := m.Apps["web"].Roles["web"]
	web.Volumes = []string{"uploads:/data", "cache:/cache:ro"}
	m.Apps["web"].Roles["web"] = web
	c, f, _ := runFixture(t, m)
	jf := &jobFake{runFake: f}
	c.Engine = jf
	return c, jf
}

var webJob = manifest.Job{App: "web", Role: "web", Command: []string{"sh", "-c", "echo hi"}, Timeout: "1m"}

func TestRunJobRunsInItsOwnContainerOfTheAppliedRelease(t *testing.T) {
	c, f := jobFixture(t)
	out, err := c.RunJob(context.Background(), imageJobManifest(), "report", webJob, 7)
	if err != nil || out.Exit == nil || *out.Exit != 0 || out.TimedOut || out.Output != "" || out.Container != "ctr-berthy-job-test-7" {
		t.Fatalf("outcome %+v %v", out, err)
	}
	// A run that succeeds reads no output, and the container always goes.
	if got := f.callList(); got != "create,start,wait,remove" {
		t.Fatalf("calls %s", got)
	}
	var spec struct {
		Image      string
		Entrypoint []string
		Cmd        []string
		Env        []string
		Labels     map[string]string
		HostConfig struct {
			Memory   int64
			NanoCpus int64
			Mounts   []struct {
				Type, Source, Target string
				ReadOnly             bool
			}
		}
		NetworkingConfig struct{ EndpointsConfig map[string]any }
	}
	if err = json.Unmarshal(f.specs[0], &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Image != runDigest || spec.Entrypoint[0] != "sh" || strings.Join(spec.Cmd, " ") != "-c echo hi" {
		t.Errorf("image and command: %s", f.specs[0])
	}
	if !contains(spec.Env, "BERTHY_ROLE=web") || !contains(spec.Env, "TOKEN=s3cret") || !contains(spec.Env, "GREETING=hi") {
		t.Errorf("env %v", spec.Env)
	}
	if spec.Labels["berthy.environment"] != "test" || spec.Labels["berthy.job"] != "report" || spec.Labels["berthy.oneoff"] != "job" || spec.Labels["berthy.operation"] != "" || spec.Labels["berthy.session"] != "" {
		t.Errorf("labels %v", spec.Labels)
	}
	if spec.HostConfig.Memory != 64*1024*1024 || spec.HostConfig.NanoCpus != 5e8 {
		t.Errorf("limits %+v", spec.HostConfig)
	}
	if len(spec.HostConfig.Mounts) != 2 || spec.HostConfig.Mounts[0].Type != "volume" || spec.HostConfig.Mounts[0].Source != "uploads" || spec.HostConfig.Mounts[0].Target != "/data" || spec.HostConfig.Mounts[0].ReadOnly ||
		spec.HostConfig.Mounts[1].Source != "cache" || !spec.HostConfig.Mounts[1].ReadOnly {
		t.Errorf("mounts %+v", spec.HostConfig.Mounts)
	}
	if _, ok := spec.NetworkingConfig.EndpointsConfig["net"]; !ok {
		t.Errorf("network %+v", spec.NetworkingConfig)
	}
}

func TestRunJobNeedsNoReplicaAndTakesNoLock(t *testing.T) {
	c, f := jobFixture(t)
	// Nothing is running: the fake knows no service task, and the lock is held by another operation.
	if _, _, err := c.Store.Admit(context.Background(), state.Admission{RequestID: "hold", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if out, err := c.RunJob(context.Background(), imageJobManifest(), "report", webJob, 1); err != nil || out.Exit == nil || *out.Exit != 0 {
		t.Fatalf("outcome %+v %v", out, err)
	}
	if !strings.Contains(f.callList(), "start") {
		t.Fatalf("calls %s", f.callList())
	}
}

func TestRunJobKeepsTheRedactedTailOfAFailedRunAndRemovesTheContainer(t *testing.T) {
	c, f := jobFixture(t)
	f.exit = 3
	f.tail = "2026-11-19T10:00:00Z stderr boom token=s3cret\n"
	out, err := c.RunJob(context.Background(), imageJobManifest(), "report", webJob, 8)
	if err != nil || out.Exit == nil || *out.Exit != 3 || out.TimedOut {
		t.Fatalf("outcome %+v %v", out, err)
	}
	if out.Output != "2026-11-19T10:00:00Z stderr boom token=[redacted]\n" {
		t.Fatalf("output %q", out.Output)
	}
	// The output is read before the container goes.
	if got := f.callList(); got != "create,start,wait,tail,remove" {
		t.Fatalf("calls %s", got)
	}
}

func TestRunJobStopsTheContainerOfATimedOutRunAndKeepsItsOutput(t *testing.T) {
	c, f := jobFixture(t)
	f.block = true
	f.tail = "still working token=s3cret\n"
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	out, err := c.RunJob(ctx, imageJobManifest(), "report", webJob, 9)
	if err != nil || !out.TimedOut || out.Exit != nil || out.Output != "still working token=[redacted]\n" || out.Container == "" {
		t.Fatalf("outcome %+v %v", out, err)
	}
	if got := f.callList(); got != "create,start,wait,stop,tail,remove" {
		t.Fatalf("calls %s", got)
	}
}

func TestRunJobOfACanceledRunStopsAndRemovesTheContainer(t *testing.T) {
	c, f := jobFixture(t)
	f.block = true
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	out, err := c.RunJob(ctx, imageJobManifest(), "report", webJob, 10)
	if !errors.Is(err, context.Canceled) || out.TimedOut || out.Output != "" {
		t.Fatalf("outcome %+v %v", out, err)
	}
	if got := f.callList(); got != "create,start,wait,stop,remove" {
		t.Fatalf("calls %s", got)
	}
}

// A run that a person cancels leaves the way a failed one does: its container is stopped, the last lines it wrote are kept, and
// the container is removed.
func TestRunJobStoppedByACancelKeepsItsOutput(t *testing.T) {
	c, f := jobFixture(t)
	f.block = true
	f.tail = "halfway there\n"
	ctx, cancel := context.WithCancelCause(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel(&jobs.Cancelled{By: "tok"})
	}()
	out, err := c.RunJob(ctx, imageJobManifest(), "report", webJob, 11)
	if !errors.Is(err, context.Canceled) || out.TimedOut || out.Output != "halfway there\n" || out.Container == "" {
		t.Fatalf("outcome %+v %v", out, err)
	}
	if got := f.callList(); got != "create,start,wait,stop,tail,remove" {
		t.Fatalf("calls %s", got)
	}
}

func TestRunJobSaysWhyItCouldNotRun(t *testing.T) {
	c, f := jobFixture(t)
	m := imageJobManifest()
	for _, tc := range []struct {
		name string
		job  manifest.Job
		want string
	}{
		{"app not deployed", manifest.Job{App: "other", Role: "web", Command: []string{"true"}}, "app is not deployed"},
		{"unknown role", manifest.Job{App: "web", Role: "ghost", Command: []string{"true"}}, string(errRunRoleUnknown)},
	} {
		out, err := c.RunJob(context.Background(), m, "report", tc.job, 11)
		if err != nil || out.Exit != nil || out.Detail != tc.want {
			t.Errorf("%s: outcome %+v %v", tc.name, out, err)
		}
	}
	if f.callList() != "" {
		t.Errorf("calls %s", f.callList())
	}
}

// imageJobManifest is the running manifest as the scheduler passes it: a job finds the kind of its app in it.
func imageJobManifest() manifest.Manifest {
	return manifest.Manifest{Project: "proj", Apps: map[string]manifest.App{"web": {Kind: "image"}, "other": {Kind: "image"}}}
}

func composeJobManifest() manifest.Manifest {
	return manifest.Manifest{Project: "proj", Apps: map[string]manifest.App{"db": {Kind: "compose"}}}
}

func TestRunJobOfAComposeAppExecsInTheServiceAndKeepsTheRedactedTail(t *testing.T) {
	c, f := jobFixture(t)
	f.composeOf = []engine.ComposeContainer{{ID: "pg-1", Service: "postgres", State: "running", Number: 1}}
	f.execExit = 2
	f.execTail = "line one\nthe password is s3cret\n"
	job := manifest.Job{App: "db", Service: "postgres", Command: []string{"pg_dump"}, Timeout: "1m"}
	out, err := c.RunJob(context.Background(), composeJobManifest(), "dump", job, 12)
	if err != nil || out.Exit == nil || *out.Exit != 2 || out.Container != "pg-1" || out.Output != "line one\nthe password is [redacted]\n" {
		t.Fatalf("outcome %+v %v", out, err)
	}
	if f.execTarget != "pg-1" || strings.Join(f.execCmd, " ") != "pg_dump" || f.callList() != "exec" {
		t.Fatalf("exec in %s: %v, calls %s", f.execTarget, f.execCmd, f.callList())
	}
	// A run that succeeds keeps no output.
	f.execExit = 0
	if out, err = c.RunJob(context.Background(), composeJobManifest(), "dump", job, 13); err != nil || out.Exit == nil || *out.Exit != 0 || out.Output != "" {
		t.Fatalf("succeeded: %+v %v", out, err)
	}
}

func TestRunJobOfAComposeAppKeepsOnlyTheLastTwoHundredLines(t *testing.T) {
	c, f := jobFixture(t)
	f.composeOf = []engine.ComposeContainer{{ID: "pg-1", Service: "postgres", State: "running", Number: 1}}
	f.execExit = 1
	var b strings.Builder
	for i := 1; i <= 500; i++ {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString("\n")
	}
	f.execTail = b.String()
	out, err := c.RunJob(context.Background(), composeJobManifest(), "dump", manifest.Job{App: "db", Command: []string{"x"}, Timeout: "1m"}, 14)
	lines := strings.Split(strings.TrimSuffix(out.Output, "\n"), "\n")
	if err != nil || len(lines) != diagnosticLogLines || lines[len(lines)-1] != "line "+strings.Repeat("x", 500%7) {
		t.Fatalf("%d lines, %v", len(lines), err)
	}
}

func TestRunJobOfAComposeAppTimesOutWithTheOutputSoFar(t *testing.T) {
	c, f := jobFixture(t)
	f.composeOf = []engine.ComposeContainer{{ID: "pg-1", Service: "postgres", State: "running", Number: 1}}
	f.execErr = context.DeadlineExceeded
	f.execTail = "half done s3cret\n"
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	out, err := c.RunJob(ctx, composeJobManifest(), "dump", manifest.Job{App: "db", Command: []string{"x"}, Timeout: "1m"}, 15)
	if err != nil || !out.TimedOut || out.Output != "half done [redacted]\n" {
		t.Fatalf("outcome %+v %v", out, err)
	}
}

func TestRunJobOfAComposeAppSaysWhyItCouldNotRun(t *testing.T) {
	c, f := jobFixture(t)
	for _, tc := range []struct {
		name       string
		containers []engine.ComposeContainer
		service    string
		want       string
	}{
		{"none running", nil, "postgres", "no running container for db/postgres"},
		{"none running, no service", []engine.ComposeContainer{{ID: "old", Service: "web", State: "exited"}}, "", "no running container for db"},
		{"two services", []engine.ComposeContainer{{ID: "a", Service: "postgres", State: "running"}, {ID: "b", Service: "web", State: "running"}}, "", "name the Compose service as service"},
	} {
		f.composeOf = tc.containers
		out, err := c.RunJob(context.Background(), composeJobManifest(), "dump", manifest.Job{App: "db", Service: tc.service, Command: []string{"x"}, Timeout: "1m"}, 16)
		if err != nil || out.Exit != nil || out.Detail != tc.want {
			t.Errorf("%s: outcome %+v %v", tc.name, out, err)
		}
	}
}

// RunJob is the Runner of the scheduler.
var _ jobs.RunnerFunc = (*Controller)(nil).RunJob
