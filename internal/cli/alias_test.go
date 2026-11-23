package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"berthy/internal/agentclient"
)

// aliasManifest is a manifest of one image app with the three aliases the guide shows.
const aliasManifest = `project: demo
network: demo
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    roles:
      web: {}
aliases:
  console: exec api --role web -- bin/console
  weblogs: logs api --role web --follow
  hist: history api
  ship: deploy api --version v1
`

// writeAliasDir writes berthy.yml with text into a new directory and returns the directory.
func writeAliasDir(t *testing.T, text string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCheckAliasesRefusesWhatCannotRun(t *testing.T) {
	for _, tc := range []struct {
		name    string
		aliases map[string]string
		want    string
	}{
		{"shadows deploy", map[string]string{"deploy": "exec api -- true"}, "deploy"},
		{"shadows ps", map[string]string{"ps": "logs api"}, "ps"},
		{"shadows docs", map[string]string{"docs": "logs api"}, "docs"},
		{"first word is ui", map[string]string{"console": "ui -e prod"}, "console"},
		{"first word is mcp", map[string]string{"serve": "mcp"}, "serve"},
		{"first word is version", map[string]string{"v": "version"}, "v"},
		{"first word is docs", map[string]string{"d": "docs"}, "d"},
		{"first word is another alias", map[string]string{"a": "logs api", "b": "a api"}, "b"},
		{"first word is unknown", map[string]string{"x": "nosuch api"}, "x"},
		{"sets -e", map[string]string{"console": "exec api -e prod -- bin/console"}, "console"},
		{"sets --config", map[string]string{"console": "exec api --config c.json -- bin/console"}, "console"},
		{"sets --environment", map[string]string{"console": "exec --environment prod api"}, "console"},
	} {
		err := checkAliases(tc.aliases)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want one naming %s", tc.name, err, tc.want)
		}
	}
}

