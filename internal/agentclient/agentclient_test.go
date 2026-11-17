package agentclient

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"berthy/internal/deploy"
	"berthy/internal/manifest"
)

const composeManifest = `project: demo
network: demo
apps:
  db:
    kind: compose
    file: compose.yaml
    stateful: true
`
const imageManifest = `project: demo
network: demo
apps:
  api:
    kind: image
    image: alpine
    roles:
      web: {}
`

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

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadReadsTargetAndToken(t *testing.T) {
	t.Setenv("BERTHY_TEST_TOKEN", "secret-token")
	cfg := writeConfig(t, `{"environments":{"dev":{"url":"http://127.0.0.1:9000/base/","token_env":"BERTHY_TEST_TOKEN"}}}`)
	got, err := Load(cfg, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "secret-token" || got.Prefix != "/base/v1/environments/dev/" || got.Base.Host != "127.0.0.1:9000" || got.Env != "dev" {
		t.Fatalf("target %+v", got)
	}
}

func TestLoadRefusesWithoutEchoingValues(t *testing.T) {
	t.Setenv("BERTHY_TEST_TOKEN", "secret-token")
	t.Setenv("BERTHY_EMPTY_TOKEN", "")
	good := `{"environments":{"dev":{"url":"http://127.0.0.1:9000","token_env":"BERTHY_TEST_TOKEN"}}}`
	for _, tc := range []struct{ name, body, env, want string }{
		{"unknown env", good, "prod", "unknown environment"},
		{"no token_env", `{"environments":{"dev":{"url":"http://127.0.0.1:9000"}}}`, "dev", "unknown environment or missing token_env"},
		{"empty token", `{"environments":{"dev":{"url":"http://127.0.0.1:9000","token_env":"BERTHY_EMPTY_TOKEN"}}}`, "dev", "token environment variable is empty"},
		{"https", `{"environments":{"dev":{"url":"https://127.0.0.1:9000","token_env":"BERTHY_TEST_TOKEN"}}}`, "dev", "invalid agent URL"},
		{"credentials in URL", `{"environments":{"dev":{"url":"http://user:pw@127.0.0.1:9000","token_env":"BERTHY_TEST_TOKEN"}}}`, "dev", "invalid agent URL"},
		{"unknown field", `{"environments":{},"extra":1}`, "dev", "invalid config"},
		{"trailing JSON", good + `{}`, "dev", "invalid trailing JSON"},
	} {
		_, err := Load(writeConfig(t, tc.body), tc.env)
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "pw") {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	missing := filepath.Join(t.TempDir(), "missing.json")
	if _, err := Load(missing, "dev"); err == nil || err.Error() != "cannot read config "+missing {
		t.Errorf("missing file: %v", err)
	}
}

func TestConfigPathFlagWins(t *testing.T) {
	t.Setenv(ConfigEnv, "/from/env.json")
	got, err := ConfigPath("/from/flag.json")
	if err != nil || got != "/from/flag.json" {
		t.Fatalf("ConfigPath with a flag: %q %v", got, err)
	}
}

func TestConfigPathReadsTheEnvironmentVariable(t *testing.T) {
	t.Setenv(ConfigEnv, "/from/env.json")
	got, err := ConfigPath("")
	if err != nil || got != "/from/env.json" {
		t.Fatalf("ConfigPath with %s: %q %v", ConfigEnv, got, err)
	}
}

func TestConfigPathDefaultsToTheUserConfigDirectory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the XDG_CONFIG_HOME default is Linux")
	}
	t.Setenv(ConfigEnv, "")
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	got, err := ConfigPath("")
	if want := filepath.Join(dir, "berthy", "config.json"); err != nil || got != want {
		t.Fatalf("ConfigPath: %q %v, want %s", got, err, want)
	}
}

func TestConfigPathFailsWithoutAnyLocation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the HOME and XDG_CONFIG_HOME case is Linux")
	}
	t.Setenv(ConfigEnv, "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	if got, err := ConfigPath(""); err == nil || got != "" || !strings.Contains(err.Error(), "--config") {
		t.Fatalf("ConfigPath: %q %v", got, err)
	}
}

