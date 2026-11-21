package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/session"
)

// sessionAgent is an agent that serves one interactive session: it records the request and the frames the client sends, and
// plays script on the connection.
type sessionAgent struct {
	*httptest.Server
	mu      sync.Mutex
	request session.Request
	stdin   bytes.Buffer
	closed  bool // the client sent CloseStdin
	script  func(conn io.Writer)
	status  int // a refusal's status instead of a session
	body    string
	calls   map[string]int
}

func newSessionAgent(t *testing.T, script func(conn io.Writer)) *sessionAgent {
	t.Helper()
	a := &sessionAgent{script: script, calls: map[string]int{}}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.calls[r.URL.Path]++
		a.mu.Unlock()
		if r.URL.Path != "/v1/projects/demo/environments/dev/sessions" {
			w.Write([]byte(`{"app":"web","image":"x","roles":["main"]}`))
			return
		}
		if a.status != 0 {
			w.WriteHeader(a.status)
			w.Write([]byte(a.body))
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&a.request); err != nil {
			t.Errorf("session request: %v", err)
		}
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + session.Upgrade + "\r\n\r\n"))
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				k, p, err := session.ReadFrame(brw.Reader)
				if err != nil {
					return
				}
				a.mu.Lock()
				switch k {
				case session.Stdin:
					a.stdin.Write(p)
				case session.CloseStdin:
					a.closed = true
				}
				a.mu.Unlock()
			}
		}()
		a.script(conn)
		// session.End is how the real agent ends a session, so the fake cannot drift from it.
		session.End(conn, done)
	}))
	t.Cleanup(a.Close)
	return a
}

