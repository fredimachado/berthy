package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/session"
	"berthy/internal/state"
)

// pipeStreams hands out pipe-backed streams, as Docker would, and keeps the test's end of each.
type pipeStreams struct {
	mu   sync.Mutex
	ends []net.Conn
}

func (p *pipeStreams) open(ctx context.Context) *engine.Stream {
	a, b := net.Pipe()
	p.mu.Lock()
	p.ends = append(p.ends, b)
	p.mu.Unlock()
	return engine.NewStream(ctx, a)
}

// last is the test's end of the newest stream.
func (p *pipeStreams) last() net.Conn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ends[len(p.ends)-1]
}

// execSessionFake is the Docker side of an exec session on top of the runtime fake of an image app.
type execSessionFake struct {
	*runtimeEngine
	streams *pipeStreams
	mu      sync.Mutex
	calls   []string
	exit    int
}

func (f *execSessionFake) log(format string, a ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf(format, a...))
}
func (f *execSessionFake) callList() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, ";")
}
func (f *execSessionFake) StartExecSession(ctx context.Context, container string, command []string, tty bool) (*engine.Stream, string, error) {
	f.log("exec %s %s tty=%v", container, strings.Join(command, " "), tty)
	return f.streams.open(ctx), "exec-1", nil
}
func (f *execSessionFake) ResizeExec(_ context.Context, id string, rows, cols uint16) error {
	f.log("resize %s %dx%d", id, rows, cols)
	return nil
}
func (f *execSessionFake) ExecExitCode(context.Context, string) (int, error) { return f.exit, nil }

var sessionAdmin = Actor{ID: "admin", Name: "alice", Admin: true}

// execSessionFixture is a controller of image app "app" with one healthy container of its current generation.
func execSessionFixture(t *testing.T) (*Controller, *execSessionFake) {
	t.Helper()
	s := openStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "seed", ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "app", Manifest: []byte(`{}`), Bindings: []state.Binding{{App: "app", Role: "web", Name: "app-web", ServiceID: "id"}}}}); err != nil {
		t.Fatal(err)
	}
	spec := `{"Name":"app-web","TaskTemplate":{"ContainerSpec":{"Labels":{"berthy.operation":"gen1"}}}}`
	f := &execSessionFake{runtimeEngine: &runtimeEngine{store: s, op: "gen1", spec: []byte(spec)}, streams: &pipeStreams{}}
	return &Controller{Store: s, Engine: f, Environment: "stage"}, f
}

