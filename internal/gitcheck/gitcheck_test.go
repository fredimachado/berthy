package gitcheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// newRepo makes a repository in a temporary directory with one identity, so commits need no global config.
func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "config", "user.email", "test@example.com")
	gitIn(t, dir, "config", "user.name", "Test")
	gitIn(t, dir, "config", "core.autocrlf", "false")
	return dir
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out.String())
	}
	return strings.TrimSpace(out.String())
}

// write puts content into dir/name, creating its directories.
func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// commit records every file of dir in one commit and returns its full id.
func commit(t *testing.T, dir string) string {
	t.Helper()
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "change")
	return gitIn(t, dir, "rev-parse", "HEAD")
}

func inspect(t *testing.T, dir string, files ...string) Checkout {
	t.Helper()
	co, err := Inspect(context.Background(), dir, files)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	return co
}

func TestOutsideAWorkTreeReportsNoCheckout(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "berthy.yml", "project: demo\n")
	co := inspect(t, dir, "berthy.yml")
	if co.InWorkTree || co.Head != "" || co.Changed != nil {
		t.Fatalf("outside a work tree: %+v", co)
	}
}

func TestMissingGitReportsNoCheckout(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "berthy.yml", "project: demo\n")
	commit(t, dir)
	write(t, dir, "berthy.yml", "project: edited\n")
	t.Setenv("PATH", t.TempDir())
	co := inspect(t, dir, "berthy.yml")
	if co.InWorkTree || co.Head != "" || co.Changed != nil {
		t.Fatalf("without git: %+v", co)
	}
}

func TestCleanCheckoutReportsHeadAndNoChanges(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "berthy.yml", "project: demo\n")
	write(t, dir, "notes.txt", "scratch\n")
	head := commit(t, dir)
	co := inspect(t, dir, "berthy.yml", "berthy.prod.yml")
	if !co.InWorkTree || co.Head != head || len(co.Changed) != 0 {
		t.Fatalf("clean checkout: %+v, want head %s", co, head)
	}
}

func TestEditedOverlayIsChanged(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "berthy.yml", "project: demo\n")
	write(t, dir, "berthy.prod.yml", "env: {plain: {A: \"1\"}}\n")
	commit(t, dir)
	write(t, dir, "berthy.prod.yml", "env: {plain: {A: \"2\"}}\n")
	co := inspect(t, dir, "berthy.yml", "berthy.prod.yml")
	if !reflect.DeepEqual(co.Changed, []string{"berthy.prod.yml"}) {
		t.Fatalf("changed = %v, want the overlay only", co.Changed)
	}
}

func TestUntrackedComposeFileIsChanged(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "berthy.yml", "project: demo\n")
	commit(t, dir)
	write(t, dir, "compose.yaml", "services: {}\n")
	co := inspect(t, dir, "berthy.yml", "compose.yaml")
	if !reflect.DeepEqual(co.Changed, []string{"compose.yaml"}) {
		t.Fatalf("changed = %v, want the untracked Compose file", co.Changed)
	}
}

func TestUnrelatedChangesAreNotReported(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "berthy.yml", "project: demo\n")
	write(t, dir, "README.md", "readme\n")
	commit(t, dir)
	write(t, dir, "README.md", "edited\n")
	write(t, dir, "scratch.txt", "new\n")
	co := inspect(t, dir, "berthy.yml")
	if !co.InWorkTree || len(co.Changed) != 0 {
		t.Fatalf("unrelated changes reported: %+v", co)
	}
}

func TestPathsAreRelativeToTheManifestDir(t *testing.T) {
	root := newRepo(t)
	manifestDir := filepath.Join(root, "deploy", "prod")
	write(t, root, "berthy.yml", "project: root\n")
	write(t, manifestDir, "berthy.yml", "project: demo\n")
	write(t, manifestDir, "compose/app.yaml", "services: {}\n")
	commit(t, root)
	// A change at the repository root, outside the manifest directory, is not the manifest's change.
	write(t, root, "berthy.yml", "project: edited\n")
	co := inspect(t, manifestDir, "berthy.yml", "compose/app.yaml")
	if !co.InWorkTree || len(co.Changed) != 0 {
		t.Fatalf("root change reported for a subdirectory manifest: %+v", co)
	}
	write(t, manifestDir, "compose/app.yaml", "services: {x: {}}\n")
	write(t, manifestDir, "berthy.yml", "project: edited\n")
	co = inspect(t, manifestDir, "berthy.yml", "compose/app.yaml")
	if !reflect.DeepEqual(co.Changed, []string{"berthy.yml", "compose/app.yaml"}) {
		t.Fatalf("changed = %v, want the two manifest files relative to the manifest directory", co.Changed)
	}
}

