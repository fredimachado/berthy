package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"berthy/internal/agentclient"
)

// envCLIConfig writes a CLI configuration that points env at url, and sets the token it names.
func envCLIConfig(t *testing.T, url, env string) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config.json")
	body := fmt.Sprintf(`{"projects":{"demo":{"environments":{%q:{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, env, url)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	return cfg
}

// countingEnvAgent answers every request with 200 and an empty JSON list, and counts how many it got.
func countingEnvAgent(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("request %s without the token", r.URL.Path)
		}
		n.Add(1)
		fmt.Fprint(w, `[]`)
	}))
	t.Cleanup(server.Close)
	return server, &n
}

func TestReadCommandTakesTheEnvironmentFromBerthyEnv(t *testing.T) {
	server, asked := pathLoggingAgent(t)
	t.Setenv(agentclient.EnvironmentEnv, "dev")
	cfg := ingressCLIConfig(t, server.URL)
	var out, errs bytes.Buffer
	if code := Run([]string{"ps", "--config", cfg}, &out, &errs); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errs.String())
	}
	want := []string{"/v1/projects/demo/environments/dev/apps"}
	if got := asked(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("paths %v, want %v", got, want)
	}
}

func TestChangeCommandTakesTheEnvironmentFromBerthyEnv(t *testing.T) {
	server, asked := pathLoggingAgent(t)
	t.Setenv(agentclient.EnvironmentEnv, "dev")
	cfg := ingressCLIConfig(t, server.URL)
	var out, errs bytes.Buffer
	if code := Run([]string{"history", "api", "--config", cfg}, &out, &errs); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errs.String())
	}
	if got := asked(); len(got) != 1 || got[0] != "/v1/projects/demo/environments/dev/history/api" {
		t.Fatalf("paths %v", got)
	}
}

func TestFlagWinsOverBerthyEnv(t *testing.T) {
	good, asked := pathLoggingAgent(t)
	bad, badAsked := countingEnvAgent(t)
	t.Setenv(agentclient.EnvironmentEnv, "prod")
	cfg := filepath.Join(t.TempDir(), "config.json")
	body := fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"},"prod":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, good.URL, bad.URL)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	var out, errs bytes.Buffer
	if code := Run([]string{"ps", "-e", "dev", "--config", cfg}, &out, &errs); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errs.String())
	}
	if got := asked(); len(got) != 1 || !strings.Contains(got[0], "/environments/dev/") {
		t.Fatalf("paths %v", got)
	}
	if badAsked.Load() != 0 {
		t.Fatalf("the prod agent was asked %d times", badAsked.Load())
	}
}

func TestAnExplicitEmptyEnvironmentIsRefusedEvenWithBerthyEnv(t *testing.T) {
	prod, asked := countingEnvAgent(t)
	cfg := envCLIConfig(t, prod.URL, "prod")
	t.Setenv(agentclient.EnvironmentEnv, "prod")
	for _, args := range [][]string{
		{"ps", "-e", "", "--config", cfg},
		{"restart", "api", "-e", "", "--config", cfg},
	} {
		var out, errs bytes.Buffer
		if code := Run(args, &out, &errs); code != 2 {
			t.Fatalf("%v: exit %d, stderr %q", args, code, errs.String())
		}
	}
	made := withSetupRunner(t, &setupServer{})
	code, out, errout := runSetupCmd("-e", "", "--host", "ops@s1", "--listen", "10.0.0.1:7710", "--network", "n", "--manifest-dir", t.TempDir())
	if code != 2 || out != "" || errout == "" {
		t.Fatalf("setup: exit %d, stdout %q, stderr %q", code, out, errout)
	}
	if made[0] != nil {
		t.Fatalf("setup made a runner for an empty -e")
	}
	if asked.Load() != 0 {
		t.Fatalf("the prod agent was asked %d times", asked.Load())
	}
}

func TestInvalidBerthyEnvIsRefusedBeforeAnyRequest(t *testing.T) {
	server, asked := countingEnvAgent(t)
	cfg := ingressCLIConfig(t, server.URL)
	t.Setenv(agentclient.EnvironmentEnv, "Bad!")
	for _, args := range [][]string{
		{"ps", "--config", cfg},
		{"history", "api", "--config", cfg},
		{"restart", "api", "--config", cfg},
	} {
		var out, errs bytes.Buffer
		if code := Run(args, &out, &errs); code != 2 || !strings.Contains(errs.String(), "BERTHY_ENV is not a valid environment") {
			t.Fatalf("%v: exit %d, stderr %q", args, code, errs.String())
		}
	}
	if asked.Load() != 0 {
		t.Fatalf("the agent was asked %d times", asked.Load())
	}
}

func TestAliasFindsItsManifestThroughBerthyEnv(t *testing.T) {
	srv, requests := aliasAgent(t)
	t.Setenv("BERTHY_CONFIG", ingressCLIConfig(t, srv.URL))
	t.Setenv(agentclient.EnvironmentEnv, "dev")
	t.Chdir(writeAliasDir(t, aliasManifest))
	var out, errs bytes.Buffer
	if code := Run([]string{"console"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errs.String())
	}
	got := requests()
	if len(got) != 1 || got[0].method != "POST" || got[0].path != "/v1/projects/demo/environments/dev/exec" {
		t.Fatalf("requests %+v", got)
	}
}

func TestConfigValidateTakesBerthyEnv(t *testing.T) {
	dir := writeAliasDir(t, aliasManifest)
	t.Setenv(agentclient.EnvironmentEnv, "dev")
	var out, errs bytes.Buffer
	if code := Run([]string{"config", "validate", "--manifest-dir", dir}, &out, &errs); code != 0 || out.String() != "manifest valid\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errs.String())
	}
}

func TestSetupTakesTheEnvironmentFromBerthyEnv(t *testing.T) {
	server := installedServer(t, "ops@s1")
	made := withSetupRunner(t, server)
	dir := projectDir(t)
	t.Setenv(agentclient.EnvironmentEnv, "p5")
	code, out, errout := runSetupCmd("--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", dir)
	if code != 0 || errout != "" {
		t.Fatalf("exit %d: %s %s", code, out, errout)
	}
	if made[0] != "ops@s1" || made[1] != false {
		t.Fatalf("runner made with %v", made)
	}
	if !strings.Contains(out, "cli_config") {
		t.Fatalf("setup did not finish:\n%s", out)
	}
}

func TestContextIgnoresBerthyEnv(t *testing.T) {
	isolatedUserConfig(t)
	t.Setenv(agentclient.EnvironmentEnv, "prod")
	cfg := filepath.Join(t.TempDir(), "config.json")
	code, out, errs := runCLI("context", "add", "--project", "demo", "--url", "http://127.0.0.1:9", "--token-env", "TOK", "--config", cfg)
	if code != 2 || out != "" || !strings.Contains(errs, "context add needs --project, -e, --url and --token-env") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Fatalf("context wrote a file using BERTHY_ENV: %v", err)
	}
}
