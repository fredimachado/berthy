package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/engine"
	"berthy/internal/session"
	"berthy/internal/state"
)

// sessionDocker is the Docker side of exec sessions for a Compose app web: it has one running container, and the command it
// runs echoes its input as "got:<input>". With a terminal its output is raw; without one it is framed as Docker frames it.
// The command ends, with the exit code, when its input ends.
type sessionDocker struct {
	planEngine
	mu      sync.Mutex
	started []string // the commands, one per session
	ended   chan string
	removed chan string
	tty     bool
	exit    int
}

func newSessionDocker() *sessionDocker {
	return &sessionDocker{ended: make(chan string, 64), removed: make(chan string, 64)}
}

func (d *sessionDocker) ComposeContainers(context.Context, string) ([]engine.ComposeContainer, error) {
	return []engine.ComposeContainer{{ID: "c1", Service: "web", State: "running", Number: 1}}, nil
}
func (d *sessionDocker) InspectContainer(context.Context, string) (engine.ContainerSnapshot, error) {
	return engine.ContainerSnapshot{}, nil
}
func (d *sessionDocker) Exec(context.Context, string, []string) (engine.ExecResult, error) {
	return engine.ExecResult{}, nil
}
func (d *sessionDocker) ResizeExec(context.Context, string, uint16, uint16) error { return nil }
func (d *sessionDocker) ExecExitCode(context.Context, string) (int, error)        { return d.exit, nil }
func (d *sessionDocker) startedList() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.started...)
}

func (d *sessionDocker) StartExecSession(ctx context.Context, _ string, command []string, tty bool) (*engine.Stream, string, error) {
	s, err := d.pipe(ctx, strings.Join(command, " "), tty)
	return s, "exec-1", err
}

// pipe is a stream to a command that echoes its input.
func (d *sessionDocker) pipe(ctx context.Context, command string, tty bool) (*engine.Stream, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer l.Close()
	client, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		return nil, err
	}
	far, err := l.Accept()
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.started = append(d.started, command)
	d.mu.Unlock()
	go func() {
		defer far.Close()
		buf := make([]byte, 1024)
		for {
			n, err := far.Read(buf)
			if n > 0 {
				out := []byte("got:" + string(buf[:n]))
				if !tty {
					out = dockerFrame(1, out)
					out = append(out, dockerFrame(2, []byte("warn"))...)
				}
				far.Write(out)
			}
			if err != nil {
				d.ended <- "input ended"
				return
			}
		}
	}()
	return engine.NewStream(ctx, client), nil
}

// The run side: containers are made in a map, and attach gives the echoing command.
func (d *sessionDocker) InspectImage(context.Context, string) (engine.Image, error) {
	return engine.Image{ID: "image"}, nil
}
func (d *sessionDocker) CreateContainer(_ context.Context, _ string, spec json.RawMessage) (string, error) {
	var s struct{ Tty bool }
	_ = json.Unmarshal(spec, &s)
	d.mu.Lock()
	d.tty = s.Tty
	d.mu.Unlock()
	return "run-ctr", nil
}
func (d *sessionDocker) AttachSession(ctx context.Context, _ string) (*engine.Stream, error) {
	d.mu.Lock()
	tty := d.tty
	d.mu.Unlock()
	return d.pipe(ctx, "run", tty)
}
func (d *sessionDocker) StartContainer(context.Context, string) error                  { return nil }
func (d *sessionDocker) WaitContainer(context.Context, string) (int, error)            { return d.exit, nil }
func (d *sessionDocker) ContainerLogs(context.Context, string) ([]byte, error)         { return nil, nil }
func (d *sessionDocker) StopContainer(context.Context, string, int) error              { return nil }
func (d *sessionDocker) ResizeContainer(context.Context, string, uint16, uint16) error { return nil }
func (d *sessionDocker) RemoveContainer(_ context.Context, id string) error {
	d.removed <- id
	return nil
}

func dockerFrame(stream byte, payload []byte) []byte {
	hdr := []byte{stream, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload)))
	return append(hdr, payload...)
}

// sessionFixture is the approval fixture with Compose app web deployed and a Docker that serves sessions, behind a real HTTP
// server.
type sessionFixture struct {
	*approvalFixture
	docker *sessionDocker
	srv    *httptest.Server
}