func TestRepositoryWithoutCommitsReportsUntrackedFiles(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "berthy.yml", "project: demo\n")
	co := inspect(t, dir, "berthy.yml")
	if !co.InWorkTree || co.Head != "" || !reflect.DeepEqual(co.Changed, []string{"berthy.yml"}) {
		t.Fatalf("no commit yet: %+v", co)
	}
}

func TestIgnoredFilesDoNotCount(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, ".gitignore", "berthy.prod.yml\n")
	write(t, dir, "berthy.yml", "project: demo\n")
	commit(t, dir)
	write(t, dir, "berthy.prod.yml", "env: {}\n")
	co := inspect(t, dir, "berthy.yml", "berthy.prod.yml")
	if len(co.Changed) != 0 {
		t.Fatalf("ignored overlay reported: %v", co.Changed)
	}
}

func TestDeletedTrackedFileCounts(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "berthy.yml", "project: demo\n")
	write(t, dir, "berthy.prod.yml", "env: {}\n")
	commit(t, dir)
	if err := os.Remove(filepath.Join(dir, "berthy.prod.yml")); err != nil {
		t.Fatal(err)
	}
	co := inspect(t, dir, "berthy.yml", "berthy.prod.yml")
	if !reflect.DeepEqual(co.Changed, []string{"berthy.prod.yml"}) {
		t.Fatalf("changed = %v, want the deleted overlay", co.Changed)
	}
}

func TestNamesAreTakenLiterally(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "berthy.yml", "project: demo\n")
	write(t, dir, "berthy[1].yml", "a\n")
	write(t, dir, "berthy1.yml", "b\n")
	commit(t, dir)
	write(t, dir, "berthy1.yml", "edited\n")
	co := inspect(t, dir, "berthy[1].yml")
	if len(co.Changed) != 0 {
		t.Fatalf("a glob matched another file: %v", co.Changed)
	}
}

func TestCancelledCheckoutReleasesView(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "file", "content\n")
	write(t, dir, ".gitattributes", "file filter=blocked\n")
	head := commit(t, dir)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := fmt.Sprintf("\"%s\" -test.run=^TestCheckoutFilterHelper$ -- %s", filepath.ToSlash(exe), listener.Addr())
	gitIn(t, dir, "config", "filter.blocked.smudge", command)
	gitIn(t, dir, "config", "filter.blocked.required", "true")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := CompareCaptured(ctx, dir, head, []CapturedFile{{Path: "file", Present: true, Bytes: []byte("content\n")}})
		result <- err
	}()
	type accepted struct {
		conn net.Conn
		err  error
	}
	connections := make(chan accepted)
	done := make(chan struct{})
	defer close(done)
	go func() {
		conn, err := listener.Accept()
		select {
		case connections <- accepted{conn, err}:
		case <-done:
			if conn != nil {
				conn.Close()
			}
		}
	}()
	var conn net.Conn
	select {
	case incoming := <-connections:
		if incoming.err != nil {
			t.Fatal(incoming.err)
		}
		conn = incoming.conn
	case err := <-result:
		t.Fatalf("comparison ended before filter started: %v", err)
	}
	defer conn.Close()
	cancel()
	err = <-result
	if err == nil || !strings.Contains(err.Error(), "checkout input conversion unavailable") || strings.Contains(err.Error(), "cleanup failed") {
		t.Fatalf("cancellation: %v", err)
	}
	if runtime.GOOS == "windows" && !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}

func TestCheckoutFilterHelper(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	conn, err := net.Dial("tcp", os.Args[len(os.Args)-1])
	if err != nil {
		os.Exit(2)
	}
	defer conn.Close()
	_, _ = io.Copy(io.Discard, conn)
}
