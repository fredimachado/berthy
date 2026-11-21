package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestConfigValidateOffline(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte("project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"), 0600)
	var out, errs bytes.Buffer
	if code := Run([]string{"config", "validate", "-e", "dev", "--manifest-dir", dir}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
}
func TestDeployDryRunSendsManifest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/projects/demo/environments/dev/deploys" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("bad request %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"app":"api","roles":["web"]}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte("project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"), 0600)
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte(fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, server.URL)), 0600)
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	var out, errs bytes.Buffer
	if code := Run([]string{"deploy", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", "v1", "--dry-run"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
}

func TestDeployWaitsForTerminalAndRejectsExternalEvents(t *testing.T) {
	for _, tc := range []struct {
		name, events, state string
		want                int
	}{{"success", "/v1/projects/demo/environments/dev/operations/op1/events", "succeeded", 0}, {"rollback", "/v1/projects/demo/environments/dev/operations/op1/events", "rolled_back", 1}, {"external", "https://evil.example/events", "succeeded", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/projects/demo/environments/dev/deploys":
					w.WriteHeader(202)
					fmt.Fprintf(w, `{"operation_id":"op1","events_url":%q}`, tc.events)
				case "/v1/projects/demo/environments/dev/operations/op1/events":
					fmt.Fprint(w, "id: 1\nevent: progress\ndata: {}\n\n")
				case "/v1/projects/demo/environments/dev/operations/op1":
					fmt.Fprintf(w, `{"state":%q}`, tc.state)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			}))
			defer server.Close()
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte("project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"), 0600)
			cfg := filepath.Join(dir, "config.json")
			os.WriteFile(cfg, []byte(fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, server.URL)), 0600)
			t.Setenv("BERTHY_TEST_TOKEN", "test-token")
			var out, errs bytes.Buffer
			got := Run([]string{"deploy", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", "v1"}, &out, &errs)
			if got != tc.want {
				t.Fatalf("exit %d: %s", got, errs.String())
			}
		})
	}
}

