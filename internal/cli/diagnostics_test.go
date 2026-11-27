package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/agent"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

const cannedDiagnostics = `{"operation_id":"op1","reason":"role web: task exited with code 1 (2 failed tasks)","diagnostics":[
 {"kind":"task","name":"proj-app-web","role":"web","task_id":"t2","container":"c2","state":"failed","error":"task: non-zero exit (1)","exit_code":1,"health":"unhealthy","health_log":[{"start":"2026-11-16T10:00:01Z","exit_code":1,"output":"connection refused"}],"logs":"2026-11-16T10:00:01Z stderr config: REQUIRED_SETTING is missing\n2026-11-16T10:00:02Z stdout \u001b[31mred\u001b[0m\n","truncated":true,"captured":"2026-11-16T10:00:05Z"},
 {"kind":"oneoff","name":"pre_hook","index":1,"container":"berthy-op1-10000","state":"exited","exit_code":3,"oom_killed":true,"logs":"migration 0042 failed: column exists\n","captured":"2026-11-16T10:00:06Z"}
]}`

func diagnosticsConfig(t *testing.T, url string) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, url)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	return cfg
}

type composeDiagnosticEngine struct{ deploy.Engine }

func (composeDiagnosticEngine) InspectNetwork(_ context.Context, name string) (engine.Network, error) {
	return engine.Network{Name: name, Driver: "overlay", Scope: "swarm", Attachable: true}, nil
}

func (composeDiagnosticEngine) ComposeContainers(context.Context, string) ([]engine.ComposeContainer, error) {
	return nil, nil
}

func (composeDiagnosticEngine) VolumeExists(context.Context, string) (bool, error) { return false, nil }

type failedDiagnosticCompose struct{ stderr string }

func (f failedDiagnosticCompose) Run(_ context.Context, r engine.ComposeRun) ([]byte, []byte, error) {
	switch r.Args[0] {
	case "config":
		if r.Args[1] == "--hash" {
			return []byte("web hash-0000000000000001\n"), nil, nil
		}
		return []byte(`{"services":{"web":{"image":"alpine:3.20"}}}`), nil, nil
	case "up":
		return []byte("stdout fixture marker\n"), []byte(f.stderr), errors.New("compose exited with status 1")
	default:
		return nil, nil, errors.New("unexpected compose command")
	}
}

