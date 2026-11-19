package engine

import (
	"context"
	"encoding/binary"
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

// frame is one frame of Docker's multiplexed stream: stream 1 is stdout and 2 is stderr.
func frame(stream byte, payload string) string {
	var hdr [8]byte
	hdr[0] = stream
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload)))
	return string(hdr[:]) + payload
}

// attachServer is a Docker daemon that only knows the attach call. It checks the request, switches to the raw stream the
// way Docker does and hands the hijacked connection to serve.
type attachServer struct {
	t       *testing.T
	engine  *Engine
	mu      sync.Mutex
	queries []url.Values
	headers []http.Header
}

func newAttachServer(t *testing.T, serve func(conn net.Conn, br io.Reader)) *attachServer {
	t.Helper()
	as := &attachServer{t: t}
	socket := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("no Unix sockets here: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1.44/containers/c1/attach" {
			t.Errorf("request %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		as.mu.Lock()
		as.queries = append(as.queries, r.URL.Query())
		as.headers = append(as.headers, r.Header.Clone())
		as.mu.Unlock()
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		io.WriteString(conn, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		serve(conn, brw)
		// A Windows Unix socket that is closed right after a write can lose its end of stream, so that the peer's read waits for
		// ever (about 1 in 40 tries in a plain Go client and server; a pause of a few milliseconds before the close made it
		// never happen in thousands). Linux does not need the pause.
		time.Sleep(20 * time.Millisecond)
	}))
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	as.engine = New(socket)
	return as
}

func attachCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestAttachDemultiplexesStdoutAndKeepsStderr(t *testing.T) {
	as := newAttachServer(t, func(conn net.Conn, _ io.Reader) {
		// A frame cut in the middle of its header and payload, as a socket may deliver it.
		stream := frame(1, "hello ") + frame(2, "oops") + frame(1, "") + frame(1, strings.Repeat("w", 100_000)) + frame(2, "!")
		for i := 0; i < len(stream); i += 7 {
			io.WriteString(conn, stream[i:min(i+7, len(stream))])
		}
	})
	out, in, stderr, err := as.engine.Attach(attachCtx(t), "c1", false)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if in != nil {
		t.Fatal("a stdin writer without stdin")
	}
	got, err := io.ReadAll(out)
	if err != nil || string(got) != "hello "+strings.Repeat("w", 100_000) {
		t.Fatalf("stdout %d bytes %v", len(got), err)
	}
	if stderr.String() != "oops!" {
		t.Fatalf("stderr %q", stderr.String())
	}
	if want := (url.Values{"stream": {"1"}, "stdout": {"1"}, "stderr": {"1"}}); !reflect.DeepEqual(as.queries[0], want) {
		t.Errorf("query %v", as.queries[0])
	}
	if h := as.headers[0]; !strings.EqualFold(h.Get("Upgrade"), "tcp") || !strings.Contains(strings.ToLower(h.Get("Connection")), "upgrade") {
		t.Errorf("headers %v", h)
	}
}

func TestAttachSendsStdinAndHalfClosesIt(t *testing.T) {
	as := newAttachServer(t, func(conn net.Conn, br io.Reader) {
		// Docker keeps the container's stdin open until the client's write side ends, and only then does the container
		// finish. A server that never sees the end of stdin never answers.
		b, _ := io.ReadAll(br)
		io.WriteString(conn, frame(1, "got:"+string(b)))
	})
	out, in, _, err := as.engine.Attach(attachCtx(t), "c1", true)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if in == nil {
		t.Fatal("no stdin writer")
	}
	if want := (url.Values{"stream": {"1"}, "stdout": {"1"}, "stderr": {"1"}, "stdin": {"1"}}); !reflect.DeepEqual(as.queries[0], want) {
		t.Errorf("query %v", as.queries[0])
	}
	if _, err = in.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err = in.Write([]byte("def")); err != nil {
		t.Fatal(err)
	}
	if err = in.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(out)
	if err != nil || string(got) != "got:abcdef" {
		t.Fatalf("stdout %q %v", got, err)
	}
}

func TestAttachKeepsAtMostFourKiBOfStderr(t *testing.T) {
	as := newAttachServer(t, func(conn net.Conn, _ io.Reader) {
		io.WriteString(conn, frame(2, strings.Repeat("e", 3000))+frame(2, strings.Repeat("f", 3000))+frame(1, "done"))
	})
	out, _, stderr, err := as.engine.Attach(attachCtx(t), "c1", false)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if got, err := io.ReadAll(out); err != nil || string(got) != "done" {
		t.Fatalf("stdout %q %v", got, err)
	}
	want := strings.Repeat("e", 3000) + strings.Repeat("f", 1096)
	if stderr.String() != want {
		t.Fatalf("stderr kept %d bytes, want the first 4096", len(stderr.String()))
	}
}

func TestAttachRefusesAMalformedStream(t *testing.T) {
	for name, stream := range map[string]string{
		"bad stream id":     "\x07\x00\x00\x00\x00\x00\x00\x01x",
		"reserved bytes":    "\x01\x01\x00\x00\x00\x00\x00\x01x",
		"cut header":        "\x01\x00\x00",
		"cut payload":       frame(1, "hello")[:10],
		"stdin as a stream": frame(0, "x"),
	} {
		as := newAttachServer(t, func(conn net.Conn, _ io.Reader) { io.WriteString(conn, stream) })
		out, _, _, err := as.engine.Attach(attachCtx(t), "c1", false)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err = io.ReadAll(out); err == nil {
			t.Errorf("%s: accepted", name)
		}
		out.Close()
	}
}

func TestAttachEndsCleanlyBetweenFrames(t *testing.T) {
	as := newAttachServer(t, func(conn net.Conn, _ io.Reader) { io.WriteString(conn, frame(1, "x")) })
	out, _, _, err := as.engine.Attach(attachCtx(t), "c1", false)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if got, err := io.ReadAll(out); err != nil || string(got) != "x" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestAttachStopsWhenTheContextEnds(t *testing.T) {
	release := make(chan struct{})
	as := newAttachServer(t, func(conn net.Conn, _ io.Reader) {
		io.WriteString(conn, frame(1, "first"))
		<-release
	})
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, _, _, err := as.engine.Attach(ctx, "c1", false)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	buf := make([]byte, 5)
	if _, err = io.ReadFull(out, buf); err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan error, 1)
	go func() { _, err := out.Read(buf); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("read after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the read did not end with the context")
	}
}

func TestAttachReportsADockerRefusal(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("no Unix sockets here: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"No such container: c1"}`, http.StatusNotFound)
	}))
	srv.Listener = l
	srv.Start()
	defer srv.Close()
	_, _, _, err = New(socket).Attach(attachCtx(t), "c1", false)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 404 {
		t.Fatalf("error %v", err)
	}
	if _, _, _, err = New(socket).Attach(attachCtx(t), "bad id/..", false); err == nil {
		t.Fatal("invalid identifier accepted")
	}
	if _, _, _, err = New(filepath.Join(t.TempDir(), "none.sock")).Attach(attachCtx(t), "c1", false); err == nil {
		t.Fatal("missing socket accepted")
	}
}

