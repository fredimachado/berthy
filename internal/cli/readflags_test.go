package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// readCommand is one command that the read-only parser in Run serves, with the positional arguments it takes.
type readCommand struct {
	name string
	args []string
}

var readCommands = []readCommand{
	{"ps", nil},
	{"describe", []string{"api"}},
	{"logs", []string{"api"}},
	{"stats", nil},
	{"df", nil},
	{"doctor", nil},
	{"checks", nil},
	{"jobs", nil},
	{"agent-info", nil},
}

func TestReadCommandsRejectAnUnknownFlagBeforeCallingTheAgent(t *testing.T) {
	for _, flag := range [][]string{{"--manifest-dir", "x"}, {"-x"}, {"--tail=5"}} {
		for _, rc := range readCommands {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			args := append([]string{rc.name}, rc.args...)
			args = append(args, "-e", "dev", "--config", ingressCLIConfig(t, server.URL))
			args = append(args, flag...)
			var out, errout bytes.Buffer
			code := Run(args, &out, &errout)
			server.Close()
			if want := "unknown flag " + flag[0]; code != 2 || !strings.Contains(errout.String(), want) {
				t.Errorf("%s %v: exit %d stderr %q, want exit 2 with %q", rc.name, flag, code, errout.String(), want)
			}
			if calls.Load() != 0 {
				t.Errorf("%s %v: the agent was called %d times", rc.name, flag, calls.Load())
			}
		}
	}
}

func TestReadCommandsKeepEveryFlagTheyAccept(t *testing.T) {
	const streamDone = "event: done\ndata: {\"complete\":true}\n\n"
	for name, tc := range map[string]struct {
		cmd   string
		args  []string
		flags []string
		body  string
		query url.Values // what the request must carry
	}{
		"ps json":       {"ps", nil, []string{"--json"}, `[]`, nil},
		"describe json": {"describe", []string{"api"}, []string{"--json"}, `{}`, nil},
		"logs filters": {"logs", []string{"api"}, []string{"--tail", "5", "--grep", "x", "--since", "1h", "--role", "web", "--replica", "2"}, `[]`,
			url.Values{"tail": {"5"}, "grep": {"x"}, "since": {"1h"}, "role": {"web"}, "replica": {"2"}}},
		"logs follow": {"logs", []string{"api"}, []string{"-f"}, streamDone, url.Values{"follow": {"1"}}},
		"stats since": {"stats", nil, []string{"--since", "5m"}, `{}`, url.Values{"since": {"5m"}}},
		"stats watch": {"stats", nil, []string{"--watch"}, streamDone, url.Values{"watch": {"1"}}},
		"df":          {"df", nil, nil, `{}`, nil},
		"doctor json": {"doctor", nil, []string{"--json"}, `{"checks":[]}`, nil},
		"checks json": {"checks", nil, []string{"--json"}, `[]`, nil},
		"jobs json":   {"jobs", nil, []string{"--json"}, `[]`, nil},
		"agent-info":  {"agent-info", nil, nil, `{}`, nil},
	} {
		var query string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			query = r.URL.RawQuery
			if strings.HasPrefix(tc.body, "event:") {
				w.Header().Set("Content-Type", "text/event-stream")
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(tc.body))
		}))
		args := append([]string{tc.cmd}, tc.args...)
		args = append(args, "-e", "dev", "--config", ingressCLIConfig(t, server.URL))
		args = append(args, tc.flags...)
		var out, errout bytes.Buffer
		code := Run(args, &out, &errout)
		server.Close()
		if code != 0 || strings.Contains(errout.String(), "unknown flag") {
			t.Errorf("%s: exit %d stderr %q", name, code, errout.String())
			continue
		}
		got, err := url.ParseQuery(query)
		if err != nil {
			t.Fatal(err)
		}
		for key, want := range tc.query {
			if got.Get(key) != want[0] {
				t.Errorf("%s: request %s=%q, want %q (query %q)", name, key, got.Get(key), want[0], query)
			}
		}
	}
}