func TestCheckAliasesAcceptsWhatTheGuideShows(t *testing.T) {
	if err := checkAliases(nil); err != nil {
		t.Fatalf("no aliases: %v", err)
	}
	for name, value := range map[string]string{
		"console": "exec api --role web -- bin/console",
		"weblogs": "logs api --role web --follow",
		"hist":    "history api",
	} {
		if err := checkAliases(map[string]string{name: value}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestExpandAliasPlacesTheEnvironmentAfterTheFirstWord(t *testing.T) {
	dir := writeAliasDir(t, aliasManifest)
	t.Chdir(dir)
	for _, tc := range []struct {
		name string
		cmd  string
		args []string
		want []string
	}{
		{"exec alias", "console", []string{"-e", "prod"}, []string{"exec", "-e", "prod", "api", "--role", "web", "--", "bin/console"}},
		{"extra arguments go last", "console", []string{"-e", "prod", "--config", "c.json", "extra"},
			[]string{"exec", "-e", "prod", "--config", "c.json", "api", "--role", "web", "--", "bin/console", "extra"}},
		{"flags after an alias without --", "weblogs", []string{"-e", "prod", "--tail", "5"},
			[]string{"logs", "-e", "prod", "api", "--role", "web", "--follow", "--tail", "5"}},
		// --manifest-dir is read for the lookup. It is passed on only to a command that takes it: deploy reads the manifest, and
		// exec and the read commands do not take it.
		{"manifest dir is dropped for an exec alias", "console", []string{"-e", "prod", "--manifest-dir", dir},
			[]string{"exec", "-e", "prod", "api", "--role", "web", "--", "bin/console"}},
		{"manifest dir is dropped for a read alias", "weblogs", []string{"--manifest-dir", dir, "-e", "prod"},
			[]string{"logs", "-e", "prod", "api", "--role", "web", "--follow"}},
		{"manifest dir is kept for a deploy alias", "ship", []string{"-e", "dev", "--manifest-dir", dir},
			[]string{"deploy", "-e", "dev", "--manifest-dir", dir, "api", "--version", "v1"}},
	} {
		// Whether or not the line keeps --manifest-dir, the directory the alias was found in is returned, for the command to find its
		// project in.
		wantDir := "."
		if slices.Contains(tc.args, "--manifest-dir") {
			wantDir = dir
		}
		got, gotDir, _, ok, err := expandAlias(tc.cmd, tc.args)
		if err != nil || !ok || !reflect.DeepEqual(got, tc.want) || gotDir != wantDir {
			t.Errorf("%s: got %q dir %q ok %v err %v, want %q dir %q", tc.name, got, gotDir, ok, err, tc.want, wantDir)
		}
	}
}

func TestExpandAliasRefusesWhatItCannotExpand(t *testing.T) {
	t.Setenv(agentclient.EnvironmentEnv, "")
	dir := writeAliasDir(t, aliasManifest)
	empty := t.TempDir()
	broken := writeAliasDir(t, "apps: [\n")
	shadowing := writeAliasDir(t, aliasManifest+"  deploy: exec api -- true\n")
	for _, tc := range []struct {
		name    string
		cmd     string
		args    []string
		isAlias bool
		want    string
	}{
		{"no -e with a manifest", "console", []string{"--manifest-dir", dir}, false, "unknown command console (an alias of the manifest needs -e)"},
		{"no -e and no manifest", "console", []string{"--manifest-dir", empty}, false, "unknown command console"},
		{"unknown name", "nosuch", []string{"-e", "prod", "--manifest-dir", dir}, false, "unknown command nosuch"},
		{"manifest does not load", "console", []string{"-e", "prod", "--manifest-dir", broken}, false, "unknown command console (the manifest did not load"},
		{"no manifest with -e", "console", []string{"-e", "prod", "--manifest-dir", empty}, false, "unknown command console"},
		{"missing value", "console", []string{"-e"}, false, "missing value for -e"},
		{"bad alias set", "console", []string{"-e", "prod", "--manifest-dir", shadowing}, true, "deploy"},
	} {
		got, _, _, ok, err := expandAlias(tc.cmd, tc.args)
		if got != nil || ok != tc.isAlias || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %q ok %v err %v, want ok %v and an error with %q", tc.name, got, ok, err, tc.isAlias, tc.want)
		}
	}
}

// aliasRequest is one request the fake agent received.
type aliasRequest struct {
	method, path, query string
	body                []byte
}

// aliasAgent serves every request with 200 and records it. A logs request gets a stream that ends at once.
func aliasAgent(t *testing.T) (*httptest.Server, func() []aliasRequest) {
	t.Helper()
	var got []aliasRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, aliasRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: b})
		if strings.HasSuffix(r.URL.Path, "/logs") {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: done\ndata: {\"complete\":true}\n\n")
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []aliasRequest { return got }
}

func TestAliasRunsThroughTheAgent(t *testing.T) {
	srv, requests := aliasAgent(t)
	t.Setenv("BERTHY_CONFIG", ingressCLIConfig(t, srv.URL))
	t.Chdir(writeAliasDir(t, aliasManifest))
	for _, args := range [][]string{
		{"console", "-e", "dev"},
		{"console", "-e", "dev", "extra"},
		{"weblogs", "-e", "dev", "--tail", "5"},
	} {
		var out, errs bytes.Buffer
		if code := Run(args, &out, &errs); code != 0 {
			t.Fatalf("%v: exit %d, stderr %q", args, code, errs.String())
		}
	}
	got := requests()
	if len(got) != 3 {
		t.Fatalf("requests %+v", got)
	}
	for i, wantCommand := range [][]string{{"bin/console"}, {"bin/console", "extra"}} {
		var body struct {
			App     string   `json:"app"`
			Role    string   `json:"role"`
			Command []string `json:"command"`
		}
		if got[i].method != "POST" || got[i].path != "/v1/projects/demo/environments/dev/exec" {
			t.Fatalf("request %d: %s %s", i, got[i].method, got[i].path)
		}
		if err := json.Unmarshal(got[i].body, &body); err != nil {
			t.Fatal(err)
		}
		if body.App != "api" || body.Role != "web" || !reflect.DeepEqual(body.Command, wantCommand) {
			t.Errorf("request %d: app %q role %q command %q, want command %q", i, body.App, body.Role, body.Command, wantCommand)
		}
	}
	query, err := url.ParseQuery(got[2].query)
	if err != nil {
		t.Fatal(err)
	}
	if got[2].method != "GET" || got[2].path != "/v1/projects/demo/environments/dev/apps/api/logs" || query.Get("role") != "web" || query.Get("follow") != "1" || query.Get("tail") != "5" {
		t.Errorf("logs request: %s %s?%s", got[2].method, got[2].path, got[2].query)
	}
}

func TestConfigValidateRefusesABadAlias(t *testing.T) {
	for name, tc := range map[string]struct {
		line, want string
	}{
		"shadows deploy": {"  deploy: exec api -- true\n", "deploy"},
		"sets -e":        {"  bad: exec api -e prod -- true\n", "bad"},
	} {
		dir := writeAliasDir(t, aliasManifest+tc.line)
		var out, errs bytes.Buffer
		if code := Run([]string{"config", "validate", "-e", "dev", "--manifest-dir", dir}, &out, &errs); code != 1 || !strings.Contains(errs.String(), tc.want) || out.Len() != 0 {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, out.String(), errs.String())
		}
	}
	var out, errs bytes.Buffer
	if code := Run([]string{"config", "validate", "-e", "dev", "--manifest-dir", writeAliasDir(t, aliasManifest)}, &out, &errs); code != 0 || out.String() != "manifest valid\n" {
		t.Fatalf("valid aliases: exit %d, stdout %q, stderr %q", code, out.String(), errs.String())
	}
}