func TestExecSessionRunsInTheContainerExecWouldChooseAndStreams(t *testing.T) {
	c, f := execSessionFixture(t)
	ctx := context.Background()
	s, err := c.OpenSession(ctx, sessionAdmin, session.Request{Kind: "exec", App: "app", Command: []string{"sh", "-l"}, TTY: true, Size: &session.Size{Rows: 24, Cols: 80}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	if s.Kind != "exec" || s.App != "app" || s.Target != "app-web" || !s.TTY {
		t.Fatalf("session %+v", s)
	}
	if got := f.callList(); got != "exec container sh -l tty=true;resize exec-1 24x80" {
		t.Fatalf("calls %s", got)
	}
	go io.WriteString(f.streams.last(), "hello")
	buf := make([]byte, 5)
	if _, err = io.ReadFull(s.Stream(), buf); err != nil || string(buf) != "hello" {
		t.Fatalf("output %q %v", buf, err)
	}
	if err = s.Resize(ctx, session.Size{Rows: 30, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(f.callList(), "resize exec-1 30x100") {
		t.Fatalf("calls %s", f.callList())
	}
	f.exit = 7
	if code, err := s.Wait(ctx); err != nil || code != 7 {
		t.Fatalf("wait %d %v", code, err)
	}
	if err = s.Close(ctx); err != nil || s.Close(ctx) != nil {
		t.Fatalf("close %v", err)
	}
	if _, err = f.streams.last().Read(make([]byte, 1)); err == nil {
		t.Error("the stream was not closed")
	}
}

func TestExecSessionSendsNoSizeWithoutATerminal(t *testing.T) {
	c, f := execSessionFixture(t)
	s, err := c.OpenSession(context.Background(), sessionAdmin, session.Request{Kind: "exec", App: "app", Command: []string{"cat"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if got := f.callList(); got != "exec container cat tty=false" {
		t.Fatalf("calls %s", got)
	}
}

func TestExecSessionOfAComposeAppUsesTheServiceRule(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	raw, _ := json.Marshal(composeApp(false))
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "seed", ActorID: "a", Kind: "deploy", App: "db", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "db", Manifest: raw}}); err != nil {
		t.Fatal(err)
	}
	f := &composeSessionFake{composeDocker: newComposeDocker(
		engine.ComposeContainer{ID: "c-web", Service: "web", State: "running", Number: 1},
		engine.ComposeContainer{ID: "c-pg", Service: "postgres", State: "running", Number: 1},
	), streams: &pipeStreams{}}
	c := &Controller{Store: s, Engine: f, Environment: "stage"}
	_, err = c.OpenSession(ctx, sessionAdmin, session.Request{Kind: "exec", App: "db", Command: []string{"psql"}})
	var r Refusal
	if !errors.As(err, &r) || r != errExecServices {
		t.Fatalf("several services without --service: %v", err)
	}
	sess, err := c.OpenSession(ctx, sessionAdmin, session.Request{Kind: "exec", App: "db", Service: "postgres", Command: []string{"psql"}})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close(ctx)
	if !strings.HasSuffix(sess.Target, "/postgres") {
		t.Errorf("target %q", sess.Target)
	}
	if got := f.callList(); got != "exec c-pg psql tty=false" {
		t.Fatalf("calls %s", got)
	}
}

// composeSessionFake is a Compose app's Docker with the exec session calls.
type composeSessionFake struct {
	*composeDocker
	streams *pipeStreams
	mu      sync.Mutex
	calls   []string
}

func (f *composeSessionFake) callList() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, ";")
}
func (f *composeSessionFake) StartExecSession(ctx context.Context, container string, command []string, tty bool) (*engine.Stream, string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fmt.Sprintf("exec %s %s tty=%v", container, strings.Join(command, " "), tty))
	f.mu.Unlock()
	return f.streams.open(ctx), "exec-1", nil
}
func (f *composeSessionFake) ResizeExec(context.Context, string, uint16, uint16) error { return nil }
func (f *composeSessionFake) ExecExitCode(context.Context, string) (int, error)        { return 0, nil }

func (f *composeSessionFake) InspectContainer(context.Context, string) (engine.ContainerSnapshot, error) {
	return engine.ContainerSnapshot{}, nil
}
func (f *composeSessionFake) Exec(context.Context, string, []string) (engine.ExecResult, error) {
	return engine.ExecResult{}, nil
}

func TestSessionNeedsAnAdministratorAndAValidRequest(t *testing.T) {
	c, f := execSessionFixture(t)
	ctx := context.Background()
	if _, err := c.OpenSession(ctx, Actor{ID: "dev"}, session.Request{Kind: "exec", App: "app", Command: []string{"sh"}}); err == nil {
		t.Error("a non-administrator opened a session")
	}
	if _, err := c.OpenSession(ctx, sessionAdmin, session.Request{Kind: "exec", App: "app"}); err == nil {
		t.Error("a session without a command was opened")
	}
	if got := f.callList(); got != "" {
		t.Errorf("calls %s", got)
	}
}

// runSessionFake is the Docker side of a run session: the run fake with attach and resize.
type runSessionFake struct {
	*runFake
	streams *pipeStreams
	stopped chan struct{}
	once    sync.Once
	hold    bool          // WaitContainer returns only once the container is stopped
	gate    chan struct{} // when set, AttachSession returns only once it is closed
}

func newRunSessionFake() *runSessionFake {
	return &runSessionFake{runFake: newRunFake(), streams: &pipeStreams{}, stopped: make(chan struct{})}
}
func (f *runSessionFake) AttachSession(ctx context.Context, id string) (*engine.Stream, error) {
	f.log("attach " + id)
	if f.gate != nil {
		<-f.gate
	}
	return f.streams.open(ctx), nil
}
func (f *runSessionFake) ResizeContainer(_ context.Context, id string, rows, cols uint16) error {
	f.log(fmt.Sprintf("resize %dx%d", rows, cols))
	return nil
}
func (f *runSessionFake) StopContainer(ctx context.Context, id string, grace int) error {
	err := f.runFake.StopContainer(ctx, id, grace)
	f.once.Do(func() { close(f.stopped) })
	return err
}
func (f *runSessionFake) WaitContainer(ctx context.Context, id string) (int, error) {
	if f.hold {
		f.log("wait")
		select {
		case <-f.stopped:
			return 143, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return f.runFake.WaitContainer(ctx, id)
}
func (f *runSessionFake) InventoryContainers(context.Context) ([]engine.InventoryContainer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []engine.InventoryContainer
	for id, labels := range f.labels {
		out = append(out, engine.InventoryContainer{ID: id, Labels: labels, State: "exited"})
	}
	return out, nil
}

func runSessionFixture(t *testing.T) (*Controller, *runSessionFake) {
	t.Helper()
	c, f, _ := runFixture(t, runManifest("main"))
	sf := newRunSessionFake()
	sf.runFake = f
	c.Engine = sf
	return c, sf
}

func TestRunSessionCreatesAttachesStartsAndRemoves(t *testing.T) {
	c, f := runSessionFixture(t)
	ctx := context.Background()
	f.exit = 3
	s, err := c.OpenSession(ctx, sessionAdmin, session.Request{Kind: "run", App: "web", Command: []string{"sh"}, TTY: true, Size: &session.Size{Rows: 40, Cols: 120}})
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind != "run" || s.App != "web" || s.Target != runDigest || !s.TTY {
		t.Fatalf("session %+v", s)
	}
	if got := f.callList(); got != "create,attach ctr-"+nameOf(f)+",start,resize 40x120" {
		t.Fatalf("calls %s", got)
	}
	var spec struct {
		Env              []string
		Tty, OpenStdin   bool
		StdinOnce        bool
		AttachStdin      bool
		AttachStdout     bool
		AttachStderr     bool
		Labels           map[string]string
		HostConfig       struct{ Memory int64 }
		NetworkingConfig struct{ EndpointsConfig map[string]any }
	}
	if err = json.Unmarshal(f.specs[0], &spec); err != nil {
		t.Fatal(err)
	}
	if !spec.Tty || !spec.OpenStdin || !spec.StdinOnce || !spec.AttachStdin || !spec.AttachStdout || !spec.AttachStderr {
		t.Errorf("interactive settings: %s", f.specs[0])
	}
	if spec.Labels["berthy.environment"] != "test" || spec.Labels["berthy.oneoff"] != "session" || len(spec.Labels["berthy.session"]) < 16 || spec.Labels["berthy.operation"] != "" {
		t.Errorf("labels %v", spec.Labels)
	}
	if !contains(spec.Env, "BERTHY_ROLE=main") || !contains(spec.Env, "TOKEN=s3cret") || spec.HostConfig.Memory != 64*1024*1024 {
		t.Errorf("env %v limits %+v", spec.Env, spec.HostConfig)
	}
	if _, ok := spec.NetworkingConfig.EndpointsConfig["net"]; !ok {
		t.Errorf("network %+v", spec.NetworkingConfig)
	}
	if code, err := s.Wait(ctx); err != nil || code != 3 {
		t.Fatalf("wait %d %v", code, err)
	}
	if err = s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.callList(); !strings.HasSuffix(got, ",wait,remove") || strings.Count(got, "remove") != 1 {
		t.Fatalf("calls %s", got)
	}
	if err = s.Close(ctx); err != nil || strings.Count(f.callList(), "remove") != 1 {
		t.Fatalf("a second close: %v %s", err, f.callList())
	}
}

// nameOf is the part of the container name the fake derives its id from.
func nameOf(f *runSessionFake) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id := range f.labels {
		return strings.TrimPrefix(id, "ctr-")
	}
	return ""
}

func TestRunSessionWithoutATerminalHasNoTty(t *testing.T) {
	c, f := runSessionFixture(t)
	s, err := c.OpenSession(context.Background(), sessionAdmin, session.Request{Kind: "run", App: "web", Command: []string{"cat"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	var spec struct{ Tty, OpenStdin bool }
	if err = json.Unmarshal(f.specs[0], &spec); err != nil || spec.Tty || !spec.OpenStdin {
		t.Fatalf("spec %s %v", f.specs[0], err)
	}
	if strings.Contains(f.callList(), "resize") {
		t.Errorf("calls %s", f.callList())
	}
}

func TestRunSessionClosedBeforeItEndsStopsAndRemovesTheContainer(t *testing.T) {
	c, f := runSessionFixture(t)
	ctx := context.Background()
	s, err := c.OpenSession(ctx, sessionAdmin, session.Request{Kind: "run", App: "web", Command: []string{"sleep", "300"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.callList(); !strings.HasSuffix(got, "stop,remove") {
		t.Fatalf("calls %s", got)
	}
}

func TestRunSessionPastItsTimeoutIsStoppedAndReported(t *testing.T) {
	c, f := runSessionFixture(t)
	f.hold = true
	ctx := context.Background()
	s, err := c.OpenSession(ctx, sessionAdmin, session.Request{Kind: "run", App: "web", Command: []string{"sleep", "300"}, Timeout: "1s"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	_, err = s.Wait(ctx)
	var r Refusal
	if !errors.As(err, &r) || string(r) != "run timed out after 1s" || !PublicMessage(string(r)) {
		t.Fatalf("wait: %v", err)
	}
	if !strings.Contains(f.callList(), "stop") {
		t.Errorf("calls %s", f.callList())
	}
}

func TestRunSessionWithoutATimeoutHasNoTimeLimit(t *testing.T) {
	c, f := runSessionFixture(t)
	f.hold = true
	ctx := context.Background()
	s, err := c.OpenSession(ctx, sessionAdmin, session.Request{Kind: "run", App: "web", Command: []string{"sleep", "300"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	time.Sleep(1200 * time.Millisecond)
	if strings.Contains(f.callList(), "stop") {
		t.Errorf("stopped without a limit: %s", f.callList())
	}
}

func TestSessionIsRefusedBeforeAnythingIsCreated(t *testing.T) {
	role := session.Request{Kind: "run", App: "web", Role: "nope", Command: []string{"true"}}
	for name, tc := range map[string]struct {
		manifest string
		req      session.Request
		want     Refusal
	}{
		"several roles": {"ab", session.Request{Kind: "run", App: "web", Command: []string{"true"}}, errRunRoles},
		"unknown role":  {"a", role, errRunRoleUnknown},
		"bad timeout":   {"a", session.Request{Kind: "run", App: "web", Command: []string{"true"}, Timeout: "soon"}, errRunTimeout},
		"not deployed":  {"", session.Request{Kind: "run", App: "web", Command: []string{"true"}}, errRunNotDeployed},
	} {
		t.Run(name, func(t *testing.T) {
			var roles []string
			for _, r := range tc.manifest {
				roles = append(roles, string(r))
			}
			m := runManifest(roles...)
			if tc.manifest == "" {
				m = manifest.Manifest{}
			}
			c, f, _ := runFixture(t, m)
			sf := newRunSessionFake()
			sf.runFake = f
			c.Engine = sf
			_, err := c.OpenSession(context.Background(), sessionAdmin, tc.req)
			var r Refusal
			if !errors.As(err, &r) || r != tc.want {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if len(f.specs) != 0 || c.sessions.used != 0 {
				t.Errorf("%d containers, %d sessions", len(f.specs), c.sessions.used)
			}
		})
	}
	// An image app has no Compose service, and a Compose app cannot run.
	c, f := execSessionFixture(t)
	_, err := c.OpenSession(context.Background(), sessionAdmin, session.Request{Kind: "exec", App: "app", Service: "x", Command: []string{"sh"}})
	var r Refusal
	if !errors.As(err, &r) || r != errExecServiceImage || f.callList() != "" {
		t.Errorf("exec --service on an image app: %v %q", err, f.callList())
	}
}

func TestRunSessionOfAComposeAppIsRefused(t *testing.T) {
	c, f, _ := runFixture(t, manifest.Manifest{Project: "p", Apps: map[string]manifest.App{"web": {Kind: "compose"}}})
	sf := newRunSessionFake()
	sf.runFake = f
	c.Engine = sf
	_, err := c.OpenSession(context.Background(), sessionAdmin, session.Request{Kind: "run", App: "web", Command: []string{"true"}})
	var r Refusal
	if !errors.As(err, &r) || r != errRunCompose {
		t.Fatalf("got %v", err)
	}
}

func TestAtMostSixteenSessionsAreOpen(t *testing.T) {
	c, f := runSessionFixture(t)
	ctx := context.Background()
	var open []*Session
	for i := 0; i < maxSessions; i++ {
		s, err := c.OpenSession(ctx, sessionAdmin, session.Request{Kind: "run", App: "web", Command: []string{"cat"}})
		if err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		open = append(open, s)
	}
	if _, err := c.OpenSession(ctx, sessionAdmin, session.Request{Kind: "run", App: "web", Command: []string{"cat"}}); !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("the 17th: %v", err)
	}
	if len(f.specs) != maxSessions {
		t.Errorf("%d containers created", len(f.specs))
	}
	open[0].Close(ctx)
	s, err := c.OpenSession(ctx, sessionAdmin, session.Request{Kind: "run", App: "web", Command: []string{"cat"}})
	if err != nil {
		t.Fatalf("after one closed: %v", err)
	}
	_ = s
	c.CloseSessions(ctx)
	if got := strings.Count(f.callList(), "remove"); got != maxSessions+1 {
		t.Errorf("%d containers removed", got)
	}
	if c.sessions.used != 0 {
		t.Errorf("%d sessions still registered", c.sessions.used)
	}
}

func TestRemoveOrphanSessionsRemovesOnlyThisEnvironmentsSessionContainers(t *testing.T) {
	c, f := runSessionFixture(t)
	f.labels["mine-exited"] = map[string]string{"berthy.environment": "test", "berthy.session": "a", "berthy.oneoff": "session"}
	f.labels["mine-running"] = map[string]string{"berthy.environment": "test", "berthy.session": "b"}
	f.running["mine-running"] = true
	f.labels["other-env"] = map[string]string{"berthy.environment": "prod", "berthy.session": "c"}
	f.labels["a-hook"] = map[string]string{"berthy.environment": "test", "berthy.operation": "op1", "berthy.oneoff": "pre_hook"}
	f.labels["unrelated"] = map[string]string{"app": "x"}
	if err := c.RemoveOrphanSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"mine-exited", "mine-running"} {
		if _, ok := f.labels[gone]; ok {
			t.Errorf("%s was kept", gone)
		}
	}
	for _, kept := range []string{"other-env", "a-hook", "unrelated"} {
		if _, ok := f.labels[kept]; !ok {
			t.Errorf("%s was removed", kept)
		}
	}
}

func TestNoSessionOpensOnceTheAgentIsStopping(t *testing.T) {
	c, f := runSessionFixture(t)
	c.CloseSessions(context.Background())
	_, err := c.OpenSession(context.Background(), sessionAdmin, session.Request{Kind: "run", App: "web", Command: []string{"cat"}})
	var r Refusal
	if !errors.As(err, &r) || r != errSessionsStopped || len(f.specs) != 0 {
		t.Fatalf("got %v with %d containers", err, len(f.specs))
	}
}

func TestCloseSessionsWhileASessionIsBeingOpenedLeavesNothingHalfClosed(t *testing.T) {
	c, f := runSessionFixture(t)
	f.gate = make(chan struct{})
	opened := make(chan error, 1)
	go func() {
		_, err := c.OpenSession(context.Background(), sessionAdmin, session.Request{Kind: "run", App: "web", Command: []string{"cat"}})
		opened <- err
	}()
	for i := 0; i < 200 && !strings.Contains(f.callList(), "attach"); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(f.callList(), "attach") {
		t.Fatal("the session did not get as far as the attach")
	}
	c.CloseSessions(context.Background())
	close(f.gate)
	var r Refusal
	if err := <-opened; !errors.As(err, &r) || r != errSessionsStopped {
		t.Fatalf("the session that was opening: %v", err)
	}
	if got := f.callList(); !strings.HasSuffix(got, "remove") || c.sessions.used != 0 {
		t.Errorf("calls %s, %d sessions", got, c.sessions.used)
	}
}

func TestCloseSessionsWaitsForTheHandlersOfTheSessions(t *testing.T) {
	c := &Controller{}
	done := c.TrackSession()
	returned := make(chan struct{})
	go func() {
		c.CloseSessions(context.Background())
		close(returned)
	}()
	select {
	case <-returned:
		t.Fatal("CloseSessions returned before the handler was done")
	case <-time.After(100 * time.Millisecond):
	}
	done()
	done() // a second call changes nothing
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("CloseSessions did not return once the handler was done")
	}
}

func TestCloseSessionsGivesUpWhenItsContextEnds(t *testing.T) {
	c := &Controller{}
	done := c.TrackSession()
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	c.CloseSessions(ctx)
}