func TestDiagnosticsCommandShowsFailedComposeUp(t *testing.T) {
	ctx := context.Background()
	deployToken := "bty.demo.dev." + strings.Repeat("b", 64)
	viewToken := "bty.demo.dev." + strings.Repeat("c", 64)
	t.Setenv("BERTHY_TOKEN_STORE", "none")
	for _, tc := range []struct {
		name, secret, template, stderr, want string
	}{
		{"plain", "", "", "executable missing\n", "executable missing\n"},
		{"stored secret", "fixture-password", "", "denied fixture-password\n", "denied [redacted]\n"},
		{"first deploy template", "fixture-password", "scheme://user:${SECRET}@db", "failed opening scheme://user:fixture-password@db\n", "failed opening [redacted]\n"},
		{"scoped bearer", "", "", "denied " + deployToken + "\n", "denied [redacted]\n"},
		{"overlapping secret and bearer", "registry-key:bty.", "", "denied registry-key:" + deployToken + " from registry\n", "denied [redacted] from registry\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			key := filepath.Join(dir, "key")
			if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
				t.Fatal(err)
			}
			s, err := state.Open(dir, key, "dev")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if tc.secret != "" {
				if err := s.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: "SECRET"}, []byte(tc.secret)); err != nil {
					t.Fatal(err)
				}
			}
			c := &deploy.Controller{Store: s, Project: "demo", Environment: "dev", StateDir: dir, Engine: composeDiagnosticEngine{}, Compose: failedDiagnosticCompose{tc.stderr}}
			app := manifest.App{Kind: "compose", File: "compose.yaml"}
			if tc.template != "" {
				app.Env.Template = map[string]string{"URL": tc.template}
			}
			m := manifest.Manifest{Project: "demo", Network: "demo", Apps: map[string]manifest.App{"web": app}}
			op, created, err := c.Admit(ctx, deploy.Actor{ID: agent.Digest(deployToken)}, deploy.Request{
				RequestID: "failed-compose-up", Kind: "deploy", App: "web", Manifest: m,
				ComposeFile: "services:\n  web:\n    image: alpine:3.20\n",
			})
			if drainErr := c.Drain(ctx); drainErr != nil {
				t.Fatal(drainErr)
			}
			if err != nil || !created {
				t.Fatalf("admit: created=%t, %v", created, err)
			}
			got, err := s.Operation(ctx, op.ID)
			if err != nil || got.State != "failed" || got.Err != "compose up failed" {
				t.Fatalf("operation %s %q: %v", got.State, got.Err, err)
			}
			cfg := agent.Config{Project: "demo", Environment: "dev", Tokens: []agent.Token{{Digest: agent.Digest(viewToken), Scope: "deploy"}}}
			server := httptest.NewServer(agent.NewDeploymentHandler(cfg, nil, nil, c))
			defer server.Close()
			args := []string{"diagnostics", op.ID, "-e", "dev", "--config", diagnosticsConfig(t, server.URL)}
			t.Setenv("BERTHY_TEST_TOKEN", viewToken)
			var out, errs bytes.Buffer
			if code := Run(args, &out, &errs); code != 0 {
				t.Fatalf("text diagnostics exit %d: %s", code, errs.String())
			}
			wantText := "[1] compose up\n    logs:\n    " + strings.TrimSuffix(tc.want, "\n") + "\n"
			if !strings.Contains(out.String(), wantText) || strings.Contains(out.String(), "stdout fixture marker") {
				t.Errorf("text diagnostics %q, want %q", out.String(), wantText)
			}
			out.Reset()
			errs.Reset()
			if code := Run(append(args, "--json"), &out, &errs); code != 0 {
				t.Fatalf("JSON diagnostics exit %d: %s", code, errs.String())
			}
			var report diagnosticReport
			if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.OperationID != op.ID || len(report.Diagnostics) != 1 || report.Diagnostics[0].Kind != "compose" || report.Diagnostics[0].Name != "up" || report.Diagnostics[0].Logs != tc.want {
				t.Errorf("JSON diagnostics %q: %v", out.String(), err)
			}
			items, err := s.Diagnostics(ctx, op.ID)
			if err != nil || len(items) != 1 {
				t.Fatalf("stored %d diagnostics, want 1: %v", len(items), err)
			}
			var item deploy.Diagnostic
			if err := json.Unmarshal(items[0], &item); err != nil || item.Logs != tc.want {
				t.Errorf("stored logs %q, want %q: %v", item.Logs, tc.want, err)
			}
		})
	}
}

func TestDiagnosticsCommandPrintsEvidenceAsText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/projects/demo/environments/dev/operations/op1/diagnostics" || r.Header.Get("Authorization") != "Bearer test-token" {
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
				if answerCapablePolicy(w, r) {
					return
				}
				switch r.URL.Path {
				case "/v1/projects/demo/environments/dev/deploys":
					w.WriteHeader(202)
					fmt.Fprint(w, `{"operation_id":"op1","events_url":"/v1/projects/demo/environments/dev/operations/op1/events"}`)
				case "/v1/projects/demo/environments/dev/operations/op1/events":
					fmt.Fprint(w, "id: 1\nevent: failed\ndata: {}\n\n")
				case "/v1/projects/demo/environments/dev/operations/op1":
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
			hint := "diagnostics: berthy diagnostics op1 -e dev --project demo"
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

func TestFailedDeployDiagnosticsNamesResolvedProject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answerCapablePolicy(w, r) {
			return
		}
		switch r.URL.Path {
		case "/v1/projects/demo/environments/dev/deploys":
			w.WriteHeader(202)
			fmt.Fprint(w, `{"operation_id":"op1","events_url":"/v1/projects/demo/environments/dev/operations/op1/events"}`)
		case "/v1/projects/demo/environments/dev/operations/op1/events":
			fmt.Fprint(w, "event: failed\ndata: {}\n\n")
		case "/v1/projects/demo/environments/dev/operations/op1":
			fmt.Fprint(w, `{"state":"failed"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	t.Setenv("DIAG_PROJECT_TOKEN", "token")
	cfg := filepath.Join(t.TempDir(), "config.json")
	config := fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"DIAG_PROJECT_TOKEN"}}},"other":{"environments":{"dev":{"url":%q,"token_env":"DIAG_PROJECT_TOKEN"}}}}}`, srv.URL, srv.URL)
	if err := os.WriteFile(cfg, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	dir := projectDirOf(t, "demo")
	var out, errs bytes.Buffer
	code := Run([]string{"deploy", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", "v1"}, &out, &errs)
	if code != 1 || !strings.Contains(out.String(), "berthy diagnostics op1 -e dev --project demo") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errs.String())
	}
}
