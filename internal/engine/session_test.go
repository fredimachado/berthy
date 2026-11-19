package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// dockerCall is one request the fake Docker daemon received.
type dockerCall struct {
	Method, Path string
	Query        url.Values
	Header       http.Header
	Body         string
}

// fakeDocker is a Docker daemon on a Unix socket that answers by route ("METHOD /path") and records what it was asked.
type fakeDocker struct {
	t      *testing.T
	engine *Engine
	mu     sync.Mutex
	calls  []dockerCall
}

func newFakeDocker(t *testing.T, routes map[string]http.HandlerFunc) *fakeDocker {
	t.Helper()
	f := &fakeDocker{t: t}
	socket := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("no Unix sockets here: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, dockerCall{r.Method, r.URL.Path, r.URL.Query(), r.Header.Clone(), string(body)})
		f.mu.Unlock()
		h, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.Error(w, `{"message":"unexpected"}`, http.StatusNotFound)
			return
		}
		h(w, r)
	}))
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	f.engine = New(socket)
	return f
}

// call is the n-th request, counting from 0.
func (f *fakeDocker) call(n int) dockerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n >= len(f.calls) {
		f.t.Fatalf("only %d requests, wanted request %d", len(f.calls), n)
	}
	return f.calls[n]
}

func jsonReply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

// switchTo answers 101 the way Docker does and hands the raw connection to serve, which reads what the client sends from br.
func switchTo(t *testing.T, serve func(conn net.Conn, br io.Reader)) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer closeAfterPause(conn)
		io.WriteString(conn, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		serve(conn, brw)
	}
}

func echo(conn net.Conn, br io.Reader) { io.Copy(conn, br) }

