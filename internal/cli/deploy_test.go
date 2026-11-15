package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		if r.Method != "POST" || r.URL.Path != "/v1/environments/dev/deploys" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("bad request %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"app":"api","roles":["web"]}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte("project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"), 0600)
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte(fmt.Sprintf(`{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}`, server.URL)), 0600)
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
	}{{"success", "/v1/environments/dev/operations/op1/events", "succeeded", 0}, {"rollback", "/v1/environments/dev/operations/op1/events", "rolled_back", 1}, {"external", "https://evil.example/events", "succeeded", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/environments/dev/deploys":
					w.WriteHeader(202)
					fmt.Fprintf(w, `{"operation_id":"op1","events_url":%q}`, tc.events)
				case "/v1/environments/dev/operations/op1/events":
					fmt.Fprint(w, "id: 1\nevent: progress\ndata: {}\n\n")
				case "/v1/environments/dev/operations/op1":
					fmt.Fprintf(w, `{"state":%q}`, tc.state)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			}))
			defer server.Close()
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte("project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"), 0600)
			cfg := filepath.Join(dir, "config.json")
			os.WriteFile(cfg, []byte(fmt.Sprintf(`{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}`, server.URL)), 0600)
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
		case "/v1/environments/dev/deploys":
			w.WriteHeader(202)
			fmt.Fprint(w, `{"operation_id":"op1","events_url":"/v1/environments/dev/operations/op1/events"}`)
		case "/v1/environments/dev/operations/op1/events":
			fmt.Fprint(w, "id: 1\nevent: accepted\ndata: {}\n\nid: 2\nevent: succeeded\ndata: {}\n\n")
		case "/v1/environments/dev/operations/op1":
			fmt.Fprint(w, `{"state":"succeeded"}`)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte("project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"), 0600)
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte(fmt.Sprintf(`{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}`, server.URL)), 0600)
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	var out, errs bytes.Buffer
	got := Run([]string{"deploy", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", "v1", "--json"}, &out, &errs)
	if got != 0 || !json.Valid(out.Bytes()) {
		t.Fatalf("exit %d, invalid JSON output %q, error %s", got, out.String(), errs.String())
	}
}

func TestScaleDoesNotLoadCheckoutManifest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/environments/dev/scale" {
			t.Errorf("path %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte(fmt.Sprintf(`{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}`, server.URL)), 0600)
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	var out, errs bytes.Buffer
	got := Run([]string{"scale", "api", "-e", "dev", "--config", cfg, "--manifest-dir", filepath.Join(dir, "missing"), "--replicas", "2", "--dry-run"}, &out, &errs)
	if got != 0 {
		t.Fatalf("exit %d: %s", got, errs.String())
	}
}
