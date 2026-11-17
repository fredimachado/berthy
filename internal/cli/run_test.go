package cli

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// runAgent answers a run: it accepts it, reports the operation's end state and serves its output. body is what the agent
// received.
func runAgent(t *testing.T, state, output string, exitCode int, body *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/environments/dev/run":
			data, _ := io.ReadAll(r.Body)
			*body = string(data)
			w.WriteHeader(202)
			fmt.Fprint(w, `{"operation_id":"op1","events_url":"/v1/environments/dev/operations/op1/events"}`)
		case "/v1/environments/dev/operations/op1/events":
			fmt.Fprint(w, "id: 1\nevent: accepted\ndata: {}\n\n")
		case "/v1/environments/dev/operations/op1":
			fmt.Fprintf(w, `{"id":"op1","state":%q,"kind":"run","app":"web"}`, state)
		case "/v1/environments/dev/operations/op1/output":
			fmt.Fprintf(w, `{"output":%q,"exit_code":%d}`, output, exitCode)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
}

func TestRunSendsTheCommandRoleAndTimeoutAndShowsTheOutput(t *testing.T) {
	var body string
	server := runAgent(t, "succeeded", "hello from run\n", 0, &body)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	code := Run([]string{"run", "web", "--role", "main", "--timeout", "30m", "-e", "dev", "--config", cfg, "--", "echo", "hi"}, &out, &errout)
	if code != 0 || !strings.Contains(out.String(), "hello from run") {
		t.Fatalf("exit %d, out %q, err %q", code, out.String(), errout.String())
	}
	for _, want := range []string{`"app":"web"`, `"role":"main"`, `"timeout":"30m"`, `"command":["echo","hi"]`} {
		if !strings.Contains(body, want) {
			t.Errorf("request lacks %s: %s", want, body)
		}
	}
}

func TestRunExitsNonzeroWhenTheCommandDid(t *testing.T) {
	var body string
	server := runAgent(t, "failed", "boom\n", 3, &body)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	if code := Run([]string{"run", "web", "-e", "dev", "--config", cfg, "--", "false"}, &out, &errout); code != 1 || !strings.Contains(out.String(), "boom") {
		t.Fatalf("exit %d, out %q, err %q", code, out.String(), errout.String())
	}
	if strings.Contains(body, `"timeout"`) || strings.Contains(body, `"role"`) {
		t.Errorf("flags that were not given were sent: %s", body)
	}
}

func TestRunNeedsACommandAfterTheDashes(t *testing.T) {
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	for _, args := range [][]string{{"run", "web"}, {"run", "web", "--"}} {
		var out, errout bytes.Buffer
		code := Run(append([]string{args[0], args[1], "-e", "dev", "--config", cfg}, args[2:]...), &out, &errout)
		if code != 2 || strings.TrimSpace(errout.String()) != "run requires a command after --" || calls.Load() != 0 {
			t.Errorf("%v: exit %d, err %q, agent calls %d", args, code, errout.String(), calls.Load())
		}
	}
}

func TestRunDryRunShowsThePlanAndStartsNothing(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		body = string(data)
		fmt.Fprint(w, `{"app":"web","image":"example/web@sha256:abc","roles":["main"],"command":["echo","x"]}`)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	if code := Run([]string{"run", "web", "--dry-run", "-e", "dev", "--config", cfg, "--", "echo", "x"}, &out, &errout); code != 0 {
		t.Fatalf("exit %d, err %q", code, errout.String())
	}
	if !strings.Contains(body, `"dry_run":true`) || !strings.Contains(out.String(), "example/web@sha256:abc") {
		t.Fatalf("body %s, out %q", body, out.String())
	}
}
