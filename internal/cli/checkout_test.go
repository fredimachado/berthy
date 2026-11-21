package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// digest is a version that a protected environment accepts.
const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// checkoutRepo makes a git repository in a temporary directory with the files committed, and returns the directory and HEAD.
// The config of the tests lives outside it, so it stays clean. The test is skipped when git is not installed.
func checkoutRepo(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	gitRunIn(t, dir, "init", "-q")
	gitRunIn(t, dir, "config", "user.email", "test@example.com")
	gitRunIn(t, dir, "config", "user.name", "Test")
	gitRunIn(t, dir, "config", "core.autocrlf", "false")
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	gitRunIn(t, dir, "add", "-A")
	gitRunIn(t, dir, "commit", "-q", "-m", "manifest")
	return dir, gitRunIn(t, dir, "rev-parse", "HEAD")
}

func gitRunIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out.String())
	}
	return strings.TrimSpace(out.String())
}

// checkoutFixture is deployAllFixture for a git checkout whose manifest is committed. It returns the checkout's HEAD too.
func checkoutFixture(t *testing.T, manifestYAML string, agent *fakeDeployAgent) (dir, cfg, head string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(agent.serve))
	t.Cleanup(server.Close)
	dir, head = checkoutRepo(t, map[string]string{"berthy.yml": manifestYAML})
	cfg = filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, server.URL)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	return dir, cfg, head
}

// editFile appends a line to a file of dir, so the checkout has a local change to it.
func editFile(t *testing.T, dir, name string) {
	t.Helper()
	p := filepath.Join(dir, name)
	old, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p, append(old, []byte("# local edit\n")...), 0600); err != nil {
		t.Fatal(err)
	}
}

// deployApp runs `berthy deploy APP` against the fixture.
func deployApp(t *testing.T, dir, cfg string, args ...string) (code int, out, errs string) {
	t.Helper()
	var o, e bytes.Buffer
	code = Run(append([]string{"deploy", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, args...), &o, &e)
	return code, o.String(), e.String()
}

// sentDeploy is the request of the one deploy the agent received.
func sentDeploy(t *testing.T, agent *fakeDeployAgent) map[string]any {
	t.Helper()
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.deploys) != 1 {
		t.Fatalf("deploys sent: %d", len(agent.deploys))
	}
	return agent.deploys[0]
}

func TestDeployOfAModifiedCheckoutWarnsAndSendsTheFlag(t *testing.T) {
	agent := &fakeDeployAgent{}
	dir, cfg, head := checkoutFixture(t, allAppsManifest, agent)
	editFile(t, dir, "berthy.yml")
	code, _, errs := deployApp(t, dir, cfg, "api", "--version", "v1.0.2")
	if code != 0 || !strings.Contains(errs, "warning: the manifest has local changes: berthy.yml\n") {
		t.Fatalf("deploy of a modified checkout: %d %q", code, errs)
	}
	sent := sentDeploy(t, agent)
	if sent["modified"] != true || sent["commit"] != head {
		t.Fatalf("request %v, want commit %s and modified", sent, head)
	}
}

func TestDeployOfACleanCheckoutRecordsHeadWithoutWarning(t *testing.T) {
	agent := &fakeDeployAgent{}
	dir, cfg, head := checkoutFixture(t, allAppsManifest, agent)
	code, _, errs := deployApp(t, dir, cfg, "api", "--version", "v1.0.2")
	if code != 0 || errs != "" {
		t.Fatalf("clean deploy: %d %q", code, errs)
	}
	sent := sentDeploy(t, agent)
	if _, flagged := sent["modified"]; flagged || sent["commit"] != head {
		t.Fatalf("request %v, want commit %s and no flag", sent, head)
	}
}

func TestDeployOutsideAGitRepositoryIsUnchanged(t *testing.T) {
	agent := &fakeDeployAgent{}
	dir, cfg := deployAllFixture(t, allAppsManifest, agent)
	code, _, errs := deployApp(t, dir, cfg, "api", "--version", "v1.0.2")
	if code != 0 || errs != "" {
		t.Fatalf("deploy outside git: %d %q", code, errs)
	}
	sent := sentDeploy(t, agent)
	if _, flagged := sent["modified"]; flagged || sent["commit"] != "" {
		t.Fatalf("request %v, want no commit and no flag", sent)
	}
}

func TestDeployWithACommitThatIsNotHeadWarnsAndKeepsIt(t *testing.T) {
	agent := &fakeDeployAgent{}
	dir, cfg, head := checkoutFixture(t, allAppsManifest, agent)
	code, _, errs := deployApp(t, dir, cfg, "api", "--version", "v1.0.2", "--commit", "deadbeef")
	if code != 0 || !strings.Contains(errs, "warning: --commit deadbeef is not HEAD ("+head+")\n") {
		t.Fatalf("given commit: %d %q", code, errs)
	}
	if sent := sentDeploy(t, agent); sent["commit"] != "deadbeef" {
		t.Fatalf("request %v, want the given commit", sent)
	}
}

