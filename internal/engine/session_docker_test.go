package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// These tests run the session primitives against a real Docker daemon. They are skipped unless BERTHY_DOCKER_SOCKET names its
// socket (a path, or unix://path). They use the local image BERTHY_DOCKER_IMAGE, alpine:3.20 by default, and label what they
// create berthy.environment=e2e-session-test.

func dockerSessionEngine(t *testing.T) (*Engine, context.Context) {
	sock := strings.TrimPrefix(os.Getenv("BERTHY_DOCKER_SOCKET"), "unix://")
	if sock == "" {
		t.Skip("BERTHY_DOCKER_SOCKET is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return New(sock), ctx
}

// sessionContainer creates a container that is removed when the test ends. stdin makes it a console container: a terminal with
// its input open until the client ends it.
func sessionContainer(t *testing.T, e *Engine, name string, cmd []string, stdin bool) string {
	t.Helper()
	image := os.Getenv("BERTHY_DOCKER_IMAGE")
	if image == "" {
		image = "alpine:3.20"
	}
	spec := map[string]any{"Image": image, "Cmd": cmd, "Labels": map[string]string{"berthy.environment": "e2e-session-test"}}
	if stdin {
		spec["Tty"], spec["OpenStdin"], spec["StdinOnce"] = true, true, true
		spec["AttachStdin"], spec["AttachStdout"], spec["AttachStderr"] = true, true, true
	}
	b, _ := json.Marshal(spec)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id, err := e.CreateContainer(ctx, name, b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		e.RemoveContainer(ctx, id)
	})
	return id
}

func TestDockerExecSessionWithATerminal(t *testing.T) {
	e, ctx := dockerSessionEngine(t)
	id := sessionContainer(t, e, "berthy-session-test-a", []string{"sleep", "300"}, false)
	if err := e.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	s, execID, err := e.StartExecSession(ctx, id, []string{"sh"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = io.WriteString(s, "echo hello-$((40+2))\nexit 7\n"); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(s)
	t.Logf("output %q err %v", out, err)
	if !bytes.Contains(out, []byte("hello-42")) {
		t.Fatalf("output %q", out)
	}
	if code, err := e.ExecExitCode(ctx, execID); err != nil || code != 7 {
		t.Fatalf("exit code %d %v", code, err)
	}
}

func TestDockerExecSessionWithoutATerminal(t *testing.T) {
	e, ctx := dockerSessionEngine(t)
	id := sessionContainer(t, e, "berthy-session-test-b", []string{"sleep", "300"}, false)
	if err := e.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	s, execID, err := e.StartExecSession(ctx, id, []string{"sh", "-c", "cat; echo err >&2"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = io.WriteString(s, "abc"); err != nil {
		t.Fatal(err)
	}
	if err = s.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err = Demux(s, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	t.Logf("stdout %q stderr %q", stdout.String(), stderr.String())
	if stdout.String() != "abc" || stderr.String() != "err\n" {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	if code, err := e.ExecExitCode(ctx, execID); err != nil || code != 0 {
		t.Fatalf("exit code %d %v", code, err)
	}
}

func TestDockerResizeExec(t *testing.T) {
	e, ctx := dockerSessionEngine(t)
	id := sessionContainer(t, e, "berthy-session-test-c", []string{"sleep", "300"}, false)
	if err := e.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	s, execID, err := e.StartExecSession(ctx, id, []string{"sh", "-c", "sleep 1; stty size"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = e.ResizeExec(ctx, execID, 33, 77); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(s)
	t.Logf("output %q", out)
	if !bytes.Contains(out, []byte("33 77")) {
		t.Fatalf("output %q", out)
	}
}

func TestDockerAttachSessionToACreatedContainer(t *testing.T) {
	e, ctx := dockerSessionEngine(t)
	id := sessionContainer(t, e, "berthy-session-test-d", []string{"sh"}, true)
	s, err := e.AttachSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = e.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(s, "echo attached\nexit 3\n"); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(s)
	t.Logf("output %q", out)
	if !bytes.Contains(out, []byte("attached")) {
		t.Fatalf("output %q", out)
	}
	if code, err := e.WaitContainer(ctx, id); err != nil || code != 3 {
		t.Fatalf("exit code %d %v", code, err)
	}
}

func TestDockerResizeContainer(t *testing.T) {
	e, ctx := dockerSessionEngine(t)
	id := sessionContainer(t, e, "berthy-session-test-e", []string{"sh", "-c", "sleep 1; stty size"}, true)
	s, err := e.AttachSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = e.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err = e.ResizeContainer(ctx, id, 40, 100); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(s)
	t.Logf("output %q", out)
	if !bytes.Contains(out, []byte("40 100")) {
		t.Fatalf("output %q", out)
	}
}