func TestUnknownCommandSaysSo(t *testing.T) {
	isolatedUserConfig(t)
	t.Setenv(agentclient.EnvironmentEnv, "")
	t.Chdir(t.TempDir())
	for _, args := range [][]string{{"nosuch"}, {"nosuch", "-e", "dev"}} {
		var out, errs bytes.Buffer
		if code := Run(args, &out, &errs); code != 2 || !strings.Contains(errs.String(), "unknown command nosuch") || out.Len() != 0 {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, out.String(), errs.String())
		}
	}
}

// probeOnlyCommands are the built-ins that serve or write a volume whatever flags they get, so Run would start them. The
// list checks them by name only.
var probeOnlyCommands = []string{"probe-helper", "volume-archive", "volume-extract"}

// TestBuiltinCommandsAllDispatch keeps builtinCommands in step with Run: every name reaches its handler, and a made-up name
// does not. A local command is given a flag it does not read, so it fails at once; an agent command is given an environment,
// and reaches the agent, which refuses the connection.
func TestBuiltinCommandsAllDispatch(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv(agentclient.EnvironmentEnv, "")
	t.Setenv("BERTHY_CONFIG", ingressCLIConfig(t, "http://127.0.0.1:1"))
	for _, name := range builtinCommands {
		if slices.Contains(probeOnlyCommands, name) {
			continue
		}
		args := []string{name, "--nonsense"}
		if slices.Contains(agentCommands, name) {
			args = []string{name, "-e", "dev"}
		}
		var out, errs bytes.Buffer
		Run(args, &out, &errs)
		if strings.Contains(errs.String(), "unknown command") {
			t.Errorf("%v: %q", args, errs.String())
		}
	}
	var out, errs bytes.Buffer
	if code := Run([]string{"nosuchcommand"}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "unknown command nosuchcommand") {
		t.Errorf("made-up name: exit %d, stderr %q", code, errs.String())
	}
}

