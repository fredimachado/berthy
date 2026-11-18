package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// logsAgent answers "job logs" with one canned response and remembers the requests.
func logsAgent(t *testing.T, status int, body string) (*httptest.Server, *[]string) {
	t.Helper()
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func runJobLogsCLI(t *testing.T, server *httptest.Server, args ...string) (int, string, string) {
	t.Helper()
	all := append([]string{"job", "logs", "tick", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, args...)
	var out, errout bytes.Buffer
	code := Run(all, &out, &errout)
	return code, out.String(), errout.String()
}

const failedLogs = `{"job":"tick","run_id":7,"status":"failed","detail":"exit status 3","finished":"2026-11-16T10:00:01Z","output":"2026-11-16T10:00:00Z stderr boom [redacted]\n2026-11-16T10:00:01Z stdout bye\n"}`

func TestJobLogsPrintsTheOutputAndNamesTheRunApart(t *testing.T) {
	server, requests := logsAgent(t, 200, failedLogs)
	code, out, errout := runJobLogsCLI(t, server)
	if code != 0 || out != "2026-11-16T10:00:00Z stderr boom [redacted]\n2026-11-16T10:00:01Z stdout bye\n" {
		t.Fatalf("exit %d out %q err %q", code, out, errout)
	}
	if !strings.Contains(errout, "run 7 of tick failed: exit status 3") {
		t.Fatalf("stderr %q", errout)
	}
	if len(*requests) != 1 || (*requests)[0] != "GET /v1/environments/dev/jobs/tick/logs" {
		t.Fatalf("requests %v", *requests)
	}
}

func TestJobLogsOfANamedRunAsJSON(t *testing.T) {
	server, requests := logsAgent(t, 200, failedLogs)
	code, out, _ := runJobLogsCLI(t, server, "--run", "7", "--json")
	if code != 0 || !strings.Contains(out, `"run_id": 7`) || !strings.Contains(out, `boom [redacted]`) {
		t.Fatalf("exit %d out %q", code, out)
	}
	if (*requests)[0] != "GET /v1/environments/dev/jobs/tick/logs?run=7" {
		t.Fatalf("requests %v", *requests)
	}
}

func TestJobLogsOfARunWithoutOutputSaysSo(t *testing.T) {
	server, _ := logsAgent(t, 200, `{"job":"tick","run_id":8,"status":"succeeded","output":""}`)
	code, out, errout := runJobLogsCLI(t, server, "--run", "8")
	if code != 0 || out != "" || !strings.Contains(errout, "kept no output") {
		t.Fatalf("exit %d out %q err %q", code, out, errout)
	}
}

func TestJobLogsKeepsControlCharactersOffTheTerminal(t *testing.T) {
	server, _ := logsAgent(t, 200, `{"job":"tick","run_id":7,"status":"failed","output":"red \u001b[31malert\u001b[0m\n"}`)
	_, out, _ := runJobLogsCLI(t, server)
	if strings.Contains(out, "\x1b") || !strings.Contains(out, `\x1b[31malert`) {
		t.Fatalf("out %q", out)
	}
}

func TestJobLogsRefusals(t *testing.T) {
	notFound, _ := logsAgent(t, 404, `{"code":"not_found","message":"no output kept"}`)
	if code, _, errout := runJobLogsCLI(t, notFound); code != 1 || !strings.Contains(errout, "no output kept for tick") {
		t.Errorf("not found: exit %d err %q", code, errout)
	}
	forbidden, _ := logsAgent(t, 403, `{"code":"forbidden","message":"forbidden"}`)
	if code, _, errout := runJobLogsCLI(t, forbidden); code != 1 || !strings.Contains(errout, "HTTP 403") {
		t.Errorf("forbidden: exit %d err %q", code, errout)
	}
	unused, requests := logsAgent(t, 200, failedLogs)
	for _, bad := range []string{"x", "0", "-3"} {
		if code, _, errout := runJobLogsCLI(t, unused, "--run", bad); code != 2 || !strings.Contains(errout, "--run needs a run id") {
			t.Errorf("--run %s: exit %d err %q", bad, code, errout)
		}
	}
	if len(*requests) != 0 {
		t.Errorf("a bad run id reached the agent: %v", *requests)
	}
}