func TestPrepareDeployFillsTheRequestFromTheManifestDirectory(t *testing.T) {
	image := writeComposeProject(t, imageManifest, "")
	req := deploy.Request{App: "api", Version: "1.0"}
	if err := PrepareDeploy(image, "dev", &req); err != nil || req.Manifest.Project != "demo" || req.ComposeFile != "" {
		t.Fatalf("image: %v %+v", err, req)
	}
	compose := writeComposeProject(t, composeManifest, "services: {}\n")
	req = deploy.Request{App: "db"}
	if err := PrepareDeploy(compose, "dev", &req); err != nil || req.ComposeFile != "services: {}\n" {
		t.Fatalf("compose: %v %+v", err, req)
	}
	var usage UsageError
	req = deploy.Request{App: "api"}
	if err := PrepareDeploy(image, "dev", &req); !errors.Is(err, ErrVersionRequired) {
		t.Fatalf("image without version: %v", err)
	}
	req = deploy.Request{App: "db", Version: "1"}
	if err := PrepareDeploy(compose, "dev", &req); !errors.Is(err, ErrVersionNotAllowed) {
		t.Fatalf("compose with version: %v", err)
	}
	req = deploy.Request{App: "api", Version: "1"}
	if err := PrepareDeploy(t.TempDir(), "dev", &req); err == nil || errors.As(err, &usage) {
		t.Fatalf("missing manifest: %v", err)
	}
}

func TestDoSendsTheTokenAndBoundsTheAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Path != "/p/v1/x" || r.URL.RawQuery != "a=b" {
			t.Errorf("request %s %s", r.URL, r.Header.Get("Authorization"))
		}
		w.Write([]byte(strings.Repeat("x", (1<<20)+1)))
	}))
	defer srv.Close()
	target := &Target{Base: mustParse(t, srv.URL), Prefix: "/p/", Token: "tok"}
	if _, _, err := Do(t.Context(), NewHTTPClient(), target.Base, target.Prefix, target.Token, "GET", "v1/x", "a=b", nil); err == nil {
		t.Fatal("oversize answer accepted")
	}
}

func TestReadComposeFileStaysInsideAndWithinLimit(t *testing.T) {
	dir := writeComposeProject(t, composeManifest, "services: {}\n")
	if got, err := ReadComposeFile(dir, "compose.yaml"); err != nil || got != "services: {}\n" {
		t.Fatalf("read: %q %v", got, err)
	}
	for _, file := range []string{"..", "missing.yaml", "sub/../../escape.yaml"} {
		if _, err := ReadComposeFile(dir, file); err == nil {
			t.Errorf("%s accepted", file)
		}
	}
	outside := filepath.Join(filepath.Dir(dir), "outside.yaml")
	if err := os.WriteFile(outside, []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link.yaml")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks need developer mode on Windows")
		}
		t.Fatal(err)
	}
	if _, err := ReadComposeFile(dir, "link.yaml"); err == nil || !strings.Contains(err.Error(), "inside the manifest directory") {
		t.Fatalf("symlink out of the directory: %v", err)
	}
	big := writeComposeProject(t, composeManifest, strings.Repeat("x", manifest.MaxComposeFile+1))
	if _, err := ReadComposeFile(big, "compose.yaml"); err == nil {
		t.Fatal("file over the limit accepted")
	}
	binary := writeComposeProject(t, composeManifest, "services:\x00\n")
	if _, err := ReadComposeFile(binary, "compose.yaml"); err == nil {
		t.Fatal("NUL in compose file accepted")
	}
}

func TestAgentErrorShowsOnlySafeMessages(t *testing.T) {
	long := strings.Repeat("x", 300)
	for _, tc := range []struct{ name, body, want string }{
		{"short", `{"message":"use --to"}`, "agent returned HTTP 409: use --to"},
		{"300 bytes", `{"message":"` + long + `"}`, "agent returned HTTP 409: " + long},
		{"301 bytes", `{"message":"` + long + `x"}`, "agent returned HTTP 409"},
		{"nul", `{"message":"a\u0000b"}`, "agent returned HTTP 409"},
		{"newline", `{"message":"a\nb"}`, "agent returned HTTP 409"},
		{"empty", `{"message":""}`, "agent returned HTTP 409"},
		{"no message", `{"code":"x"}`, "agent returned HTTP 409"},
		{"not json", `<html>`, "agent returned HTTP 409"},
	} {
		if got := Message(409, []byte(tc.body)); got != tc.want {
			t.Errorf("%s: %q", tc.name, got)
		}
	}
	if PrintableMessage("bad\xffbyte") {
		t.Error("invalid UTF-8 accepted")
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestPrepareDeployAcceptsAnImageAppThatKeepsItsVersion(t *testing.T) {
	image := writeComposeProject(t, imageManifest, "")
	req := deploy.Request{App: "api", KeepVersion: true}
	if err := PrepareDeploy(image, "dev", &req); err != nil || req.Manifest.Project != "demo" || req.Version != "" {
		t.Fatalf("keep version: %v %+v", err, req)
	}
}
