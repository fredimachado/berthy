package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"berthy/internal/agentclient"
)

// isolatedUserConfig points the user configuration directory at a new temporary directory for the rest of the test, clears
// BERTHY_CONFIG, and returns the path of that directory as os.UserConfigDir reports it. No test reads the configuration of
// the person who runs the tests.
func isolatedUserConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv(agentclient.ConfigEnv, "")
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("HOME", home)
	t.Setenv("AppData", home)
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeDefaultConfig writes a CLI configuration for dev at url into the user configuration directory, where the CLI looks
// for it when neither --config nor BERTHY_CONFIG is given.
func writeDefaultConfig(t *testing.T, dir, url string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "berthy"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "berthy", "config.json")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, url)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	return path
}

// pathLoggingAgent answers every request with 200 and an empty JSON list, and returns the paths it was asked for.
func pathLoggingAgent(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("request %s without the token", r.URL.Path)
		}
		paths = append(paths, r.URL.Path)
		fmt.Fprint(w, `[]`)
	}))
	t.Cleanup(server.Close)
	return server, func() []string { return paths }
}

func TestRemoteCommandsFindTheConfigThroughTheEnvironmentVariable(t *testing.T) {
	server, asked := pathLoggingAgent(t)
	t.Setenv(agentclient.ConfigEnv, ingressCLIConfig(t, server.URL))
	for _, args := range [][]string{{"ps", "-e", "dev"}, {"history", "api", "-e", "dev"}} {
		var out, errs bytes.Buffer
		if code := Run(args, &out, &errs); code != 0 {
			t.Fatalf("%v: exit %d, stderr %q", args, code, errs.String())
		}
	}
	want := []string{"/v1/environments/dev/apps", "/v1/environments/dev/history/api"}
	if got := asked(); !reflect.DeepEqual(got, want) {
		t.Fatalf("paths %v, want %v", got, want)
	}
}

func TestRemoteCommandsReadTheDefaultFile(t *testing.T) {
	dir := isolatedUserConfig(t)
	server, asked := pathLoggingAgent(t)
	writeDefaultConfig(t, dir, server.URL)
	for _, args := range [][]string{{"ps", "-e", "dev"}, {"history", "api", "-e", "dev"}} {
		var out, errs bytes.Buffer
		if code := Run(args, &out, &errs); code != 0 {
			t.Fatalf("%v: exit %d, stderr %q", args, code, errs.String())
		}
	}
	if got := asked(); len(got) != 2 {
		t.Fatalf("paths %v", got)
	}
}

func TestFlagWinsOverTheEnvironmentVariableAndTheDefaultFile(t *testing.T) {
	dir := isolatedUserConfig(t)
	server, asked := pathLoggingAgent(t)
	writeDefaultConfig(t, dir, "http://127.0.0.1:1")
	t.Setenv(agentclient.ConfigEnv, filepath.Join(dir, "missing.json"))
	good := ingressCLIConfig(t, server.URL)
	var out, errs bytes.Buffer
	if code := Run([]string{"ps", "-e", "dev", "--config", good}, &out, &errs); code != 0 || len(asked()) != 1 {
		t.Fatalf("exit %d, stderr %q", code, errs.String())
	}
}

func TestRemoteCommandNamesTheDefaultFileWhenItIsMissing(t *testing.T) {
	dir := isolatedUserConfig(t)
	want := filepath.Join(dir, "berthy", "config.json")
	for _, args := range [][]string{{"ps", "-e", "dev"}, {"history", "api", "-e", "dev"}} {
		var out, errs bytes.Buffer
		if code := Run(args, &out, &errs); code != 1 || !strings.Contains(errs.String(), want) || out.Len() != 0 {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q", args, code, out.String(), errs.String())
		}
	}
}

func TestRemoteCommandsStillRequireAnEnvironment(t *testing.T) {
	isolatedUserConfig(t)
	t.Setenv(agentclient.ConfigEnv, ingressCLIConfig(t, "http://127.0.0.1:1"))
	var out, errs bytes.Buffer
	if code := Run([]string{"ps"}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "remote commands require -e") {
		t.Fatalf("ps: exit %d, stderr %q", code, errs.String())
	}
	errs.Reset()
	if code := Run([]string{"history", "api"}, &out, &errs); code != 2 {
		t.Fatalf("history: exit %d, stderr %q", code, errs.String())
	}
}