func TestAliasPassesProjectFlagAfterCommandSeparator(t *testing.T) {
	srv, requests := aliasAgent(t)
	t.Setenv("BERTHY_CONFIG", ingressCLIConfig(t, srv.URL))
	t.Chdir(writeAliasDir(t, strings.Replace(aliasManifest, "bin/console", "bin/console --project demo", 1)))
	var out, errs bytes.Buffer
	if code := Run([]string{"console", "-e", "dev"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	got := requests()
	if len(got) != 1 {
		t.Fatalf("requests: %v", got)
	}
	var body struct {
		Command []string `json:"command"`
	}
	if err := json.Unmarshal(got[0].body, &body); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(body.Command, []string{"bin/console", "--project", "demo"}) {
		t.Fatalf("command %q", body.Command)
	}
}

func TestAliasCannotHideProjectFlagBehindFlagValue(t *testing.T) {
	cfg, cardio, slice := twoProjectsServers(t)
	dir := writeAliasDir(t, strings.Replace(aliasManifest, "project: demo", "project: cardiotennis", 1)+"  acquire: lock acquire --message -- --project sliceup\n")
	t.Chdir(dir)
	var out, errs bytes.Buffer
	code := Run([]string{"acquire", "-e", "prod", "--project", "cardiotennis", "--config", cfg}, &out, &errs)
	if code == 0 || !strings.Contains(errs.String(), "--project is not allowed") {
		t.Errorf("exit %d, stderr %q", code, errs.String())
	}
	if cardio.Load() != 0 || slice.Load() != 0 {
		t.Fatalf("sent %d and %d requests", cardio.Load(), slice.Load())
	}
}

// writeDevOnlyHoldAlias writes a base manifest and a berthy.dev.yml that defines hold, with no hold in prod.
func writeDevOnlyHoldAlias(t *testing.T, holdLine string) string {
	t.Helper()
	dir := writeAliasDir(t, aliasManifest)
	overlay := "aliases:\n  hold: " + holdLine + "\n"
	if err := os.WriteFile(filepath.Join(dir, "berthy.dev.yml"), []byte(overlay), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// twoEnvAliasConfig points demo/dev and demo/prod at separate agents and sets the shared token variable.
func twoEnvAliasConfig(t *testing.T, devURL, prodURL string) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config.json")
	body := fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"},"prod":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, devURL, prodURL)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	return cfg
}

func TestAliasForwardsVersionAndHonorsExplicitEnvironment(t *testing.T) {
	dev, devReqs := aliasAgent(t)
	prod, prodReqs := aliasAgent(t)
	cfg := twoEnvAliasConfig(t, dev.URL, prod.URL)
	t.Chdir(writeAliasDir(t, aliasManifest))
	t.Setenv(agentclient.EnvironmentEnv, "prod")
	var out, errs bytes.Buffer
	code := Run([]string{"console", "--version", "-e", "dev", "--config", cfg}, &out, &errs)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errs.String())
	}
	if len(prodReqs()) != 0 {
		t.Fatalf("prod agent was asked: %v", prodReqs())
	}
	got := devReqs()
	if len(got) != 1 || got[0].method != "POST" || got[0].path != "/v1/projects/demo/environments/dev/exec" {
		t.Fatalf("dev requests %+v", got)
	}
	var body struct {
		Command []string `json:"command"`
	}
	if err := json.Unmarshal(got[0].body, &body); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(body.Command, []string{"bin/console", "--version"}) {
		t.Fatalf("command %q", body.Command)
	}
}

func TestAliasRefusesEnvironmentChangedAfterExpansion(t *testing.T) {
	dev, devReqs := aliasAgent(t)
	prod, prodReqs := aliasAgent(t)
	cfg := twoEnvAliasConfig(t, dev.URL, prod.URL)
	t.Setenv(agentclient.EnvironmentEnv, "dev")
	for _, tc := range []struct {
		name, hold string
		args       []string
	}{
		{"message value is dash-dash", "lock acquire", []string{"hold", "-m", "--", "-e", "prod", "--config", cfg}},
		{"alias ends with message flag", "lock acquire -m", []string{"hold", "--", "-e", "prod", "--config", cfg}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(writeDevOnlyHoldAlias(t, tc.hold))
			var out, errs bytes.Buffer
			code := Run(tc.args, &out, &errs)
			if code == 0 || out.Len() != 0 || !strings.Contains(errs.String(), "loaded for environment dev") {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errs.String())
			}
			if strings.Contains(errs.String(), "Bad!") {
				t.Fatalf("stderr echoes an invalid environment: %q", errs.String())
			}
			if len(devReqs()) != 0 || len(prodReqs()) != 0 {
				t.Fatalf("requests reached an agent: dev %v prod %v", devReqs(), prodReqs())
			}
		})
	}
}

