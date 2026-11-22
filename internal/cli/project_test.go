package cli

import (
	"berthy/internal/agentclient"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// twoProjectsServers starts one agent for each of two projects that both have a prod environment, and returns the CLI
// configuration that names both and a counter for each agent's requests.
func twoProjectsServers(t *testing.T) (cfg string, cardio, slice *atomic.Int32) {
	t.Helper()
	serve := func(n *atomic.Int32) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			fmt.Fprint(w, `[]`)
		}))
		t.Cleanup(s.Close)
		return s
	}
	cardio, slice = new(atomic.Int32), new(atomic.Int32)
	a, b := serve(cardio), serve(slice)
	t.Setenv("BERTHY_TEST_CT", "ct")
	t.Setenv("BERTHY_TEST_SU", "su")
	cfg = filepath.Join(t.TempDir(), "config.json")
	body := fmt.Sprintf(`{"projects":{"cardiotennis":{"environments":{"prod":{"url":%q,"token_env":"BERTHY_TEST_CT"}}},"sliceup":{"environments":{"prod":{"url":%q,"token_env":"BERTHY_TEST_SU"}}}}}`, a.URL, b.URL)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, cardio, slice
}

func projectDirOf(t *testing.T, project string) string {
	t.Helper()
	dir := t.TempDir()
	text := "project: " + project + "\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestACommandReachesTheAgentOfTheProjectOfItsDirectory(t *testing.T) {
	cfg, cardio, slice := twoProjectsServers(t)
	// A remote command that reads no manifest looks in the current directory.
	t.Chdir(projectDirOf(t, "sliceup"))
	var out, errout bytes.Buffer
	if code := Run([]string{"ps", "-e", "prod", "--config", cfg}, &out, &errout); code != 0 {
		t.Fatalf("ps: exit %d: %s", code, errout.String())
	}
	if cardio.Load() != 0 || slice.Load() != 1 {
		t.Fatalf("ps reached cardiotennis %d times and sliceup %d times", cardio.Load(), slice.Load())
	}
	// A change command looks in its --manifest-dir.
	out.Reset()
	errout.Reset()
	Run([]string{"secret", "list", "-e", "prod", "--config", cfg, "--manifest-dir", projectDirOf(t, "cardiotennis")}, &out, &errout)
	if cardio.Load() != 1 || slice.Load() != 1 {
		t.Fatalf("secret list reached cardiotennis %d times and sliceup %d times\n%s", cardio.Load(), slice.Load(), errout.String())
	}
}

func TestACommandWithAnEnvironmentOfSeveralProjectsIsRefusedBeforeAnyRequest(t *testing.T) {
	cfg, cardio, slice := twoProjectsServers(t)
	t.Chdir(t.TempDir())
	for _, args := range [][]string{
		{"ps", "-e", "prod", "--config", cfg},
		{"secret", "list", "-e", "prod", "--config", cfg},
	} {
		var out, errout bytes.Buffer
		code := Run(args, &out, &errout)
		if want := "environment prod exists in several projects (cardiotennis, sliceup): give --project, or run the command in the project's directory"; code != 1 || !strings.Contains(errout.String(), want) {
			t.Errorf("%v: exit %d: %s", args, code, errout.String())
		}
	}
	if cardio.Load() != 0 || slice.Load() != 0 {
		t.Fatalf("a refused command sent %d and %d requests", cardio.Load(), slice.Load())
	}
}

func TestAnAliasFoundThroughManifestDirReachesTheProjectOfThatDirectory(t *testing.T) {
	cfg, cardio, slice := twoProjectsServers(t)
	sliceDir := writeAliasDir(t, strings.Replace(aliasManifest, "project: demo", "project: sliceup", 1)+"  list: ps\n")
	// The person is in the checkout of another project, whose prod is not the one the alias means.
	t.Chdir(projectDirOf(t, "cardiotennis"))
	// "list" is a read command and "hist" a deployment command that takes no --manifest-dir: the expanded line has none.
	for i, alias := range []string{"list", "hist"} {
		var out, errout bytes.Buffer
		if code := Run([]string{alias, "-e", "prod", "--config", cfg, "--manifest-dir", sliceDir}, &out, &errout); code != 0 {
			t.Fatalf("%s: exit %d: %s", alias, code, errout.String())
		}
		if cardio.Load() != 0 || slice.Load() != int32(i+1) {
			t.Fatalf("%s reached cardiotennis %d times and sliceup %d times", alias, cardio.Load(), slice.Load())
		}
	}
}

func TestAReadCommandRefusesAnInvalidEnvironmentLikeTheOthers(t *testing.T) {
	cfg, cardio, slice := twoProjectsServers(t)
	for _, cmd := range []string{"ps", "doctor", "logs"} {
		var out, errout bytes.Buffer
		code := Run([]string{cmd, "api", "-e", "prod_1", "--config", cfg}, &out, &errout)
		if code != 2 || errout.String() != "valid -e environment required\n" || out.Len() != 0 {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", cmd, code, out.String(), errout.String())
		}
	}
	if cardio.Load() != 0 || slice.Load() != 0 {
		t.Fatalf("a refused command sent requests")
	}
}

func TestProjectFlagPicksTheAgentOfThatProject(t *testing.T) {
	cfg, cardio, slice := twoProjectsServers(t)
	t.Chdir(projectDirOf(t, "cardiotennis"))
	for _, args := range [][]string{{"ps", "-e", "prod", "--project", "sliceup", "--config", cfg}, {"history", "api", "-e", "prod", "--project", "sliceup", "--config", cfg}} {
		var out, errout bytes.Buffer
		if code := Run(args, &out, &errout); code != 0 {
			t.Errorf("%v: exit %d: %s", args, code, errout.String())
		}
	}
	if cardio.Load() != 0 || slice.Load() != 2 {
		t.Fatalf("requests: %d, %d", cardio.Load(), slice.Load())
	}
}