func TestStartExecSessionWithATerminalSendsInputAndReadsOutput(t *testing.T) {
	d := newFakeDocker(t, map[string]http.HandlerFunc{
		"POST /v1.44/containers/c1/exec": jsonReply(201, `{"Id":"e1"}`),
		"POST /v1.44/exec/e1/start":      switchTo(t, echo),
	})
	s, id, err := d.engine.StartExecSession(attachCtx(t), "c1", []string{"sh", "-l"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if id != "e1" {
		t.Errorf("exec id %q", id)
	}
	if _, err = s.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err = io.ReadFull(s, buf); err != nil || string(buf) != "ping\n" {
		t.Fatalf("echo %q %v", buf, err)
	}

	create := d.call(0)
	var body struct {
		AttachStdin, AttachStdout, AttachStderr, Tty bool
		Cmd                                          []string
	}
	if err = json.Unmarshal([]byte(create.Body), &body); err != nil {
		t.Fatal(err)
	}
	if want := (struct {
		AttachStdin, AttachStdout, AttachStderr, Tty bool
		Cmd                                          []string
	}{true, true, true, true, []string{"sh", "-l"}}); !reflect.DeepEqual(body, want) {
		t.Errorf("create body %+v", body)
	}
	start := d.call(1)
	if start.Body != `{"Detach":false,"Tty":true}` {
		t.Errorf("start body %s", start.Body)
	}
	if h := start.Header; !strings.EqualFold(h.Get("Upgrade"), "tcp") || !strings.Contains(strings.ToLower(h.Get("Connection")), "upgrade") {
		t.Errorf("start headers %v", h)
	}
}

func TestStartExecSessionWithoutATerminalAsksForFramedOutput(t *testing.T) {
	d := newFakeDocker(t, map[string]http.HandlerFunc{
		"POST /v1.44/containers/c1/exec": jsonReply(201, `{"Id":"e1"}`),
		"POST /v1.44/exec/e1/start": switchTo(t, func(conn net.Conn, _ io.Reader) {
			io.WriteString(conn, frame(1, "out")+frame(2, "err"))
		}),
	})
	s, _, err := d.engine.StartExecSession(attachCtx(t), "c1", []string{"cat"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !strings.Contains(d.call(0).Body, `"Tty":false`) || d.call(1).Body != `{"Detach":false,"Tty":false}` {
		t.Errorf("bodies %s / %s", d.call(0).Body, d.call(1).Body)
	}
	var stdout, stderr bytes.Buffer
	if err = Demux(s, &stdout, &stderr); err != nil || stdout.String() != "out" || stderr.String() != "err" {
		t.Fatalf("demux %q %q %v", stdout.String(), stderr.String(), err)
	}
}

func TestStreamCloseWriteEndsTheInputAndKeepsTheOutput(t *testing.T) {
	d := newFakeDocker(t, map[string]http.HandlerFunc{
		"POST /v1.44/containers/c1/exec": jsonReply(201, `{"Id":"e1"}`),
		"POST /v1.44/exec/e1/start": switchTo(t, func(conn net.Conn, br io.Reader) {
			b, _ := io.ReadAll(br) // returns only when the client's write side ends
			io.WriteString(conn, "got:"+string(b))
		}),
	})
	s, _, err := d.engine.StartExecSession(attachCtx(t), "c1", []string{"cat"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err = s.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(s)
	if err != nil || string(got) != "got:abc" {
		t.Fatalf("output %q %v", got, err)
	}
}

func TestStreamCloseIsIdempotentAndEndsReads(t *testing.T) {
	d := newFakeDocker(t, map[string]http.HandlerFunc{
		"POST /v1.44/containers/c1/exec": jsonReply(201, `{"Id":"e1"}`),
		"POST /v1.44/exec/e1/start":      switchTo(t, echo),
	})
	s, _, err := d.engine.StartExecSession(attachCtx(t), "c1", []string{"cat"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err = s.Read(make([]byte, 1)); err == nil {
		t.Fatal("read after Close")
	}
}

func TestStreamEndsWithTheContext(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	d := newFakeDocker(t, map[string]http.HandlerFunc{
		"POST /v1.44/containers/c1/exec": jsonReply(201, `{"Id":"e1"}`),
		"POST /v1.44/exec/e1/start":      switchTo(t, func(net.Conn, io.Reader) { <-release }),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, _, err := d.engine.StartExecSession(ctx, "c1", []string{"sleep"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	done := make(chan error, 1)
	go func() { _, err := s.Read(make([]byte, 1)); done <- err }()
	time.Sleep(50 * time.Millisecond) // let the read block
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("read after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the read did not end with the context")
	}
	if _, err = s.Write([]byte("x")); err == nil {
		t.Fatal("write after cancel")
	}
}

func TestStartExecSessionRefusalClosesTheConnection(t *testing.T) {
	closed := make(chan struct{})
	d := newFakeDocker(t, map[string]http.HandlerFunc{
		"POST /v1.44/containers/c1/exec": jsonReply(201, `{"Id":"e1"}`),
		"POST /v1.44/exec/e1/start": func(w http.ResponseWriter, _ *http.Request) {
			conn, brw, _ := w.(http.Hijacker).Hijack()
			defer conn.Close()
			io.WriteString(conn, "HTTP/1.1 409 Conflict\r\nContent-Length: 0\r\n\r\n")
			io.Copy(io.Discard, brw) // ends when the client drops the connection
			close(closed)
		},
	})
	s, _, err := d.engine.StartExecSession(attachCtx(t), "c1", []string{"sh"}, true)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 409 || s != nil {
		t.Fatalf("stream %v error %v", s, err)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the connection stayed open")
	}
}

func TestStartExecSessionRefusesAnUpgradeThatIsNotOne(t *testing.T) {
	d := newFakeDocker(t, map[string]http.HandlerFunc{
		"POST /v1.44/containers/c1/exec": jsonReply(201, `{"Id":"e1"}`),
		"POST /v1.44/exec/e1/start":      jsonReply(200, ``),
	})
	if _, _, err := d.engine.StartExecSession(attachCtx(t), "c1", []string{"sh"}, true); err == nil {
		t.Fatal("a 200 was taken for an upgrade")
	}
}

func TestStartExecSessionStopsWhenTheExecCannotBeCreated(t *testing.T) {
	d := newFakeDocker(t, map[string]http.HandlerFunc{
		"POST /v1.44/containers/c1/exec": jsonReply(404, `{"message":"No such container"}`),
	})
	_, _, err := d.engine.StartExecSession(attachCtx(t), "c1", []string{"sh"}, true)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 404 {
		t.Fatalf("error %v", err)
	}
	e := d.engine
	for name, call := range map[string]func() error{
		"container id": func() error {
			_, _, err := e.StartExecSession(attachCtx(t), "bad id/..", []string{"sh"}, true)
			return err
		},
		"empty command": func() error { _, _, err := e.StartExecSession(attachCtx(t), "c1", nil, true); return err },
		"empty program": func() error { _, _, err := e.StartExecSession(attachCtx(t), "c1", []string{""}, true); return err },
		"no socket": func() error {
			_, _, err := New(filepath.Join(t.TempDir(), "none.sock")).StartExecSession(attachCtx(t), "c1", []string{"sh"}, true)
			return err
		},
	} {
		if call() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestResizeCallsDockerWithRowsAndColumns(t *testing.T) {
	d := newFakeDocker(t, map[string]http.HandlerFunc{
		"POST /v1.44/exec/e1/resize":       jsonReply(201, ``),
		"POST /v1.44/containers/c1/resize": jsonReply(200, ``),
	})
	if err := d.engine.ResizeExec(attachCtx(t), "e1", 24, 80); err != nil {
		t.Fatal(err)
	}
	if err := d.engine.ResizeContainer(attachCtx(t), "c1", 24, 80); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if c := d.call(i); c.Method != "POST" || c.Query.Get("h") != "24" || c.Query.Get("w") != "80" || len(c.Query) != 2 {
			t.Errorf("call %d: %s %v", i, c.Path, c.Query)
		}
	}
	if err := d.engine.ResizeExec(attachCtx(t), "bad id/..", 24, 80); err == nil {
		t.Error("invalid exec id accepted")
	}
	if err := d.engine.ResizeContainer(attachCtx(t), "bad id/..", 24, 80); err == nil {
		t.Error("invalid container id accepted")
	}
}

func TestResizeReportsADockerRefusal(t *testing.T) {
	d := newFakeDocker(t, map[string]http.HandlerFunc{"POST /v1.44/exec/e1/resize": jsonReply(404, `{"message":"no such exec"}`)})
	var se *StatusError
	if err := d.engine.ResizeExec(attachCtx(t), "e1", 24, 80); !errors.As(err, &se) || se.Code != 404 {
		t.Fatalf("error %v", err)
	}
}

func TestExecExitCode(t *testing.T) {
	for name, tc := range map[string]struct {
		reply string
		code  int
		ok    bool
	}{
		"finished":       {`{"Running":false,"ExitCode":7}`, 7, true},
		"zero":           {`{"Running":false,"ExitCode":0}`, 0, true},
		"still running":  {`{"Running":true,"ExitCode":null}`, 0, false},
		"running with 0": {`{"Running":true,"ExitCode":0}`, 0, false},
		"no exit code":   {`{"Running":false}`, 0, false},
		"not json":       {`nope`, 0, false},
	} {
		d := newFakeDocker(t, map[string]http.HandlerFunc{"GET /v1.44/exec/e1/json": jsonReply(200, tc.reply)})
		code, err := d.engine.ExecExitCode(attachCtx(t), "e1")
		if (err == nil) != tc.ok || code != tc.code {
			t.Errorf("%s: %d %v", name, code, err)
		}
	}
	if _, err := New("").ExecExitCode(attachCtx(t), "bad id/.."); err == nil {
		t.Error("invalid exec id accepted")
	}
}

func TestAttachSessionAttachesStdinStdoutAndStderr(t *testing.T) {
	d := newFakeDocker(t, map[string]http.HandlerFunc{
		"POST /v1.44/containers/c1/attach": switchTo(t, echo),
	})
	s, err := d.engine.AttachSession(attachCtx(t), "c1")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if want := (url.Values{"stream": {"1"}, "stdin": {"1"}, "stdout": {"1"}, "stderr": {"1"}}); !reflect.DeepEqual(d.call(0).Query, want) {
		t.Errorf("query %v", d.call(0).Query)
	}
	if h := d.call(0).Header; !strings.EqualFold(h.Get("Upgrade"), "tcp") || !strings.Contains(strings.ToLower(h.Get("Connection")), "upgrade") {
		t.Errorf("headers %v", h)
	}
	s.Write([]byte("hi"))
	buf := make([]byte, 2)
	if _, err = io.ReadFull(s, buf); err != nil || string(buf) != "hi" {
		t.Fatalf("echo %q %v", buf, err)
	}
	if _, err = d.engine.AttachSession(attachCtx(t), "bad id/.."); err == nil {
		t.Error("invalid container id accepted")
	}
}

func TestDemuxSplitsStdoutAndStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	stream := frame(1, "a") + frame(2, "b") + frame(1, "") + frame(1, "c") + frame(2, "d")
	if err := Demux(strings.NewReader(stream), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "ac" || stderr.String() != "bd" {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	if err := Demux(strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("an empty stream: %v", err)
	}
}

func TestDemuxReportsAMalformedStream(t *testing.T) {
	for name, stream := range map[string]string{
		"bad stream id":  "\x07\x00\x00\x00\x00\x00\x00\x01x",
		"reserved bytes": "\x01\x01\x00\x00\x00\x00\x00\x01x",
		"cut header":     "\x01\x00\x00",
		"cut payload":    frame(1, "hello")[:10],
		"stdin frame":    frame(0, "x"),
	} {
		if err := Demux(strings.NewReader(stream), io.Discard, io.Discard); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestDemuxReportsAFailedWriteAsThatWrite(t *testing.T) {
	err := Demux(strings.NewReader(frame(1, "hello")), failingWriter{}, io.Discard)
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("error %v", err)
	}
}

func TestNewStreamWrapsAConnection(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no TCP here: %v", err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer closeAfterPause(c)
		b, _ := io.ReadAll(c)
		c.Write(bytes.ToUpper(b))
	}()
	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s := NewStream(context.Background(), conn)
	defer s.Close()
	if _, err = io.WriteString(s, "abc"); err != nil {
		t.Fatal(err)
	}
	if err = s.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if out, err := io.ReadAll(s); err != nil || string(out) != "ABC" {
		t.Fatalf("output %q %v", out, err)
	}
}
