package agentclient

import (
	"errors"
	"io"
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

// cfgDev is the CLI configuration of one project with one environment, dev, whose token is in BERTHY_TEST_TOKEN.
const cfgDev = `{"projects":{"demo":{"environments":{"dev":{"url":"http://127.0.0.1:9000/base/","token_env":"BERTHY_TEST_TOKEN"}}}}}`

func TestResolveReadsTargetAndToken(t *testing.T) {
	t.Setenv("BERTHY_TEST_TOKEN", "secret-token")
	got, err := Resolve(writeConfig(t, cfgDev), Selector{Environment: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "secret-token" || got.Prefix != "/base/v1/projects/demo/environments/dev/" || got.Base.Host != "127.0.0.1:9000" || got.Env != "dev" || got.Project != "demo" {
		t.Fatalf("target %+v", got)
	}
}

func TestResolveBuildsTheProjectPath(t *testing.T) {
	t.Setenv("BERTHY_TEST_TOKEN", "secret-token")
	cfg := writeConfig(t, `{"projects":{"demo":{"environments":{"dev":{"url":"http://127.0.0.1:9000/base/","token_env":"BERTHY_TEST_TOKEN"}}},"other-app":{"environments":{"dev":{"url":"http://127.0.0.1:9000","token_env":"BERTHY_TEST_TOKEN"}}}}}`)
	for project, want := range map[string]string{"demo": "/base/v1/projects/demo/environments/dev/", "other-app": "/v1/projects/other-app/environments/dev/"} {
		got, err := Resolve(cfg, Selector{Project: project, Environment: "dev"})
		if err != nil || got.Prefix != want {
			t.Errorf("project %s: prefix %q, %v, want %q", project, got.Prefix, err, want)
		}
	}
}

func TestResolveRefusesWithoutEchoingValues(t *testing.T) {
	t.Setenv("BERTHY_TEST_TOKEN", "secret-token")
	t.Setenv("BERTHY_EMPTY_TOKEN", "")
	one := func(entry string) string {
		return `{"projects":{"demo":{"environments":{"dev":` + entry + `}}}}`
	}
	good := one(`{"url":"http://127.0.0.1:9000","token_env":"BERTHY_TEST_TOKEN"}`)
	for _, tc := range []struct{ name, body, env, want string }{
		{"unknown env", good, "prod", "unknown environment or missing token_env"},
		{"no token_env", one(`{"url":"http://127.0.0.1:9000"}`), "dev", "unknown environment or missing token_env"},
		{"empty token", one(`{"url":"http://127.0.0.1:9000","token_env":"BERTHY_EMPTY_TOKEN"}`), "dev", "no token for demo/dev: its token variable is empty"},
		{"https", one(`{"url":"https://127.0.0.1:9000","token_env":"BERTHY_TEST_TOKEN"}`), "dev", "invalid agent URL"},
		{"credentials in URL", one(`{"url":"http://user:pw@127.0.0.1:9000","token_env":"BERTHY_TEST_TOKEN"}`), "dev", "invalid agent URL"},
		{"unknown field", `{"projects":{},"extra":1}`, "dev", "invalid config"},
		{"trailing JSON", good + `{}`, "dev", "invalid trailing JSON"},
	} {
		_, err := Resolve(writeConfig(t, tc.body), Selector{Environment: tc.env})
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "pw") {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	missing := filepath.Join(t.TempDir(), "missing.json")
	if _, err := Resolve(missing, Selector{Environment: "dev"}); err == nil || err.Error() != "cannot read config "+missing {
		t.Errorf("missing file: %v", err)
	}
}

// twoProjects is a CLI configuration where CardioTennis and SliceUp both have prod, and only SliceUp has dev.
const twoProjects = `{"projects":{
  "cardiotennis":{"environments":{"prod":{"url":"http://10.0.0.1:7710","token_env":"CT_TOKEN"}}},
  "sliceup":{"environments":{"prod":{"url":"http://10.0.0.2:7710","token_env":"SU_TOKEN"},"dev":{"url":"http://10.0.0.3:7710","token_env":"SU_DEV_TOKEN"}}}}}`

func TestResolvePicksTheProjectOfTheFlag(t *testing.T) {
	t.Setenv("CT_TOKEN", "ct")
	t.Setenv("SU_TOKEN", "su")
	cfg := writeConfig(t, twoProjects)
	for project, want := range map[string]string{"cardiotennis": "10.0.0.1:7710", "sliceup": "10.0.0.2:7710"} {
		got, err := Resolve(cfg, Selector{Project: project, Environment: "prod"})
		if err != nil || got.Base.Host != want || got.Project != project {
			t.Errorf("%s: %+v, %v", project, got, err)
		}
	}
	// The flag wins over the manifest directory.
	dir := writeComposeProject(t, imageManifest, "")
	got, err := Resolve(cfg, Selector{Project: "sliceup", Environment: "prod", ManifestDir: dir})
	if err != nil || got.Project != "sliceup" {
		t.Errorf("the flag and a manifest of another project: %+v, %v", got, err)
	}
	_, err = Resolve(cfg, Selector{Project: "cardiotennis", Environment: "dev"})
	if err == nil || err.Error() != "no environment dev for project cardiotennis in the CLI configuration" {
		t.Errorf("an environment the project does not have: %v", err)
	}
	_, err = Resolve(cfg, Selector{Project: "nobody", Environment: "prod"})
	if err == nil || err.Error() != "no environment prod for project nobody in the CLI configuration" {
		t.Errorf("a project the file does not have: %v", err)
	}
}

func TestResolveReadsTheProjectOfTheManifestDirectory(t *testing.T) {
	t.Setenv("CT_TOKEN", "ct")
	t.Setenv("SU_TOKEN", "su")
	cfg := writeConfig(t, twoProjects)
	dir := writeComposeProject(t, strings.Replace(imageManifest, "project: demo", "project: sliceup", 1), "")
	got, err := Resolve(cfg, Selector{Environment: "prod", ManifestDir: dir})
	if err != nil || got.Project != "sliceup" || got.Base.Host != "10.0.0.2:7710" {
		t.Fatalf("%+v, %v", got, err)
	}
	// A manifest of a project the file has no environment for is not guessed around.
	other := writeComposeProject(t, imageManifest, "")
	if _, err := Resolve(cfg, Selector{Environment: "prod", ManifestDir: other}); err == nil || err.Error() != "no environment prod for project demo in the CLI configuration" {
		t.Errorf("a manifest of another project: %v", err)
	}
	// A directory without a manifest falls back to the only match.
	t.Setenv("SU_DEV_TOKEN", "su-dev")
	got, err = Resolve(cfg, Selector{Environment: "dev", ManifestDir: t.TempDir()})
	if err != nil || got.Project != "sliceup" {
		t.Errorf("a directory without a manifest: %+v, %v", got, err)
	}
	// A manifest without a valid project is an error, whatever the file holds.
	bad := writeComposeProject(t, "network: demo\n", "")
	_, err = Resolve(cfg, Selector{Environment: "dev", ManifestDir: bad})
	if want := "cannot read the project of " + filepath.Join(bad, "berthy.yml") + ": give --project"; err == nil || err.Error() != want {
		t.Errorf("a manifest without a project: %v, want %q", err, want)
	}
}

func TestResolveWithoutAProjectTakesTheOnlyOneThatHasTheEnvironment(t *testing.T) {
	t.Setenv("SU_DEV_TOKEN", "su-dev")
	got, err := Resolve(writeConfig(t, twoProjects), Selector{Environment: "dev"})
	if err != nil || got.Project != "sliceup" || got.Token != "su-dev" {
		t.Fatalf("%+v, %v", got, err)
	}
	// An environment that no project has is unknown.
	if _, err := Resolve(writeConfig(t, twoProjects), Selector{Environment: "staging"}); err == nil || err.Error() != "unknown environment or missing token_env" {
		t.Errorf("an environment no project has: %v", err)
	}
}

func TestResolveRefusesAnEnvironmentThatSeveralProjectsHave(t *testing.T) {
	t.Setenv("CT_TOKEN", "ct")
	t.Setenv("SU_TOKEN", "su")
	_, err := Resolve(writeConfig(t, twoProjects), Selector{Environment: "prod"})
	want := "environment prod exists in several projects (cardiotennis, sliceup): give --project, or run the command in the project's directory"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	for _, leaked := range []string{"10.0.0", "CT_TOKEN", "SU_TOKEN"} {
		if strings.Contains(err.Error(), leaked) {
			t.Errorf("the refusal repeats %q", leaked)
		}
	}
}

func TestParseConfigRefusesTheLayoutOfAnEarlierRelease(t *testing.T) {
	_, err := ParseConfig([]byte(`{"environments":{"dev":{"url":"http://127.0.0.1:9000","token_env":"SECRET_VAR"}}}`))
	want := "the CLI configuration is in the layout of an earlier release: run berthy setup again, or write it as projects.<project>.environments.<environment>"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	if _, err := Resolve(writeConfig(t, `{"environments":{}}`), Selector{Environment: "dev"}); err == nil || err.Error() != want {
		t.Errorf("Resolve: %v", err)
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
	if _, err := PrepareDeploy(image, "dev", &req); err != nil || req.Manifest.Project != "demo" || req.ComposeFile != "" {
		t.Fatalf("image: %v %+v", err, req)
	}
	compose := writeComposeProject(t, composeManifest, "services: {}\n")
	req = deploy.Request{App: "db"}
	if _, err := PrepareDeploy(compose, "dev", &req); err != nil || req.ComposeFile != "services: {}\n" {
		t.Fatalf("compose: %v %+v", err, req)
	}
	var usage UsageError
	req = deploy.Request{App: "api"}
	if _, err := PrepareDeploy(image, "dev", &req); !errors.Is(err, ErrVersionRequired) {
		t.Fatalf("image without version: %v", err)
	}
	req = deploy.Request{App: "db", Version: "1"}
	if _, err := PrepareDeploy(compose, "dev", &req); !errors.Is(err, ErrVersionNotAllowed) {
		t.Fatalf("compose with version: %v", err)
	}
	req = deploy.Request{App: "api", Version: "1"}
	if _, err := PrepareDeploy(t.TempDir(), "dev", &req); err == nil || errors.As(err, &usage) {
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

// A backslash in a manifest's file is a separator on every OS, as the manifest check reads it.
func TestReadComposeFileTakesABackslashAsASeparator(t *testing.T) {
	dir := writeComposeProject(t, composeManifest, "services: {}\n")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "compose.yaml"), []byte("services: {a: {}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadComposeFile(dir, `sub\compose.yaml`); err != nil || got != "services: {a: {}}\n" {
		t.Fatalf("read: %q %v", got, err)
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
	if _, err := PrepareDeploy(image, "dev", &req); err != nil || req.Manifest.Project != "demo" || req.Version != "" {
		t.Fatalf("keep version: %v %+v", err, req)
	}
}

// preProjectsAgent is an agent of a release before projects: it serves /v1/environments/dev/ for the token "tok" and answers 404
// to everything else, as the agent of that release does.
func preProjectsAgent(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/base/v1/environments/dev/agent" && r.Header.Get("Authorization") == "Bearer tok" {
			w.Write([]byte(`{"environment":"dev","version":"v0.8.0"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"code":"not_found","message":"not found"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestARequestToAnAgentWithoutProjectsSaysToUpgradeIt(t *testing.T) {
	srv := preProjectsAgent(t)
	base := mustParse(t, srv.URL)
	const hint = "agent returned HTTP 404: the agent runs an older release than this CLI; upgrade it with berthy setup"
	data, status, err := Do(t.Context(), NewHTTPClient(), base, "/base/v1/projects/demo/environments/dev/", "tok", "GET", "apps", "", nil)
	if err != nil || status != http.StatusNotFound || Message(status, data) != hint {
		t.Fatalf("Do: %d %s %v", status, data, err)
	}
	resp, err := Send(t.Context(), NewHTTPClient(), "POST", srv.URL+"/base/v1/projects/demo/environments/dev/deploys", "tok", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !IsOlderAgent(resp) {
		t.Fatal("successful legacy probe was not marked")
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound || Message(resp.StatusCode, body) != hint {
		t.Fatalf("Send: %d %s", resp.StatusCode, body)
	}
}

func TestA404FromAnAgentWithProjectsIsLeftAlone(t *testing.T) {
	var probes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/v1/environments/") {
			probes++
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"code":"not_found","message":"app not found"}`))
	}))
	defer srv.Close()
	data, status, err := Do(t.Context(), NewHTTPClient(), mustParse(t, srv.URL), "/v1/projects/demo/environments/dev/", "tok", "GET", "apps/web", "", nil)
	if err != nil || status != http.StatusNotFound || Message(status, data) != "agent returned HTTP 404: app not found" {
		t.Fatalf("Do: %d %s %v", status, data, err)
	}
	// An agent that does not answer the probe, or refuses its token, is not told to upgrade either.
	older := preProjectsAgent(t)
	data, status, _ = Do(t.Context(), NewHTTPClient(), mustParse(t, older.URL), "/base/v1/projects/demo/environments/dev/", "wrong", "GET", "apps", "", nil)
	if Message(status, data) != "agent returned HTTP 404: not found" {
		t.Fatalf("a refused probe: %d %s", status, data)
	}
}

// A CLI configuration edited by hand can hold a pair the agent would refuse; Resolve refuses it too.
func TestResolveRefusesAPairOverTheLengthBound(t *testing.T) {
	t.Setenv("BERTHY_TEST_TOKEN", "secret-token")
	project, env := strings.Repeat("p", 30), strings.Repeat("e", 30)
	cfg := writeConfig(t, `{"projects":{"`+project+`":{"environments":{"`+env+`":{"url":"http://127.0.0.1:9000","token_env":"BERTHY_TEST_TOKEN"}}}}}`)
	for _, sel := range []Selector{{Environment: env}, {Project: project, Environment: env}} {
		_, err := Resolve(cfg, sel)
		if err == nil || !strings.Contains(err.Error(), "longer than 59 characters; shorten the project or environment") {
			t.Errorf("selector %+v: %v", sel, err)
		}
	}
}
