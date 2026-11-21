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
	"testing"
)

const composeManifest = "project: demo\nnetwork: demo\napps:\n  db:\n    kind: compose\n    file: compose.yaml\n    stateful: true\n"

// composeAgent answers a dry run with 200 and records the body of every deploy request it receives.
func composeAgent(t *testing.T, bodies *[]map[string]any) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/projects/demo/environments/dev/deploys" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("bad request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		if json.Unmarshal(raw, &body) != nil {
			t.Errorf("invalid body %q", raw)
		}
		*bodies = append(*bodies, body)
		fmt.Fprint(w, `{"app":"db"}`)
	}))
	t.Cleanup(server.Close)
	cfg := t.TempDir() + "/config.json"
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, server.URL)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	return cfg
}

func writeComposeProject(t *testing.T, manifestText, file string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(manifestText), 0600); err != nil {
		t.Fatal(err)
	}
	if file != "" {
		if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(file), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDeployComposeSendsFileWithoutVersion(t *testing.T) {
	var bodies []map[string]any
	cfg := composeAgent(t, &bodies)
	file := "services:\n  database:\n    image: postgres:16\n"
	dir := writeComposeProject(t, composeManifest, file)
	var out, errs bytes.Buffer
	if code := Run([]string{"deploy", "db", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--dry-run"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	if len(bodies) != 1 || bodies[0]["compose_file"] != file {
		t.Fatalf("request %v", bodies)
	}
	if _, ok := bodies[0]["allow_recreate"]; ok {
		t.Fatal("allow_recreate sent without the flag")
	}
	if v, ok := bodies[0]["version"]; ok && v != "" {
		t.Fatalf("version sent: %v", v)
	}
}

func TestDeployComposeSendsAllowRecreate(t *testing.T) {
	var bodies []map[string]any
	cfg := composeAgent(t, &bodies)
	dir := writeComposeProject(t, composeManifest, "services: {}\n")
	var out, errs bytes.Buffer
	if code := Run([]string{"deploy", "db", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--dry-run", "--allow-recreate"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	if len(bodies) != 1 || bodies[0]["allow_recreate"] != true {
		t.Fatalf("request %v", bodies)
	}
}

func TestDeployComposeRefusesVersionFlagAndKeepsImageRule(t *testing.T) {
	var bodies []map[string]any
	cfg := composeAgent(t, &bodies)
	dir := writeComposeProject(t, composeManifest, "services: {}\n")
	var out, errs bytes.Buffer
	if code := Run([]string{"deploy", "db", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", "v1"}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "--version does not apply to a compose app") {
		t.Fatalf("version on compose: exit %d %q", code, errs.String())
	}
	image := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"
	imageDir := writeComposeProject(t, image, "")
	errs.Reset()
	if code := Run([]string{"deploy", "api", "-e", "dev", "--config", cfg, "--manifest-dir", imageDir, "--dry-run"}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "deploy requires --version") {
		t.Fatalf("image deploy without version: exit %d %q", code, errs.String())
	}
	if len(bodies) != 0 {
		t.Fatalf("refused commands reached the agent: %v", bodies)
	}
}

func TestDeployComposeRefusesFileOutsideManifestDirectory(t *testing.T) {
	var bodies []map[string]any
	cfg := composeAgent(t, &bodies)
	dir := writeComposeProject(t, strings.Replace(composeManifest, "file: compose.yaml", "file: '..'", 1), "")
	var out, errs bytes.Buffer
	if code := Run([]string{"deploy", "db", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--dry-run"}, &out, &errs); code != 1 || !strings.Contains(errs.String(), "invalid compose file") {
		t.Fatalf("exit %d: %q", code, errs.String())
	}
	if len(bodies) != 0 {
		t.Fatal("refused file reached the agent")
	}
}
