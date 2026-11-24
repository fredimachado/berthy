package agentclient

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"berthy/internal/deploy"
	"berthy/internal/gitcheck"
	"berthy/internal/manifest"
)

// checkoutRepo makes a git repository holding the files, committed, and returns its directory and HEAD. The test is skipped
// when git is not installed.
func checkoutRepo(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "Test")
	git(t, dir, "config", "core.autocrlf", "false")
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "manifest")
	return dir, git(t, dir, "rev-parse", "HEAD")
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out.String())
	}
	return strings.TrimSpace(out.String())
}

func TestPrepareDeployRecordsHeadOfACleanCheckout(t *testing.T) {
	dir, head := checkoutRepo(t, map[string]string{"berthy.yml": imageManifest})
	req := deploy.Request{App: "api", Version: "1.0"}
	co, err := PrepareDeploy(dir, "dev", &req)
	if err != nil || req.Commit != head || req.Modified || !co.InWorkTree || len(co.Changed) != 0 {
		t.Fatalf("clean checkout: %v %+v %+v", err, req, co)
	}
}

func TestPrepareDeployMarksAnEditedManifest(t *testing.T) {
	dir, head := checkoutRepo(t, map[string]string{"berthy.yml": imageManifest})
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(imageManifest+"# edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	req := deploy.Request{App: "api", Version: "1.0"}
	co, err := PrepareDeploy(dir, "dev", &req)
	if err != nil || req.Commit != head || !req.Modified || !reflect.DeepEqual(co.Changed, []string{"berthy.yml"}) {
		t.Fatalf("edited manifest: %v %+v %+v", err, req, co)
	}
}

func TestPrepareDeployKeepsAGivenCommit(t *testing.T) {
	dir, head := checkoutRepo(t, map[string]string{"berthy.yml": imageManifest})
	req := deploy.Request{App: "api", Version: "1.0", Commit: "deadbeef"}
	if _, err := PrepareDeploy(dir, "dev", &req); err != nil || req.Commit != "deadbeef" || req.Modified {
		t.Fatalf("given commit: %v %+v (HEAD is %s)", err, req, head)
	}
}

func TestPrepareDeployChecksTheOverlayAndTheComposeFileOfTheApp(t *testing.T) {
	dir, _ := checkoutRepo(t, map[string]string{"berthy.yml": composeManifest, "compose.yaml": "services: {}\n"})
	if err := os.WriteFile(filepath.Join(dir, "berthy.dev.yml"), []byte("apps: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	req := deploy.Request{App: "db"}
	co, err := PrepareDeploy(dir, "dev", &req)
	if err != nil || !req.Modified || !reflect.DeepEqual(co.Changed, []string{"berthy.dev.yml"}) {
		t.Fatalf("untracked overlay: %v %+v %+v", err, req, co)
	}
	if err = os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {x: {}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	req = deploy.Request{App: "db"}
	co, err = PrepareDeploy(dir, "dev", &req)
	if err != nil || !reflect.DeepEqual(co.Changed, []string{"berthy.dev.yml", "compose.yaml"}) || req.ComposeFile != "services: {x: {}}\n" {
		t.Fatalf("compose file: %v %+v %+v", err, req, co)
	}
}

func TestPrepareDeployOutsideAWorkTreeChangesNothing(t *testing.T) {
	dir := writeComposeProject(t, imageManifest, "")
	req := deploy.Request{App: "api", Version: "1.0"}
	co, err := PrepareDeploy(dir, "dev", &req)
	if err != nil || req.Commit != "" || req.Modified || co.InWorkTree {
		t.Fatalf("outside a work tree: %v %+v %+v", err, req, co)
	}
}

func TestPrepareDeployFilesAttributesComposeChanges(t *testing.T) {
	dir, _ := checkoutRepo(t, map[string]string{"berthy.yml": strings.Replace(composeManifest, "compose.yaml", "./compose.yaml", 1), "compose.yaml": "services: {}\n"})
	m, err := manifest.Load(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	m.Apps["api"] = manifest.App{Kind: "image", Image: "example/api"}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {x: {}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	image := deploy.Request{App: "api"}
	compose := deploy.Request{App: "db"}
	co, err := PrepareDeployFiles(context.Background(), dir, "dev", m, []*deploy.Request{&image, &compose})
	if err != nil || image.Modified || !compose.Modified || compose.ComposeFile != "services: {x: {}}\n" || len(co.Changed) != 1 {
		t.Fatalf("preparation: %+v %+v %+v %v", image, compose, co, err)
	}
	strict := deploy.Request{App: "db"}
	co, err = PrepareDeploy(dir, "dev", &strict)
	if err != nil || !strict.Modified || strict.ComposeFile != "services: {x: {}}\n" || len(co.Changed) != 1 {
		t.Fatalf("strict preparation: %+v %+v %v", strict, co, err)
	}
}

func TestPrepareDeployFilesCheckedKeepsStableCheckoutFacts(t *testing.T) {
	for _, tc := range []struct {
		name          string
		unborn, noGit bool
	}{{"clean", false, false}, {"unborn", true, false}, {"no git", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir, head := checkoutRepo(t, map[string]string{"berthy.yml": imageManifest})
			if tc.unborn {
				git(t, dir, "update-ref", "-d", "HEAD")
				head = ""
			}
			if tc.noGit {
				t.Setenv("PATH", t.TempDir())
				head = ""
			}
			before, err := gitcheck.Capture(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			m, err := manifest.Load(dir, "dev")
			if err != nil {
				t.Fatal(err)
			}
			req := deploy.Request{App: "api", Version: "v1", Commit: "explicit"}
			co, err := PrepareDeployFilesChecked(context.Background(), dir, "dev", m, []*deploy.Request{&req}, before)
			if err != nil || co.Head != head || req.Commit != "explicit" || co.InWorkTree == tc.noGit {
				t.Fatalf("checkout %+v request %+v err %v", co, req, err)
			}
		})
	}
}

func TestPrepareDeployFilesCheckedRejectsWorktreeReplacement(t *testing.T) {
	parent := t.TempDir()
	dir, head := checkoutRepo(t, map[string]string{"berthy.yml": imageManifest})
	before, err := gitcheck.Capture(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "moved")
	if err := os.Rename(dir, moved); err != nil {
		t.Skipf("cannot move open worktree: %v", err)
	}
	git(t, parent, "clone", "-q", moved, dir)
	if got := git(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatal("replacement checkout has wrong revision")
	}
	req := deploy.Request{App: "api", Version: "v1"}
	if _, err := PrepareDeployFilesChecked(context.Background(), dir, "dev", m, []*deploy.Request{&req}, before); err == nil || !strings.Contains(err.Error(), "checkout changed") {
		t.Fatalf("replacement not refused: %v", err)
	}
}

func TestPrepareDeployFilesCheckedRejectsCheckoutAppearing(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(imageManifest), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := gitcheck.Capture(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q")
	req := deploy.Request{App: "api", Version: "v1"}
	if _, err := PrepareDeployFilesChecked(context.Background(), dir, "dev", m, []*deploy.Request{&req}, before); err == nil || !strings.Contains(err.Error(), "checkout changed") {
		t.Fatalf("new checkout not refused: %v", err)
	}
}

func TestPrepareDeployFilesCheckedRejectsSwitchDuringStatus(t *testing.T) {
	dir, headA := checkoutRepo(t, map[string]string{"berthy.yml": imageManifest})
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(imageManifest+"# second commit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "berthy.yml")
	git(t, dir, "commit", "-q", "-m", "second")
	headB := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "checkout", "-q", "--detach", headA)
	before, err := gitcheck.Capture(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	previous := inspectDeployFiles
	inspectDeployFiles = func(ctx context.Context, dir string, files []string) (gitcheck.Checkout, error) {
		git(t, dir, "checkout", "-q", "--detach", headB)
		co, err := previous(ctx, dir, files)
		git(t, dir, "checkout", "-q", "--detach", headA)
		return co, err
	}
	defer func() { inspectDeployFiles = previous }()
	req := deploy.Request{App: "api", Version: "v1"}
	if _, err := PrepareDeployFilesChecked(context.Background(), dir, "dev", m, []*deploy.Request{&req}, before); err == nil || !strings.Contains(err.Error(), "checkout changed") {
		t.Fatalf("checkout switch was not refused: %v", err)
	}
}