// waitForInputEnd blocks until the client has sent CloseStdin.
func (a *sessionAgent) waitForInputEnd() {
	for i := 0; i < 500; i++ {
		a.mu.Lock()
		done := a.closed
		a.mu.Unlock()
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func frame(k session.Kind, payload string) func(io.Writer) {
	return func(w io.Writer) { session.WriteFrame(w, k, []byte(payload)) }
}

func play(steps ...func(io.Writer)) func(io.Writer) {
	return func(w io.Writer) {
		for _, s := range steps {
			s(w)
		}
	}
}

func TestExecInteractiveSendsASessionRequestAndExitsWithTheCommandsCode(t *testing.T) {
	a := newSessionAgent(t, play(frame(session.Stdout, "hello\n"), frame(session.Stderr, "warn\n"), frame(session.Exit, `{"code":7}`)))
	cfg := ingressCLIConfig(t, a.URL)
	var out, errout bytes.Buffer
	code := Run([]string{"exec", "web", "-i", "--role", "main", "-e", "dev", "--config", cfg, "--", "sh", "-c", "exit 7"}, &out, &errout)
	if code != 7 || out.String() != "hello\n" || errout.String() != "warn\n" {
		t.Fatalf("exit %d out %q err %q", code, out.String(), errout.String())
	}
	r := a.request
	if r.Kind != "exec" || r.App != "web" || r.Role != "main" || strings.Join(r.Command, " ") != "sh -c exit 7" || r.TTY || r.Size != nil || r.Timeout != "" {
		t.Fatalf("request %+v", r)
	}
	if a.calls["/v1/projects/demo/environments/dev/exec"] != 0 {
		t.Error("an operation was asked for")
	}
}

func TestRunInteractiveSendsTheTimeoutAndTheLongFlag(t *testing.T) {
	a := newSessionAgent(t, frame(session.Exit, `{"code":0}`))
	cfg := ingressCLIConfig(t, a.URL)
	var out, errout bytes.Buffer
	code := Run([]string{"run", "web", "--interactive", "--timeout", "30m", "-e", "dev", "--config", cfg, "--", "rails", "console"}, &out, &errout)
	if code != 0 {
		t.Fatalf("exit %d err %q", code, errout.String())
	}
	r := a.request
	if r.Kind != "run" || r.App != "web" || r.Timeout != "30m" || strings.Join(r.Command, " ") != "rails console" || r.Service != "" {
		t.Fatalf("request %+v", r)
	}
}

func TestExecInteractiveSendsTheService(t *testing.T) {
	a := newSessionAgent(t, frame(session.Exit, `{"code":0}`))
	cfg := ingressCLIConfig(t, a.URL)
	var out, errout bytes.Buffer
	if code := Run([]string{"exec", "db", "--service", "postgres", "-i", "-e", "dev", "--config", cfg, "--", "psql"}, &out, &errout); code != 0 {
		t.Fatalf("exit %d err %q", code, errout.String())
	}
	if a.request.Service != "postgres" || a.request.App != "db" {
		t.Fatalf("request %+v", a.request)
	}
}

func TestInteractiveFailureFrameIsPrintedAndExitsOne(t *testing.T) {
	a := newSessionAgent(t, frame(session.Failure, "run timed out after 1s"))
	cfg := ingressCLIConfig(t, a.URL)
	var out, errout bytes.Buffer
	if code := Run([]string{"run", "web", "-i", "-e", "dev", "--config", cfg, "--", "sleep", "9"}, &out, &errout); code != 1 || out.Len() != 0 || strings.TrimSpace(errout.String()) != "run timed out after 1s" {
		t.Fatalf("exit %d out %q err %q", code, out.String(), errout.String())
	}
}

func TestInteractiveLostConnectionIsReported(t *testing.T) {
	a := newSessionAgent(t, frame(session.Stdout, "partial"))
	cfg := ingressCLIConfig(t, a.URL)
	var out, errout bytes.Buffer
	if code := Run([]string{"exec", "web", "-i", "-e", "dev", "--config", cfg, "--", "sh"}, &out, &errout); code != 1 || out.String() != "partial" || strings.TrimSpace(errout.String()) != "the session ended unexpectedly" {
		t.Fatalf("exit %d out %q err %q", code, out.String(), errout.String())
	}
}

func TestInteractiveRefusalShowsTheAgentsMessage(t *testing.T) {
	a := newSessionAgent(t, nil)
	a.status, a.body = 403, `{"code":"forbidden","message":"an interactive session cannot be held for approval; use a person's token"}`
	cfg := ingressCLIConfig(t, a.URL)
	var out, errout bytes.Buffer
	if code := Run([]string{"exec", "web", "-i", "-e", "dev", "--config", cfg, "--", "sh"}, &out, &errout); code != 1 || strings.TrimSpace(errout.String()) != "agent returned HTTP 403: an interactive session cannot be held for approval; use a person's token" {
		t.Fatalf("exit %d err %q", code, errout.String())
	}
}

func TestInteractiveDryRunIsTheOrdinaryPlan(t *testing.T) {
	a := newSessionAgent(t, nil)
	cfg := ingressCLIConfig(t, a.URL)
	var out, errout bytes.Buffer
	code := Run([]string{"exec", "web", "-i", "--dry-run", "-e", "dev", "--config", cfg, "--", "sh"}, &out, &errout)
	if code != 0 || a.calls["/v1/projects/demo/environments/dev/exec"] != 1 || a.calls["/v1/projects/demo/environments/dev/sessions"] != 0 {
		t.Fatalf("exit %d err %q calls %v", code, errout.String(), a.calls)
	}
}

func TestInteractiveFlagBelongsToExecAndRunOnly(t *testing.T) {
	a := newSessionAgent(t, nil)
	cfg := ingressCLIConfig(t, a.URL)
	var out, errout bytes.Buffer
	if code := Run([]string{"restart", "web", "-i", "-e", "dev", "--config", cfg}, &out, &errout); code != 2 || !strings.Contains(errout.String(), "-i") {
		t.Fatalf("exit %d err %q", code, errout.String())
	}
	if len(a.calls) != 0 {
		t.Errorf("the agent was called: %v", a.calls)
	}
}

func TestInteractiveStillNeedsACommand(t *testing.T) {
	a := newSessionAgent(t, nil)
	cfg := ingressCLIConfig(t, a.URL)
	var out, errout bytes.Buffer
	if code := Run([]string{"exec", "web", "-i", "-e", "dev", "--config", cfg}, &out, &errout); code != 2 || strings.TrimSpace(errout.String()) != "exec requires a command after --" {
		t.Fatalf("exit %d err %q", code, errout.String())
	}
}

func TestRunInteractiveForwardsStandardInputAndEndsItAtEOF(t *testing.T) {
	var a *sessionAgent
	a = newSessionAgent(t, func(conn io.Writer) {
		a.waitForInputEnd()
		frame(session.Exit, `{"code":0}`)(conn)
	})
	u, _ := url.Parse(a.URL)
	target := &agentclient.Target{Env: "dev", Base: u, Prefix: "/v1/projects/demo/environments/dev/", Token: "t"}
	var out, errout bytes.Buffer
	code := runInteractive(context.Background(), target, session.Request{Kind: "exec", App: "web", Command: []string{"cat"}}, strings.NewReader("a dump\nof data\n"), &out, &errout)
	if code != 0 {
		t.Fatalf("exit %d err %q", code, errout.String())
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stdin.String() != "a dump\nof data\n" || !a.closed {
		t.Fatalf("stdin %q closed %v", a.stdin.String(), a.closed)
	}
}