func newSessionFixture(t *testing.T, protected bool) *sessionFixture {
	t.Helper()
	f := newApprovalFixture(t, protected, nil)
	ctx := context.Background()
	op, _, err := f.s.Admit(ctx, state.Admission{RequestID: "seed-web", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	release := &state.Release{App: "web", Manifest: json.RawMessage(`{"project":"shop","network":"berthy","apps":{"web":{"kind":"compose","file":"compose.yaml"}}}`)}
	if err = f.s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: release}); err != nil {
		t.Fatal(err)
	}
	if err = f.s.PutSecret(ctx, state.Actor{ID: "a"}, state.SecretInfo{Name: "TOKEN"}, []byte("s3cret")); err != nil {
		t.Fatal(err)
	}
	docker := newSessionDocker()
	f.ctrl.Engine = docker
	// A session outlives the test unless it is closed: its handler must be done before the store closes.
	var handlers sync.WaitGroup
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		f.h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		f.ctrl.CloseSessions(context.Background())
		handlers.Wait()
	})
	return &sessionFixture{approvalFixture: f, docker: docker, srv: srv}
}

// open sends a session request and returns the connection after the answer, and the answer.
func (f *sessionFixture) open(token string, headers map[string]string, body string) (net.Conn, *bufio.Reader, *http.Response) {
	f.t.Helper()
	conn, err := net.Dial("tcp", f.srv.Listener.Addr().String())
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { conn.Close() })
	req, err := http.NewRequest("POST", f.srv.URL+"/v1/projects/demo/environments/lab/sessions", strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if headers == nil {
		headers = map[string]string{"Connection": "Upgrade", "Upgrade": session.Upgrade}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if err = req.Write(conn); err != nil {
		f.t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		f.t.Fatal(err)
	}
	return conn, br, resp
}

const sessionBody = `{"kind":"exec","app":"web","command":["sh","-c","echo s3cret"],"tty":true}`

func readFrame(t *testing.T, br *bufio.Reader) (session.Kind, string) {
	t.Helper()
	k, p, err := session.ReadFrame(br)
	if err != nil {
		t.Fatal(err)
	}
	return k, string(p)
}

func (f *sessionFixture) auditLines() []string {
	var out []string
	for _, line := range f.audit() {
		if strings.Contains(line, " session ") {
			out = append(out, line)
		}
	}
	return out
}

func TestSessionStreamsStdinStdoutAndExit(t *testing.T) {
	f := newSessionFixture(t, false)
	f.docker.exit = 7
	conn, br, resp := f.open(apAdminToken, nil, sessionBody)
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Upgrade") != session.Upgrade {
		t.Fatalf("answer %d %v", resp.StatusCode, resp.Header)
	}
	if err := session.WriteFrame(conn, session.Resize, session.Size{Rows: 30, Cols: 100}.Encode()); err != nil {
		t.Fatal(err)
	}
	if err := session.WriteFrame(conn, session.Stdin, []byte("hi\n")); err != nil {
		t.Fatal(err)
	}
	if k, p := readFrame(t, br); k != session.Stdout || p != "got:hi\n" {
		t.Fatalf("frame %d %q", k, p)
	}
	if err := session.WriteFrame(conn, session.CloseStdin, nil); err != nil {
		t.Fatal(err)
	}
	if k, p := readFrame(t, br); k != session.Exit || p != `{"code":7}` {
		t.Fatalf("frame %d %q", k, p)
	}
	if _, _, err := session.ReadFrame(br); err != io.EOF {
		t.Fatalf("after Exit: %v", err)
	}
	waitFor(t, func() bool { return len(f.auditLines()) == 2 })
	lines := f.auditLines()
	if lines[0] != "alice session exec web started: sh -c echo [redacted]" || lines[1] != "alice session exec web ended: exit 7" {
		t.Fatalf("audit %q", lines)
	}
	for _, l := range f.audit() {
		if strings.Contains(l, "got:") || strings.Contains(l, "hi") || strings.Contains(l, "s3cret") {
			t.Errorf("the audit log holds input, output or a secret: %q", l)
		}
	}
	f.noOperation()
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestSessionWithoutATerminalKeepsStdoutAndStderrApart(t *testing.T) {
	f := newSessionFixture(t, false)
	conn, br, resp := f.open(apAdminToken, nil, `{"kind":"exec","app":"web","service":"web","command":["cat"]}`)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("answer %d", resp.StatusCode)
	}
	session.WriteFrame(conn, session.Stdin, []byte("x"))
	var out, errout bytes.Buffer
	for out.Len() < 5 || errout.Len() < 4 {
		k, p := readFrame(t, br)
		switch k {
		case session.Stdout:
			out.Write([]byte(p))
		case session.Stderr:
			errout.Write([]byte(p))
		default:
			t.Fatalf("frame %d", k)
		}
	}
	if out.String() != "got:x" || errout.String() != "warn" {
		t.Fatalf("stdout %q stderr %q", out.String(), errout.String())
	}
	session.WriteFrame(conn, session.CloseStdin, nil)
	if k, p := readFrame(t, br); k != session.Exit || p != `{"code":0}` {
		t.Fatalf("frame %d %q", k, p)
	}
}