func TestAliasRefusesProjectOrConfigChangedAfterExpansion(t *testing.T) {
	demo, demoReqs := aliasAgent(t)
	other, otherReqs := aliasAgent(t)
	cfg := filepath.Join(t.TempDir(), "both.json")
	body := fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}},"other":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, demo.URL, other.URL)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	t.Setenv(agentclient.EnvironmentEnv, "dev")
	dir := writeDevOnlyHoldAlias(t, "lock acquire -m")
	t.Chdir(dir)
	for _, args := range [][]string{
		{"hold", "--config", cfg, "--", "--project", "other"},
		{"hold", "-e", "dev", "--", "--config", filepath.Join(t.TempDir(), "other.json")},
	} {
		var out, errs bytes.Buffer
		code := Run(args, &out, &errs)
		if code == 0 || out.Len() != 0 || !strings.Contains(errs.String(), "alias hold") {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q", args, code, out.String(), errs.String())
		}
		if len(demoReqs()) != 0 || len(otherReqs()) != 0 {
			t.Fatalf("%v: requests reached an agent", args)
		}
	}
}

// The alias ends in --grep with no value. Lookup leaves "-- --project other" in the tail, and the read parser takes "--" as
// the grep value, so --project other becomes a real selector. The read-side intent check must refuse that.
func TestAliasRefusesProjectChangedThroughReadGrepValue(t *testing.T) {
	demo, demoReqs := aliasAgent(t)
	other, otherReqs := aliasAgent(t)
	cfg := filepath.Join(t.TempDir(), "both.json")
	body := fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}},"other":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, demo.URL, other.URL)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	t.Setenv(agentclient.EnvironmentEnv, "dev")
	t.Chdir(writeAliasDir(t, aliasManifest+"  peek: logs api --grep\n"))
	var out, errs bytes.Buffer
	code := Run([]string{"peek", "--config", cfg, "--", "--project", "other"}, &out, &errs)
	if code == 0 || out.Len() != 0 || !strings.Contains(errs.String(), "alias peek targets a different project") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errs.String())
	}
	if len(demoReqs()) != 0 || len(otherReqs()) != 0 {
		t.Fatalf("requests reached an agent: demo %v other %v", demoReqs(), otherReqs())
	}
}

func TestAliasKeepsInvalidExplicitEnvironmentDiagnostic(t *testing.T) {
	dev, asked := aliasAgent(t)
	cfg := twoEnvAliasConfig(t, dev.URL, "http://127.0.0.1:1")
	t.Chdir(writeDevOnlyHoldAlias(t, "lock acquire -m"))
	var out, errs bytes.Buffer
	code := Run([]string{"hold", "-e", "dev", "--", "-e", "Bad!", "--config", cfg}, &out, &errs)
	if code != 2 || out.Len() != 0 || errs.String() != "valid -e environment required\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errs.String())
	}
	if len(asked()) != 0 {
		t.Fatalf("requests %v", asked())
	}
}

func TestAliasStillTreatsBareDashDashAsTheEndOfOptions(t *testing.T) {
	t.Setenv(agentclient.EnvironmentEnv, "")
	t.Chdir(writeAliasDir(t, aliasManifest))
	_, _, _, ok, err := expandAlias("console", []string{"--", "-e", "dev"})
	if ok || err == nil || !strings.Contains(err.Error(), "an alias of the manifest needs -e") {
		t.Fatalf("ok %v err %v, want a missing -e refusal", ok, err)
	}
}