func TestBoundedBufferKeepsTheStartAndNeverFails(t *testing.T) {
	b := NewBoundedBuffer(5)
	for _, s := range []string{"ab", "cdefg", "hi"} {
		if n, err := b.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("write %q: %d %v", s, n, err)
		}
	}
	if b.String() != "abcde" {
		t.Fatalf("kept %q", b.String())
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); b.Write([]byte("x")); _ = b.String() }()
	}
	wg.Wait()
}

func TestCreateVolumeSendsNameAndLabels(t *testing.T) {
	var created string
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1.44/volumes/restore-1":
			return response(404, `{"message":"no such volume"}`), nil
		case "POST /v1.44/volumes/create":
			b, _ := io.ReadAll(r.Body)
			created = string(b)
			return response(201, `{"Name":"restore-1"}`), nil
		}
		t.Errorf("request %s %s", r.Method, r.URL)
		return response(500, `{}`), nil
	})
	if err := e.CreateVolume(context.Background(), "restore-1", map[string]string{"berthy.environment": "prod"}); err != nil {
		t.Fatal(err)
	}
	if created != `{"Name":"restore-1","Labels":{"berthy.environment":"prod"}}` {
		t.Fatalf("body %s", created)
	}
}

func TestCreateVolumeRefusesAVolumeThatExists(t *testing.T) {
	// Docker answers 201 for a volume that exists, and a restore must never fill someone's volume.
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" {
			t.Error("create sent for an existing volume")
		}
		return response(200, `{"Name":"data"}`), nil
	})
	err := e.CreateVolume(context.Background(), "data", nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusConflict {
		t.Fatalf("error %v", err)
	}
	if err = e.CreateVolume(context.Background(), "bad name/..", nil); err == nil {
		t.Fatal("invalid name accepted")
	}
}

func TestRemoveVolume(t *testing.T) {
	status := 204
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		if r.Method != "DELETE" || r.URL.Path != "/v1.44/volumes/restore-1" || r.URL.RawQuery != "" {
			t.Errorf("request %s %s", r.Method, r.URL)
		}
		return response(status, ``), nil
	})
	if err := e.RemoveVolume(context.Background(), "restore-1"); err != nil {
		t.Fatal(err)
	}
	status = 409
	var se *StatusError
	if err := e.RemoveVolume(context.Background(), "restore-1"); !errors.As(err, &se) || se.Code != 409 {
		t.Fatalf("in-use volume: %v", err)
	}
	if err := e.RemoveVolume(context.Background(), "bad name/.."); err == nil {
		t.Fatal("invalid name accepted")
	}
}

func TestRemoveContainerForcesTheRemovalOfARunningOne(t *testing.T) {
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		if r.Method != "DELETE" || r.URL.Path != "/v1.44/containers/c1" || r.URL.Query().Get("force") != "1" {
			t.Errorf("request %s %s", r.Method, r.URL)
		}
		return response(204, ``), nil
	})
	if err := e.RemoveContainer(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
}
