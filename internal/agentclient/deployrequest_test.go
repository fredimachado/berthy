package agentclient

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
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

func TestCheckedCapturedOverlayRestoredBeforeStatus(t *testing.T) {
	dir, _ := checkoutRepo(t, map[string]string{"berthy.yml": imageManifest, "berthy.dev.yml": "apps: {}\n"})
	before, err := gitcheck.Capture(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "berthy.dev.yml"), []byte("apps: {}\n# dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.LoadDetailed(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "checkout", "--", "berthy.dev.yml")
	req := deploy.Request{App: "api"}
	co, err := PrepareCapturedDeployFiles(context.Background(), dir, "dev", m, []*deploy.Request{&req}, before)
	if err == nil && (!req.Modified || len(co.Changed) == 0) {
		t.Fatalf("stale overlay attributed clean: %+v %+v", req, co)
	}
}

func TestCapturedComposeRestoredBeforeStatus(t *testing.T) {
	dir, _ := checkoutRepo(t, map[string]string{"berthy.yml": composeManifest, "compose.yaml": "services: {}\n"})
	before, err := gitcheck.Capture(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := manifest.LoadDetailed(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {x: {}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	previous := inspectDeployFiles
	inspectDeployFiles = func(ctx context.Context, dir string, files []string) (gitcheck.Checkout, error) {
		git(t, dir, "checkout", "--", "compose.yaml")
		return previous(ctx, dir, files)
	}
	defer func() { inspectDeployFiles = previous }()
	req := deploy.Request{App: "db"}
	co, err := PrepareCapturedDeployFiles(context.Background(), dir, "dev", loaded, []*deploy.Request{&req}, before)
	if err != nil || !req.Modified || !reflect.DeepEqual(co.Changed, []string{"compose.yaml"}) || req.ComposeFile != "services: {x: {}}\n" {
		t.Fatalf("%+v %+v %v", req, co, err)
	}
}

func TestCapturedOverlayAbsence(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		t.Run(fmt.Sprint(tracked), func(t *testing.T) {
			files := map[string]string{"berthy.yml": imageManifest}
			if tracked {
				files["berthy.dev.yml"] = "apps: {}\n"
			}
			dir, _ := checkoutRepo(t, files)
			before, err := gitcheck.Capture(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			if tracked {
				if err := os.Remove(filepath.Join(dir, "berthy.dev.yml")); err != nil {
					t.Fatal(err)
				}
			}
			loaded, err := manifest.LoadDetailed(dir, "dev")
			if err != nil {
				t.Fatal(err)
			}
			if tracked {
				git(t, dir, "checkout", "--", "berthy.dev.yml")
			}
			req := deploy.Request{App: "api"}
			co, err := PrepareCapturedDeployFiles(context.Background(), dir, "dev", loaded, []*deploy.Request{&req}, before)
			if err != nil || req.Modified != tracked || (len(co.Changed) != 0) != tracked {
				t.Fatalf("%+v %+v %v", req, co, err)
			}
		})
	}
}

func TestCapturedCRLFAndNoGit(t *testing.T) {
	for _, noGit := range []bool{false, true} {
		t.Run(fmt.Sprint(noGit), func(t *testing.T) {
			dir, _ := checkoutRepo(t, map[string]string{"berthy.yml": imageManifest, ".gitattributes": "*.yml text\n"})
			git(t, dir, "config", "core.autocrlf", "true")
			if err := os.Remove(filepath.Join(dir, "berthy.yml")); err != nil {
				t.Fatal(err)
			}
			git(t, dir, "checkout", "--", "berthy.yml")

			if noGit {
				t.Setenv("PATH", t.TempDir())
			}
			before, err := gitcheck.Capture(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := manifest.LoadDetailed(dir, "dev")
			if err != nil {
				t.Fatal(err)
			}
			req := deploy.Request{App: "api", Commit: "explicit"}
			co, err := PrepareCapturedDeployFiles(context.Background(), dir, "dev", loaded, []*deploy.Request{&req}, before)
			if err != nil || req.Modified || req.Commit != "explicit" || len(co.Changed) != 0 {
				t.Fatalf("%+v %+v %v", req, co, err)
			}
		})
	}
}

func TestCapturedInputsInsideRepositorySubdirectory(t *testing.T) {
	dir, _ := checkoutRepo(t, map[string]string{"berthy.yml": imageManifest})
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "berthy.yml"), []byte(imageManifest), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "sub/berthy.yml")
	git(t, dir, "commit", "-q", "-m", "add subdir")
	before, err := gitcheck.Capture(context.Background(), sub)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := manifest.LoadDetailed(sub, "dev")
	if err != nil {
		t.Fatal(err)
	}
	req := deploy.Request{App: "api"}
	co, err := PrepareCapturedDeployFiles(context.Background(), sub, "dev", loaded, []*deploy.Request{&req}, before)
	if err != nil || req.Modified || len(co.Changed) != 0 {
		t.Fatalf("%+v %+v %v", req, co, err)
	}
}

func TestCapturedUntrackedOverlayRemovedBeforeStatus(t *testing.T) {
	dir, _ := checkoutRepo(t, map[string]string{"berthy.yml": imageManifest})
	before, err := gitcheck.Capture(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "berthy.dev.yml"), []byte("apps: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := manifest.LoadDetailed(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "berthy.dev.yml")); err != nil {
		t.Fatal(err)
	}
	req := deploy.Request{App: "api"}
	co, err := PrepareCapturedDeployFiles(context.Background(), dir, "dev", loaded, []*deploy.Request{&req}, before)
	if err != nil || !req.Modified || !reflect.DeepEqual(co.Changed, []string{"berthy.dev.yml"}) {
		t.Fatalf("%+v %+v %v", req, co, err)
	}
}

func TestCapturedIdentOverlayRestoredBeforeStatus(t *testing.T) {
	overlay := "apps:\n  api:\n    env:\n      plain:\n        BUILD: \"$Id$\"\n"
	dir, _ := checkoutRepo(t, map[string]string{"berthy.yml": imageManifest, "berthy.dev.yml": overlay, ".gitattributes": "berthy.dev.yml ident\n"})
	before, err := gitcheck.Capture(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	dirty := strings.Replace(overlay, "$Id$", "$Id: local $", 1)
	if err := os.WriteFile(filepath.Join(dir, "berthy.dev.yml"), []byte(dirty), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := manifest.LoadDetailed(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Manifest.Apps["api"].Env.Plain["BUILD"]; got != "$Id: local $" {
		t.Fatalf("decoded BUILD %q", got)
	}
	if err := os.Remove(filepath.Join(dir, "berthy.dev.yml")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "checkout", "--", "berthy.dev.yml")
	req := deploy.Request{App: "api"}
	co, err := PrepareCapturedDeployFiles(context.Background(), dir, "dev", loaded, []*deploy.Request{&req}, before)
	if err != nil || !req.Modified || !reflect.DeepEqual(co.Changed, []string{"berthy.dev.yml"}) {
		t.Fatalf("%+v %+v %v", req, co, err)
	}
}

func TestCapturedCRLFBlobRestoredCheckout(t *testing.T) {
	crlf := strings.ReplaceAll(imageManifest, "\n", "\r\n")
	dir, _ := checkoutRepo(t, map[string]string{"berthy.yml": crlf})
	git(t, dir, "config", "core.autocrlf", "true")
	if err := os.Remove(filepath.Join(dir, "berthy.yml")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "checkout", "--", "berthy.yml")
	before, err := gitcheck.Capture(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := manifest.LoadDetailed(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if string(loaded.Files["berthy.yml"].Bytes) != crlf {
		t.Fatal("checkout did not preserve CRLF")
	}
	req := deploy.Request{App: "api"}
	co, err := PrepareCapturedDeployFiles(context.Background(), dir, "dev", loaded, []*deploy.Request{&req}, before)
	if err != nil || req.Modified || len(co.Changed) != 0 {
		t.Fatalf("%+v %+v %v", req, co, err)
	}
}

func TestCapturedCheckoutConversionFailureRefuses(t *testing.T) {
	for _, required := range []string{"true", "false"} {
		t.Run(required, func(t *testing.T) {
			dir, _ := checkoutRepo(t, map[string]string{"berthy.yml": imageManifest, ".gitattributes": "berthy.yml filter=broken\n"})
			git(t, dir, "config", "filter.broken.clean", "git --version")
			git(t, dir, "config", "filter.broken.smudge", "git no-such-command")
			git(t, dir, "config", "filter.broken.required", required)
			before, err := gitcheck.Capture(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := manifest.LoadDetailed(dir, "dev")
			if err != nil {
				t.Fatal(err)
			}
			req := deploy.Request{App: "api"}
			_, err = PrepareCapturedDeployFiles(context.Background(), dir, "dev", loaded, []*deploy.Request{&req}, before)
			if err == nil || !strings.Contains(err.Error(), "checkout input conversion unavailable") {
				t.Fatalf("failed checkout conversion: %v", err)
			}
		})
	}
}

func TestCapturedAttributesPinnedToHead(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(fmt.Sprint(staged), func(t *testing.T) {
			overlay := "apps:\n  api:\n    env:\n      plain:\n        BUILD: \"$Id$\"\n"
			dir, _ := checkoutRepo(t, map[string]string{"berthy.yml": imageManifest, "berthy.dev.yml": overlay, ".gitattributes": "*.yml -ident\n"})
			if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.yml ident\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if staged {
				git(t, dir, "add", ".gitattributes")
			}
			if err := os.Remove(filepath.Join(dir, "berthy.dev.yml")); err != nil {
				t.Fatal(err)
			}
			git(t, dir, "checkout", "--", "berthy.dev.yml")
			before, err := gitcheck.Capture(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := manifest.LoadDetailed(dir, "dev")
			if err != nil {
				t.Fatal(err)
			}
			value := loaded.Manifest.Apps["api"].Env.Plain["BUILD"]
			if !strings.HasPrefix(value, "$Id: ") {
				t.Fatalf("ident was not expanded: %q", value)
			}
			if err := os.Remove(filepath.Join(dir, "berthy.dev.yml")); err != nil {
				t.Fatal(err)
			}
			git(t, dir, "checkout", "--", "berthy.dev.yml")
			req := deploy.Request{App: "api"}
			co, err := PrepareCapturedDeployFiles(context.Background(), dir, "dev", loaded, []*deploy.Request{&req}, before)
			if err != nil || !req.Modified || !reflect.DeepEqual(co.Changed, []string{"berthy.dev.yml"}) {
				t.Fatalf("attributes changed: modified %v, paths %v, err %v", req.Modified, co.Changed, err)
			}
		})
	}
}

func TestCapturedCommittedIdentCheckout(t *testing.T) {
	overlay := "apps:\n  api:\n    env:\n      plain:\n        BUILD: \"$Id$\"\n"
	dir, _ := checkoutRepo(t, map[string]string{"berthy.yml": imageManifest, "berthy.dev.yml": overlay, ".gitattributes": "*.yml ident\n"})
	if err := os.Remove(filepath.Join(dir, "berthy.dev.yml")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "checkout", "--", "berthy.dev.yml")
	before, err := gitcheck.Capture(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := manifest.LoadDetailed(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if value := loaded.Manifest.Apps["api"].Env.Plain["BUILD"]; !strings.HasPrefix(value, "$Id: ") {
		t.Fatalf("ident was not expanded: %q", value)
	}
	req := deploy.Request{App: "api"}
	co, err := PrepareCapturedDeployFiles(context.Background(), dir, "dev", loaded, []*deploy.Request{&req}, before)
	if err != nil || req.Modified || len(co.Changed) != 0 {
		t.Fatalf("committed ident: modified %v, paths %v, err %v", req.Modified, co.Changed, err)
	}
}

func TestCapturedRelativeGitConversionConfig(t *testing.T) {
	overlay := "apps:\n  api:\n    env:\n      plain:\n        BUILD: \"$Id$\"\n"
	for _, filter := range []bool{false, true} {
		t.Run(fmt.Sprint(filter), func(t *testing.T) {
			files := map[string]string{"berthy.yml": imageManifest, "berthy.dev.yml": overlay}
			if filter {
				files[".gitattributes"] = "berthy.dev.yml filter=local\n"
			}
			dir, _ := checkoutRepo(t, files)
			if filter {
				if err := os.WriteFile(filepath.Join(dir, "local-filter"), []byte("#!/bin/sh\ncat\n"), 0700); err != nil {
					t.Fatal(err)
				}
				git(t, dir, "config", "filter.local.smudge", "./local-filter")
				git(t, dir, "config", "filter.local.clean", "cat")
				git(t, dir, "config", "filter.local.required", "true")
			} else {
				if err := os.WriteFile(filepath.Join(dir, "local attrs $value"), []byte("berthy.dev.yml ident\n"), 0600); err != nil {
					t.Fatal(err)
				}
				git(t, dir, "config", "core.attributesFile", "local attrs $value")
			}
			if err := os.Remove(filepath.Join(dir, "berthy.dev.yml")); err != nil {
				t.Fatal(err)
			}
			git(t, dir, "checkout", "--", "berthy.dev.yml")
			before, err := gitcheck.Capture(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := manifest.LoadDetailed(dir, "dev")
			if err != nil {
				t.Fatal(err)
			}
			if !filter && !strings.HasPrefix(loaded.Manifest.Apps["api"].Env.Plain["BUILD"], "$Id: ") {
				t.Fatal("relative attributes were not applied")
			}
			req := deploy.Request{App: "api"}
			co, err := PrepareCapturedDeployFiles(context.Background(), dir, "dev", loaded, []*deploy.Request{&req}, before)
			if err != nil || req.Modified || len(co.Changed) != 0 {
				t.Fatalf("conversion: modified %v, paths %v, err %v", req.Modified, co.Changed, err)
			}
		})
	}
}

func TestCapturedDoesNotRunLinkedWorktreeIndexHook(t *testing.T) {
	dir, _ := checkoutRepo(t, map[string]string{"berthy.yml": imageManifest})
	linked := filepath.Join(t.TempDir(), "linked")
	git(t, dir, "worktree", "add", "--detach", linked, "HEAD")
	hooks := git(t, dir, "rev-parse", "--git-path", "hooks")
	if !filepath.IsAbs(hooks) {
		hooks = filepath.Join(dir, hooks)
	}
	marker := filepath.Join(t.TempDir(), "hook-ran")
	script := "#!/bin/sh\nprintf ran > \"" + filepath.ToSlash(marker) + "\"\n"
	if err := os.WriteFile(filepath.Join(hooks, "post-index-change"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	git(t, linked, "read-tree", "HEAD")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control did not run post-index-change: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	indexPath := git(t, linked, "rev-parse", "--git-path", "index")
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(linked, indexPath)
	}
	indexBefore, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	before, err := gitcheck.Capture(context.Background(), linked)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := manifest.LoadDetailed(linked, "dev")
	if err != nil {
		t.Fatal(err)
	}
	req := deploy.Request{App: "api"}
	_, err = PrepareCapturedDeployFiles(context.Background(), linked, "dev", loaded, []*deploy.Request{&req}, before)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("index hook ran during capture: %v", err)
	}
	indexAfter, err := os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(indexBefore, indexAfter) {
		t.Fatalf("source index changed: %v", err)
	}
}

func TestCapturedRelativeProcessRunsInSource(t *testing.T) {
	dir, head := checkoutRepo(t, map[string]string{"berthy.yml": imageManifest, ".gitattributes": "berthy.yml filter=local\n"})
	if err := os.WriteFile(filepath.Join(dir, "local-process"), []byte("#!/bin/sh\n: > process-ran\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "config", "filter.local.process", "./local-process")
	git(t, dir, "config", "filter.local.required", "true")
	_, err := gitcheck.CompareCaptured(context.Background(), dir, head, []gitcheck.CapturedFile{{Path: "berthy.yml", Present: true, Bytes: []byte(imageManifest)}})
	if err == nil {
		t.Fatal("failed process filter was accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "process-ran")); err != nil {
		t.Fatalf("process did not run in source: %v", err)
	}
}

func TestCapturedFilterSeesSourceGitContext(t *testing.T) {
	overlay := "apps:\n  api:\n    env:\n      plain:\n        BUILD: value\n"
	dir, _ := checkoutRepo(t, map[string]string{"berthy.yml": imageManifest, "berthy.dev.yml": overlay, ".gitattributes": "berthy.dev.yml filter=local\n"})
	linked := filepath.Join(t.TempDir(), "linked source")
	git(t, dir, "worktree", "add", "--detach", linked, "HEAD")
	script := "#!/bin/sh\nroot=$(git rev-parse --show-toplevel) || exit 1\ntest \"$(cat \"$root/source-marker\")\" = source || exit 1\ntest -z \"${GIT_INDEX_FILE+x}\" || exit 1\ntest -z \"${GIT_CONFIG_PARAMETERS+x}\" || exit 1\ntest \"$1\" = berthy.dev.yml || exit 1\ncat\n"
	if err := os.WriteFile(filepath.Join(linked, "local-filter"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, "source-marker"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, linked, "config", "filter.local.smudge", "./local-filter %f")
	git(t, linked, "config", "filter.local.clean", "cat")
	git(t, linked, "config", "filter.local.required", "true")
	if err := os.Remove(filepath.Join(linked, "berthy.dev.yml")); err != nil {
		t.Fatal(err)
	}
	git(t, linked, "checkout", "--", "berthy.dev.yml")
	before, err := gitcheck.Capture(context.Background(), linked)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := manifest.LoadDetailed(linked, "dev")
	if err != nil {
		t.Fatal(err)
	}
	req := deploy.Request{App: "api"}
	co, err := PrepareCapturedDeployFiles(context.Background(), linked, "dev", loaded, []*deploy.Request{&req}, before)
	if err != nil || req.Modified || len(co.Changed) != 0 {
		t.Fatalf("filter context: modified %v paths %v err %v", req.Modified, co.Changed, err)
	}
}

func TestCapturedWindowsRootedAttributesFile(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows rooted paths")
	}
	overlay := "apps:\n  api:\n    env:\n      plain:\n        BUILD: \"$Id$\"\n"
	dir, _ := checkoutRepo(t, map[string]string{"berthy.yml": imageManifest, "berthy.dev.yml": overlay})
	attrs := filepath.Join(t.TempDir(), "attrs")
	if !strings.EqualFold(filepath.VolumeName(attrs), filepath.VolumeName(dir)) {
		t.Skip("fixture is on another drive")
	}
	if err := os.WriteFile(attrs, []byte("berthy.dev.yml ident\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rooted := filepath.ToSlash(strings.TrimPrefix(attrs, filepath.VolumeName(attrs)))
	for name, value := range map[string]string{"slash": rooted, "backslash": strings.ReplaceAll(rooted, "/", `\`)} {
		t.Run(name, func(t *testing.T) {
			git(t, dir, "config", "core.attributesFile", value)
			if err := os.Remove(filepath.Join(dir, "berthy.dev.yml")); err != nil {
				t.Fatal(err)
			}
			git(t, dir, "checkout", "--", "berthy.dev.yml")
			before, err := gitcheck.Capture(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := manifest.LoadDetailed(dir, "dev")
			if err != nil {
				t.Fatal(err)
			}
			if value := loaded.Manifest.Apps["api"].Env.Plain["BUILD"]; !strings.HasPrefix(value, "$Id: ") {
				t.Fatalf("rooted attributes did not apply: %q", value)
			}
			req := deploy.Request{App: "api"}
			co, err := PrepareCapturedDeployFiles(context.Background(), dir, "dev", loaded, []*deploy.Request{&req}, before)
			if err != nil || req.Modified || len(co.Changed) != 0 {
				t.Fatalf("rooted attributes: modified %v paths %v err %v", req.Modified, co.Changed, err)
			}
		})
	}
}