func TestAliasKeepsAManifestDirDeclaredInItsDefinition(t *testing.T) {
	lookup := filepath.Join(t.TempDir(), "path with spaces", "lookup")
	if err := os.MkdirAll(lookup, 0o700); err != nil {
		t.Fatal(err)
	}
	otherRel := "other-dir"
	if err := os.Mkdir(filepath.Join(lookup, otherRel), 0o700); err != nil {
		t.Fatal(err)
	}
	base := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"
	if err := os.WriteFile(filepath.Join(lookup, "berthy.yml"), []byte(base+"aliases:\n  show-other: config show --manifest-dir "+otherRel+" --json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lookup, otherRel, "berthy.yml"), []byte("project: other\nnetwork: other\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(lookup)
	t.Setenv(agentclient.EnvironmentEnv, "dev")
	t.Setenv(agentclient.ConfigEnv, filepath.Join(t.TempDir(), "missing.json"))
	var out, errs bytes.Buffer
	code := Run([]string{"show-other"}, &out, &errs)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errs.String())
	}
	if !strings.Contains(out.String(), `"project": "other"`) && !strings.Contains(out.String(), `"project":"other"`) {
		t.Fatalf("stdout %q, want project other", out.String())
	}
}

func TestAliasRefusesManifestDirIntroducedOnlyByTheForwardedTail(t *testing.T) {
	cfg := ingressCLIConfig(t, "http://127.0.0.1:1")
	dir := writeDevOnlyHoldAlias(t, "deploy api --version")
	evil := t.TempDir()
	t.Chdir(dir)
	t.Setenv(agentclient.EnvironmentEnv, "dev")
	var out, errs bytes.Buffer
	code := Run([]string{"hold", "--config", cfg, "--", "--manifest-dir", evil}, &out, &errs)
	if code == 0 || out.Len() != 0 || !strings.Contains(errs.String(), "manifest directory") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errs.String())
	}
}

func TestAliasKeepsManifestDirWhenDefinitionEndsInAnotherValueFlag(t *testing.T) {
	lookup := filepath.Join(t.TempDir(), "path with spaces", "lookup")
	if err := os.MkdirAll(lookup, 0o700); err != nil {
		t.Fatal(err)
	}
	chosenRel := "chosen-dir"
	if err := os.Mkdir(filepath.Join(lookup, chosenRel), 0o700); err != nil {
		t.Fatal(err)
	}
	base := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"
	alias := "aliases:\n  ship: deploy api --manifest-dir " + chosenRel + " --version\n"
	if err := os.WriteFile(filepath.Join(lookup, "berthy.yml"), []byte(base+alias), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lookup, chosenRel, "berthy.yml"), []byte("project: chosen\nnetwork: chosen\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server, asked := pathLoggingAgent(t)
	cfg := envCLIConfig(t, server.URL, "dev")
	// Point chosen/dev at the same agent so a successful request proves the declared directory was kept.
	body := fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}},"chosen":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, server.URL, server.URL)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(lookup)
	t.Setenv(agentclient.EnvironmentEnv, "dev")
	var out, errs bytes.Buffer
	code := Run([]string{"ship", "v1", "--config", cfg, "--dry-run"}, &out, &errs)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errs.String())
	}
	got := asked()
	if len(got) == 0 || !strings.Contains(got[0], "/projects/chosen/environments/dev/") {
		t.Fatalf("paths %v, want chosen/dev", got)
	}
}

func TestAliasRefusesIncompleteManifestDirFilledOnlyByTheTail(t *testing.T) {
	cfg := ingressCLIConfig(t, "http://127.0.0.1:1")
	lookup := writeDevOnlyHoldAlias(t, "deploy api --manifest-dir")
	evil := t.TempDir()
	if err := os.WriteFile(filepath.Join(evil, "berthy.yml"), []byte("project: evil\nnetwork: evil\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(lookup)
	t.Setenv(agentclient.EnvironmentEnv, "dev")
	var out, errs bytes.Buffer
	code := Run([]string{"hold", evil, "--config", cfg, "--dry-run"}, &out, &errs)
	if code == 0 || out.Len() != 0 || !strings.Contains(errs.String(), "manifest directory") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errs.String())
	}
}