func TestDeployJSONOutputIsSingleDocument(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/projects/demo/environments/dev/deploys":
			w.WriteHeader(202)
			fmt.Fprint(w, `{"operation_id":"op1","events_url":"/v1/projects/demo/environments/dev/operations/op1/events"}`)
		case "/v1/projects/demo/environments/dev/operations/op1/events":
			fmt.Fprint(w, "id: 1\nevent: accepted\ndata: {}\n\nid: 2\nevent: succeeded\ndata: {}\n\n")
		case "/v1/projects/demo/environments/dev/operations/op1":
			fmt.Fprint(w, `{"state":"succeeded"}`)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte("project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"), 0600)
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte(fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, server.URL)), 0600)
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	var out, errs bytes.Buffer
	got := Run([]string{"deploy", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", "v1", "--json"}, &out, &errs)
	if got != 0 || !json.Valid(out.Bytes()) {
		t.Fatalf("exit %d, invalid JSON output %q, error %s", got, out.String(), errs.String())
	}
}

// A deploy shows what its checks found as lines of text, and --json carries them as "checks" in its one document.
func TestDeployShowsWhatItsChecksFound(t *testing.T) {
	const warning = "role web: healthy, but port 8080 does not answer on the network; is the app listening on 127.0.0.1?"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/projects/demo/environments/dev/deploys":
			w.WriteHeader(202)
			fmt.Fprint(w, `{"operation_id":"op1","events_url":"/v1/projects/demo/environments/dev/operations/op1/events"}`)
		case "/v1/projects/demo/environments/dev/operations/op1/events":
			fmt.Fprint(w, "id: 1\nevent: accepted\ndata: {}\n\nid: 2\nevent: verify\ndata: {\"role\":\"web\",\"check\":\"network\",\"status\":\"warn\",\"step\":\""+warning+"\"}\n\nid: 3\nevent: succeeded\ndata: {}\n\n")
		case "/v1/projects/demo/environments/dev/operations/op1":
			fmt.Fprint(w, `{"state":"succeeded"}`)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte("project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"), 0600)
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte(fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, server.URL)), 0600)
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	args := []string{"deploy", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", "v1"}
	var out, errs bytes.Buffer
	if got := Run(args, &out, &errs); got != 0 || !strings.Contains(out.String(), "\n  "+warning+"\n") || strings.Contains(out.String(), "verify") {
		t.Fatalf("exit %d, output %q, error %s", got, out.String(), errs.String())
	}
	out.Reset()
	if got := Run(append(args, "--json"), &out, &errs); got != 0 {
		t.Fatalf("exit %d, error %s", got, errs.String())
	}
	var doc struct {
		State  string `json:"state"`
		Checks []struct {
			Role, Check, Status, Step string
		} `json:"checks"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil || doc.State != "succeeded" || len(doc.Checks) != 1 || doc.Checks[0].Step != warning || doc.Checks[0].Status != "warn" {
		t.Fatalf("json output %q: %v %+v", out.String(), err, doc)
	}
}

func TestScaleDoesNotLoadCheckoutManifest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/demo/environments/dev/scale" {
			t.Errorf("path %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte(fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, server.URL)), 0600)
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	var out, errs bytes.Buffer
	got := Run([]string{"scale", "api", "-e", "dev", "--config", cfg, "--manifest-dir", filepath.Join(dir, "missing"), "--replicas", "2", "--dry-run"}, &out, &errs)
	if got != 0 {
		t.Fatalf("exit %d: %s", got, errs.String())
	}
}

func TestScaleSendsZeroReplicas(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/projects/demo/environments/dev/scale" {
			t.Errorf("path %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("body %q is not JSON: %v", raw, err)
		}
		if got, ok := body["replicas"]; !ok || got != float64(0) {
			t.Errorf("body %q, want replicas 0", raw)
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte(fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, server.URL)), 0600)
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	var out, errs bytes.Buffer
	got := Run([]string{"scale", "api", "-e", "dev", "--config", cfg, "--manifest-dir", filepath.Join(dir, "missing"), "--replicas", "0", "--dry-run"}, &out, &errs)
	if got != 0 {
		t.Fatalf("exit %d: %s", got, errs.String())
	}
	if calls.Load() == 0 {
		t.Fatal("no request reached the server")
	}
}

func TestScaleRefusesNegativeReplicasBeforeAnyRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request sent for an invalid replica count: %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte(fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, server.URL)), 0600)
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	cases := map[string]string{
		"-1": "invalid --replicas: must be 0 or more",
		"x":  "invalid --replicas",
	}
	for replicas, want := range cases {
		var out, errs bytes.Buffer
		got := Run([]string{"scale", "api", "-e", "dev", "--config", cfg, "--manifest-dir", filepath.Join(dir, "missing"), "--replicas", replicas}, &out, &errs)
		if got != 2 || strings.TrimSpace(errs.String()) != want {
			t.Fatalf("--replicas %s: exit %d, message %q, want exit 2 and %q", replicas, got, strings.TrimSpace(errs.String()), want)
		}
	}
}

// refusingAgent answers every request with status 400 and body, and returns a CLI config that points at it.
func refusingAgent(t *testing.T, body string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, server.URL)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	return cfg
}

func TestAgentErrorMessageReachesOperator(t *testing.T) {
	cfg := refusingAgent(t, `{"code":"operation_failed","message":"a newer release followed the migration; use --to"}`)
	want := "agent returned HTTP 400: a newer release followed the migration; use --to\n"
	var out, errs bytes.Buffer
	if code := Run([]string{"rollback", "api", "-e", "dev", "--config", cfg, "--revert-schema"}, &out, &errs); code != 1 || errs.String() != want {
		t.Fatalf("deploy path: exit %d: %q", code, errs.String())
	}
	errs.Reset()
	if code := Run([]string{"ps", "-e", "dev", "--config", cfg}, &out, &errs); code != 1 || errs.String() != want {
		t.Fatalf("read path: exit %d: %q", code, errs.String())
	}
}

func TestAgentErrorDoesNotEchoControlCharacters(t *testing.T) {
	cfg := refusingAgent(t, `{"code":"operation_failed","message":"bell\u0007here"}`)
	var out, errs bytes.Buffer
	if code := Run([]string{"ps", "-e", "dev", "--config", cfg}, &out, &errs); code != 1 || errs.String() != "agent returned HTTP 400\n" {
		t.Fatalf("exit %d: %q", code, errs.String())
	}
}

func TestRecoveryResolveNeedsReason(t *testing.T) {
	cfg := refusingAgent(t, `{"code":"operation_failed","message":"unexpected"}`)
	var out, errs bytes.Buffer
	if code := Run([]string{"recovery", "resolve", "op1", "-e", "dev", "--config", cfg}, &out, &errs); code != 2 || errs.String() != "recovery resolve requires -m\n" {
		t.Fatalf("without -m: exit %d: %q", code, errs.String())
	}
	errs.Reset()
	if code := Run([]string{"recovery", "status", "-e", "dev", "--config", cfg}, &out, &errs); code != 2 {
		t.Fatalf("unknown subcommand: exit %d: %q", code, errs.String())
	}
}

func TestRecoveryResolvePostsReason(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		extra      []string
	}{
		{"resolve", `{"dry_run":false,"reason":"service deleted outside berthy"}`, nil},
		{"dry run", `{"dry_run":true,"reason":"service deleted outside berthy"}`, []string{"--dry-run"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.Method != "POST" || r.URL.Path != "/v1/projects/demo/environments/dev/operations/OPID/resolve" || r.Header.Get("Authorization") != "Bearer test-token" || string(body) != tc.body {
					t.Errorf("bad request %s %s %s", r.Method, r.URL.Path, body)
				}
				fmt.Fprint(w, `{"operation_id":"OPID","resolved":false}`)
			}))
			defer server.Close()
			cfg := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, server.URL)), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("BERTHY_TEST_TOKEN", "test-token")
			args := append([]string{"recovery", "resolve", "OPID", "-e", "dev", "--config", cfg, "-m", "service deleted outside berthy"}, tc.extra...)
			var out, errs bytes.Buffer
			if code := Run(args, &out, &errs); code != 0 {
				t.Fatalf("exit %d: %s", code, errs.String())
			}
		})
	}
}

// TestHistoryShowsTheTagOfAReleaseOnlyWhenThereIsOne: the tag a release was deployed with is a line of its history row, both as
// text and as JSON, and a release with no tag has none.
func TestHistoryShowsTheTagOfAReleaseOnlyWhenThereIsOne(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":3,"app":"api","digest":"sha256:111c870af40e","commit":"c3","created":"2026-11-16T12:00:00Z","tag":"v1.0.2"},{"id":2,"app":"api","digest":"sha256:0123456789ab","commit":"c2","created":"2026-11-15T12:00:00Z"}]`)
	}))
	defer server.Close()
	config := diagnosticsConfig(t, server.URL)
	var out, errs bytes.Buffer
	if code := Run([]string{"history", "api", "-e", "dev", "--config", config}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	if !strings.Contains(out.String(), "tag: v1.0.2") || strings.Count(out.String(), "tag:") != 1 {
		t.Fatalf("text history lacks the tag of release 3 or shows one for release 2:\n%s", out.String())
	}
	out.Reset()
	if code := Run([]string{"history", "api", "-e", "dev", "--config", config, "--json"}, &out, &errs); code != 0 {
		t.Fatalf("json exit %d: %s", code, errs.String())
	}
	if strings.Count(out.String(), `"tag"`) != 1 || !strings.Contains(out.String(), `"v1.0.2"`) {
		t.Fatalf("json history lacks the tag of release 3 or shows one for release 2:\n%s", out.String())
	}
}