func TestSessionNeedsAnAdministratorToken(t *testing.T) {
	f := newSessionFixture(t, false)
	for _, token := range []string{apReadToken, apDeployToken, apAgentDeploy, "bty.demo.lab." + strings.Repeat("9", 64)} {
		_, _, resp := f.open(token, nil, sessionBody)
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %s...: %d", token[:12], resp.StatusCode)
		}
	}
	if len(f.docker.startedList()) != 0 {
		t.Error("a command was started")
	}
}

func TestSessionIsRefusedToAnAgentTokenOnAProtectedEnvironment(t *testing.T) {
	f := newSessionFixture(t, true)
	_, _, resp := f.open(apAgentAdmin, nil, sessionBody)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "an interactive session cannot be held for approval; use a person's token") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if len(f.docker.startedList()) != 0 || len(f.auditLines()) != 0 {
		t.Errorf("started %v audit %v", f.docker.startedList(), f.auditLines())
	}
	f.noOperation()
	if list := f.call("GET", "/approvals", apReadToken, ""); strings.TrimSpace(list.Body.String()) != "[]" {
		t.Errorf("the session was held: %s", list.Body.String())
	}
	// A person's token opens one on the same environment.
	if _, _, resp = f.open(apAdminToken, nil, sessionBody); resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("a person: %d", resp.StatusCode)
	}
}

func TestSessionIsOpenToAnAgentTokenOnAnUnprotectedEnvironment(t *testing.T) {
	f := newSessionFixture(t, false)
	if _, _, resp := f.open(apAgentAdmin, nil, sessionBody); resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("%d", resp.StatusCode)
	}
}

func TestSessionRefusesARequestThatIsNotAnUpgradeOrNotValid(t *testing.T) {
	f := newSessionFixture(t, false)
	for name, tc := range map[string]struct {
		headers map[string]string
		body    string
		status  int
		message string
	}{
		"no upgrade":       {map[string]string{}, sessionBody, 400, "session requires an upgrade"},
		"wrong upgrade":    {map[string]string{"Connection": "Upgrade", "Upgrade": "websocket"}, sessionBody, 400, "session requires an upgrade"},
		"bad json":         {nil, `{"kind":`, 400, "invalid request"},
		"unknown field":    {nil, `{"kind":"exec","app":"web","command":["sh"],"x":1}`, 400, "invalid request"},
		"repeated app":     {nil, `{"kind":"exec","app":"web","app":"web","command":["sh"]}`, 400, "invalid request"},
		"case variant app": {nil, `{"kind":"exec","App":"web","command":["sh"]}`, 400, "invalid request"},
		"no command":       {nil, `{"kind":"exec","app":"web","command":[]}`, 400, "command is required"},
		"unknown app":      {nil, `{"kind":"exec","app":"nope","command":["sh"]}`, 400, "exec target unavailable"},
		"run of compose":   {nil, `{"kind":"run","app":"web","command":["sh"]}`, 400, "run is for image apps; use exec --service"},
	} {
		t.Run(name, func(t *testing.T) {
			conn, _, resp := f.open(apAdminToken, tc.headers, tc.body)
			if resp.StatusCode == http.StatusSwitchingProtocols {
				conn.Close()
				t.Fatalf("unexpected upgrade: %d", resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.status || !strings.Contains(string(body), tc.message) {
				t.Fatalf("%d %s", resp.StatusCode, body)
			}
		})
	}
	if len(f.auditLines()) != 0 || len(f.docker.startedList()) != 0 {
		t.Errorf("audit %v started %v", f.auditLines(), f.docker.startedList())
	}
}

func TestSeventeenthSessionIsTurnedAway(t *testing.T) {
	f := newSessionFixture(t, false)
	var conns []net.Conn
	for i := 0; i < 16; i++ {
		conn, _, resp := f.open(apAdminToken, nil, sessionBody)
		if resp.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("session %d: %d", i, resp.StatusCode)
		}
		conns = append(conns, conn)
	}
	_, _, resp := f.open(apAdminToken, nil, sessionBody)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(body), "too many interactive sessions") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	conns[0].Close()
	waitFor(t, func() bool {
		_, _, resp := f.open(apAdminToken, nil, sessionBody)
		return resp.StatusCode == http.StatusSwitchingProtocols
	})
}

