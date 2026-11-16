package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cannedDiagnostics = `{"operation_id":"op1","reason":"role web: task exited with code 1 (2 failed tasks)","diagnostics":[
 {"kind":"task","name":"proj-app-web","role":"web","task_id":"t2","container":"c2","state":"failed","error":"task: non-zero exit (1)","exit_code":1,"health":"unhealthy","health_log":[{"start":"2026-11-16T10:00:01Z","exit_code":1,"output":"connection refused"}],"logs":"2026-11-16T10:00:01Z stderr config: REQUIRED_SETTING is missing\n2026-11-16T10:00:02Z stdout \u001b[31mred\u001b[0m\n","truncated":true,"captured":"2026-11-16T10:00:05Z"},
 {"kind":"oneoff","name":"pre_hook","index":1,"container":"berthy-op1-10000","state":"exited","exit_code":3,"oom_killed":true,"logs":"migration 0042 failed: column exists\n","captured":"2026-11-16T10:00:06Z"}
]}`

func diagnosticsConfig(t *testing.T, url string) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}`, url)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	return cfg
}

func TestDiagnosticsCommandPrintsEvidenceAsText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/environments/dev/operations/op1/diagnostics" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, cannedDiagnostics)
	}))
	defer server.Close()
	var out, errs bytes.Buffer
	if code := Run([]string{"diagnostics", "op1", "-e", "dev", "--config", diagnosticsConfig(t, server.URL)}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	text := out.String()
	for _, want := range []string{
		"operation: op1",
		"reason: role web: task exited with code 1 (2 failed tasks)",
		"task web", "proj-app-web", "t2", "state=failed", "exit=1", "task: non-zero exit (1)", "health=unhealthy",
		"exit 1: connection refused",
		"    2026-11-16T10:00:01Z stderr config: REQUIRED_SETTING is missing",
		"logs truncated",
		"oneoff pre_hook", "exit=3", "oom=true",
		"    migration 0042 failed: column exists",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\x1b") {
		t.Errorf("terminal escape sequences from the app reached the terminal:\n%q", text)
	}
	if strings.Contains(text, "{") {
		t.Errorf("text output is JSON:\n%s", text)
	}
}

func TestDiagnosticsCommandJSONIsOneDocument(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, cannedDiagnostics) }))
	defer server.Close()
	var out, errs bytes.Buffer
	if code := Run([]string{"diagnostics", "op1", "-e", "dev", "--config", diagnosticsConfig(t, server.URL), "--json"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	var got struct {
		Diagnostics []struct {
			Logs string `json:"logs"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got.Diagnostics) != 2 || !strings.Contains(got.Diagnostics[0].Logs, "\x1b[31m") {
		t.Fatalf("output %q: %v", out.String(), err)
	}
}

func TestDiagnosticsCommandReportsAgentRefusals(t *testing.T) {
	for _, status := range []int{403, 404} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"code":"x","message":"nope"}`)
		}))
		var out, errs bytes.Buffer
		code := Run([]string{"diagnostics", "op1", "-e", "dev", "--config", diagnosticsConfig(t, server.URL)}, &out, &errs)
		server.Close()
		if code != 1 || errs.Len() == 0 || out.Len() != 0 {
			t.Fatalf("status %d: exit %d out %q err %q", status, code, out.String(), errs.String())
		}
	}
	var out, errs bytes.Buffer
	if code := Run([]string{"diagnostics", "-e", "dev", "--config", diagnosticsConfig(t, "http://127.0.0.1:1")}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "usage: diagnostics OPERATION_ID") {
		t.Fatalf("missing id: exit %d %q", code, errs.String())
	}
}

func TestFailedDeployPointsToItsDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		state, command string
		args           []string
		hint           bool
	}{
		{"failed", "deploys", nil, true},
		{"rolled_back", "deploys", nil, true},
		{"recovery_required", "deploys", nil, true},
		{"succeeded", "deploys", nil, false},
		{"failed", "deploys", []string{"--json"}, true},
	} {
		t.Run(tc.state+strings.Join(tc.args, ""), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/environments/dev/deploys":
					w.WriteHeader(202)
					fmt.Fprint(w, `{"operation_id":"op1","events_url":"/v1/environments/dev/operations/op1/events"}`)
				case "/v1/environments/dev/operations/op1/events":
					fmt.Fprint(w, "id: 1\nevent: failed\ndata: {}\n\n")
				case "/v1/environments/dev/operations/op1":
					fmt.Fprintf(w, `{"state":%q,"reason":"role web: task exited with code 1"}`, tc.state)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			}))
			defer server.Close()
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte("project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"), 0600)
			args := append([]string{"deploy", "api", "-e", "dev", "--config", diagnosticsConfig(t, server.URL), "--manifest-dir", dir, "--version", "v1"}, tc.args...)
			var out, errs bytes.Buffer
			code := Run(args, &out, &errs)
			hint := "diagnostics: berthy diagnostics op1 -e dev"
			if tc.hint != (strings.Contains(out.String(), hint) || strings.Contains(errs.String(), hint)) {
				t.Fatalf("hint expected %v:\nstdout %q\nstderr %q", tc.hint, out.String(), errs.String())
			}
			if tc.hint && code != 1 {
				t.Fatalf("exit %d", code)
			}
			if !strings.Contains(strings.Join(tc.args, ""), "--json") {
				if tc.hint && !strings.Contains(out.String(), "reason: role web: task exited with code 1") {
					t.Fatalf("reason missing: %q", out.String())
				}
			} else if !json.Valid(out.Bytes()) {
				t.Fatalf("--json stdout is not one document: %q", out.String())
			}
		})
	}
}