// TestExecShowsWhyItFailedBeforeItRan: an exec that fails before its command starts has no output. The CLI shows the operation's
// record, with the reason in it, and exits 1, rather than only saying the output is unavailable.
func TestExecShowsWhyItFailedBeforeItRan(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/projects/demo/environments/dev/exec":
			w.WriteHeader(202)
			fmt.Fprint(w, `{"operation_id":"op1","events_url":"/v1/projects/demo/environments/dev/operations/op1/events"}`)
		case "/v1/projects/demo/environments/dev/operations/op1/events":
			fmt.Fprint(w, "id: 1\nevent: accepted\ndata: {}\n\n")
		case "/v1/projects/demo/environments/dev/operations/op1":
			fmt.Fprint(w, `{"id":"op1","state":"failed","kind":"exec","app":"db","error":"the app has several services: name one with --service"}`)
		case "/v1/projects/demo/environments/dev/operations/op1/output":
			w.WriteHeader(404)
			fmt.Fprint(w, `{"code":"not_found","message":"output unavailable"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte(fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, server.URL)), 0600)
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	var out, errs bytes.Buffer
	got := Run([]string{"exec", "db", "-e", "dev", "--config", cfg, "--", "true"}, &out, &errs)
	if got != 1 || !strings.Contains(out.String(), "name one with --service") || strings.Contains(errs.String(), "exec output unavailable") {
		t.Fatalf("exit %d, out %q, err %q", got, out.String(), errs.String())
	}
}