func TestClientDisconnectEndsTheSession(t *testing.T) {
	f := newSessionFixture(t, false)
	conn, br, resp := f.open(apAdminToken, nil, sessionBody)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("%d", resp.StatusCode)
	}
	session.WriteFrame(conn, session.Stdin, []byte("a"))
	readFrame(t, br)
	conn.Close()
	select {
	case <-f.docker.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the command's stream stayed open")
	}
	waitFor(t, func() bool { return len(f.auditLines()) == 2 })
	if got := f.auditLines()[1]; got != "alice session exec web ended: client disconnected" {
		t.Fatalf("audit %q", got)
	}
}

func TestShutdownClosesOpenSessions(t *testing.T) {
	f := newSessionFixture(t, false)
	conn, br, resp := f.open(apAdminToken, nil, sessionBody)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("%d", resp.StatusCode)
	}
	_ = conn
	f.ctrl.CloseSessions(context.Background())
	if k, p := readFrame(t, br); k != session.Failure || p != "the agent is stopping" {
		t.Fatalf("frame %d %q", k, p)
	}
	waitFor(t, func() bool { return len(f.auditLines()) == 2 })
	if got := f.auditLines()[1]; got != "alice session exec web ended: the agent is stopping" {
		t.Fatalf("audit %q", got)
	}
}

func TestRunSessionStreamsAndRemovesItsContainer(t *testing.T) {
	f := newSessionFixture(t, false)
	seedRunApp(t, f.s)
	f.docker.exit = 4
	conn, br, resp := f.open(apAdminToken, nil, `{"kind":"run","app":"api","role":"web","command":["rails","console"],"tty":true,"size":{"Rows":24,"Cols":80}}`)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	session.WriteFrame(conn, session.Stdin, []byte("1+1\n"))
	if k, p := readFrame(t, br); k != session.Stdout || p != "got:1+1\n" {
		t.Fatalf("frame %d %q", k, p)
	}
	session.WriteFrame(conn, session.CloseStdin, nil)
	if k, p := readFrame(t, br); k != session.Exit || p != `{"code":4}` {
		t.Fatalf("frame %d %q", k, p)
	}
	select {
	case id := <-f.docker.removed:
		if id != "run-ctr" {
			t.Errorf("removed %s", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the container was not removed")
	}
	waitFor(t, func() bool { return len(f.auditLines()) == 2 })
	lines := f.auditLines()
	if lines[0] != "alice session run api/web started: rails console" || lines[1] != "alice session run api/web ended: exit 4" {
		t.Fatalf("audit %q", lines)
	}
}

func TestRunSessionDisconnectRemovesItsContainer(t *testing.T) {
	f := newSessionFixture(t, false)
	seedRunApp(t, f.s)
	conn, _, resp := f.open(apAdminToken, nil, `{"kind":"run","app":"api","command":["sleep","300"]}`)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("%d", resp.StatusCode)
	}
	conn.Close()
	select {
	case <-f.docker.removed:
	case <-time.After(5 * time.Second):
		t.Fatal("the container was not removed")
	}
}