func TestProjectFlagRejectsInvalidNames(t *testing.T) {
	cfg, cardio, slice := twoProjectsServers(t)
	for _, cmd := range []string{"ps", "history"} {
		var out, errout bytes.Buffer
		if code := Run([]string{cmd, "-e", "prod", "--project", "Bad_Name", "--config", cfg}, &out, &errout); code != 2 {
			t.Errorf("%s: exit %d: %s", cmd, code, errout.String())
		}
	}
	if cardio.Load() != 0 || slice.Load() != 0 {
		t.Fatal("sent request")
	}
}

func TestAnAliasKeepsTheProjectFlag(t *testing.T) {
	cfg, cardio, slice := twoProjectsServers(t)
	dir := writeAliasDir(t, strings.Replace(aliasManifest, "project: demo", "project: cardiotennis", 1)+"  list: ps\n")
	var out, errout bytes.Buffer
	if code := Run([]string{"list", "-e", "prod", "--project", "sliceup", "--config", cfg, "--manifest-dir", dir}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if cardio.Load() != 0 || slice.Load() != 1 {
		t.Fatalf("requests: %d, %d", cardio.Load(), slice.Load())
	}
}

func TestDeployRefusesAManifestOfAnotherProjectThanTheFlag(t *testing.T) {
	cfg, cardio, slice := twoProjectsServers(t)
	dir := projectDirOf(t, "cardiotennis")
	for _, args := range [][]string{
		{"deploy", "api", "--version", "v1"},
		{"deploy"},
		{"apply"},
		{"ingress", "apply"},
	} {
		args = append(args, "-e", "prod", "--project", "sliceup", "--config", cfg, "--manifest-dir", dir)
		var out, errout bytes.Buffer
		if code := Run(args, &out, &errout); code != 2 || !strings.Contains(errout.String(), "is for project cardiotennis, not sliceup") {
			t.Errorf("%v: exit %d: %s", args, code, errout.String())
		}
	}
	if cardio.Load() != 0 || slice.Load() != 0 {
		t.Fatalf("sent %d and %d requests", cardio.Load(), slice.Load())
	}
}

func TestAFollowedStreamAgainstAnAgentWithoutProjectsSaysToUpgrade(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/environments/dev/agent" {
			w.WriteHeader(200)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	cfg := ingressCLIConfig(t, srv.URL)
	var out, errout bytes.Buffer
	code := Run([]string{"logs", "api", "--follow", "-e", "dev", "--config", cfg}, &out, &errout)
	if code == 0 || !strings.Contains(errout.String(), "the agent runs an older release than this CLI") {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
}

func TestOperationStreamAgainstAnAgentWithoutProjectsSaysToUpgrade(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/environments/dev/agent" {
			w.WriteHeader(200)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	var out, errout bytes.Buffer
	prefix := "/v1/projects/demo/environments/dev/"
	code := watchOperation(context.Background(), agentclient.NewHTTPClient(), u, "token", prefix, "id", prefix+"operations/id/events", &out, &errout, false, "deploy", "dev", "demo")
	if code == 0 || !strings.Contains(errout.String(), agentclient.OlderAgentMessage) {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
}

func TestAnEmptyProjectFlagIsInvalid(t *testing.T) {
	cfg, cardio, slice := twoProjectsServers(t)
	for _, cmd := range []string{"ps", "history"} {
		var out, errout bytes.Buffer
		if code := Run([]string{cmd, "-e", "prod", "--project", "", "--config", cfg}, &out, &errout); code != 2 {
			t.Errorf("%s: exit %d: %s", cmd, code, errout.String())
		}
	}
	if cardio.Load() != 0 || slice.Load() != 0 {
		t.Fatal("sent request")
	}
}

func TestMalformedManifestReportsLoadErrorInsteadOfProjectMismatch(t *testing.T) {
	cfg, cardio, slice := twoProjectsServers(t)
	dir := writeAliasDir(t, "project: [\n")
	for _, args := range [][]string{{"deploy", "api", "--version", "v1"}, {"deploy"}, {"apply"}, {"ingress", "apply"}} {
		args = append(args, "-e", "prod", "--project", "sliceup", "--config", cfg, "--manifest-dir", dir)
		var out, errs bytes.Buffer
		code := Run(args, &out, &errs)
		if code != 1 || !strings.Contains(errs.String(), "yaml:") || strings.Contains(errs.String(), "is for project") {
			t.Errorf("%v: exit %d, stderr %q", args, code, errs.String())
		}
	}
	if cardio.Load() != 0 || slice.Load() != 0 {
		t.Fatal("sent request for malformed manifest")
	}
}

func TestMalformedManifestWithoutProjectFlagReportsLoadError(t *testing.T) {
	cfg, cardio, slice := twoProjectsServers(t)
	dir := writeAliasDir(t, "project: [\n")
	var out, errs bytes.Buffer
	code := Run([]string{"apply", "-e", "prod", "--config", cfg, "--manifest-dir", dir}, &out, &errs)
	if code != 1 || !strings.Contains(errs.String(), "yaml:") {
		t.Fatalf("exit %d: %q", code, errs.String())
	}
	if cardio.Load() != 0 || slice.Load() != 0 {
		t.Fatal("sent request")
	}
}