func TestDeployOnAProtectedEnvironmentRefusesLocalChangesBeforeSendingAnything(t *testing.T) {
	const want = "the manifest has local changes: commit them first: berthy.yml"
	t.Run("by the manifest", func(t *testing.T) {
		agent := &fakeDeployAgent{}
		dir, cfg, _ := checkoutFixture(t, allAppsManifest+"protected: true\n", agent)
		editFile(t, dir, "berthy.yml")
		code, _, errs := deployApp(t, dir, cfg, "api", "--version", digest)
		if code != 1 || !strings.Contains(errs, want) || len(agent.apps()) != 0 {
			t.Fatalf("protected manifest: %d %q, sent %v", code, errs, agent.apps())
		}
	})
	t.Run("by the agent", func(t *testing.T) {
		agent := &fakeDeployAgent{protected: true}
		dir, cfg, _ := checkoutFixture(t, allAppsManifest, agent)
		editFile(t, dir, "berthy.yml")
		code, _, errs := deployApp(t, dir, cfg, "api", "--version", digest)
		if code != 1 || !strings.Contains(errs, want) || len(agent.apps()) != 0 {
			t.Fatalf("protected agent: %d %q, sent %v", code, errs, agent.apps())
		}
	})
	t.Run("clean", func(t *testing.T) {
		agent := &fakeDeployAgent{}
		dir, cfg, _ := checkoutFixture(t, allAppsManifest+"protected: true\n", agent)
		code, _, errs := deployApp(t, dir, cfg, "api", "--version", digest)
		if code != 0 || strings.Contains(errs, "local changes") || len(agent.apps()) != 1 {
			t.Fatalf("clean protected deploy: %d %q", code, errs)
		}
	})
}

func TestDeployAllWarnsOnceAndFlagsEveryDeploy(t *testing.T) {
	agent := &fakeDeployAgent{}
	dir, cfg, head := checkoutFixture(t, allAppsManifest, agent)
	editFile(t, dir, "berthy.yml")
	code, _, errs := deployAllCommand(t, dir, cfg, "--version", "api=v1.0.2")
	if code != 0 || strings.Count(errs, "warning: the manifest has local changes: berthy.yml") != 1 {
		t.Fatalf("deploy without an app: %d %q", code, errs)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.deploys) != 3 {
		t.Fatalf("deploys sent: %d", len(agent.deploys))
	}
	for _, sent := range agent.deploys {
		if sent["modified"] != true || sent["commit"] != head {
			t.Fatalf("request %v, want commit %s and modified", sent, head)
		}
	}
}

func TestDeployAllOnAProtectedEnvironmentRefusesBeforeAnyRequest(t *testing.T) {
	agent := &fakeDeployAgent{}
	dir, cfg, _ := checkoutFixture(t, allAppsManifest+"protected: true\n", agent)
	editFile(t, dir, "berthy.yml")
	code, _, errs := deployAllCommand(t, dir, cfg, "--version", "api="+digest)
	if code != 1 || !strings.Contains(errs, "the manifest has local changes: commit them first: berthy.yml") {
		t.Fatalf("deploy without an app on a protected environment: %d %q", code, errs)
	}
	if len(agent.apps()) != 0 || len(agent.reads) != 0 {
		t.Fatalf("requests sent: deploys %v, reads %v", agent.apps(), agent.reads)
	}
}

func TestIngressApplyOnAProtectedEnvironmentRefusesLocalChanges(t *testing.T) {
	agent := &fakeDeployAgent{}
	dir, cfg, _ := checkoutFixture(t, allAppsManifest+"protected: true\n", agent)
	editFile(t, dir, "berthy.yml")
	var out, errs bytes.Buffer
	code := Run([]string{"ingress", "apply", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errs)
	if code != 1 || !strings.Contains(errs.String(), "the manifest has local changes: commit them first: berthy.yml") {
		t.Fatalf("ingress apply: %d %q", code, errs.String())
	}
	if len(agent.reads) != 0 {
		t.Fatalf("requests sent: %v", agent.reads)
	}
}

// historyFixture serves the history of app api: a release from a modified checkout, and one from a clean one.
func historyFixture(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/environments/dev/history/api" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `[{"id":2,"app":"api","digest":"d2","commit":"abc1234","created":"2026-11-18T10:00:00Z","modified":true},`+
			`{"id":1,"app":"api","digest":"d1","commit":"def5678","created":"2026-11-17T10:00:00Z"}]`)
	}))
	t.Cleanup(server.Close)
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, server.URL)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	return cfg
}

func TestHistoryShowsTheLocalChangesNextToTheCommit(t *testing.T) {
	cfg := historyFixture(t)
	var out, errs bytes.Buffer
	if code := Run([]string{"history", "api", "-e", "dev", "--config", cfg}, &out, &errs); code != 0 {
		t.Fatalf("history: %d %q", code, errs.String())
	}
	text := out.String()
	if !strings.Contains(text, "commit: abc1234 (modified)") || !strings.Contains(text, "commit: def5678\n") || strings.Contains(text, "def5678 (modified)") {
		t.Fatalf("history text:\n%s", text)
	}
	out.Reset()
	if code := Run([]string{"history", "api", "-e", "dev", "--config", cfg, "--json"}, &out, &errs); code != 0 {
		t.Fatalf("history --json: %d %q", code, errs.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil || len(rows) != 2 || rows[0]["modified"] != true || rows[0]["commit"] != "abc1234" {
		t.Fatalf("history --json: %v\n%s", err, out.String())
	}
	if _, flagged := rows[1]["modified"]; flagged {
		t.Fatalf("a clean row carries the flag: %v", rows[1])
	}
}
