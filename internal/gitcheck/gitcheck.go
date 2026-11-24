// Package gitcheck asks git whether the files a deploy reads have local changes, so a release can record the commit it was
// really deployed from.
package gitcheck

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Checkout is what git says about the files a deploy reads.
type Checkout struct {
	InWorkTree bool     // false outside a work tree or when git is not installed
	Head       string   // the full commit HEAD names; "" when the repository has no commit yet
	Changed    []string // the given files that are modified, staged, deleted or untracked, relative to dir, sorted
}

// Identity identifies the checkout around a read of deploy inputs. GitDir distinguishes linked worktrees.
type Identity struct {
	GitAvailable bool
	InWorkTree   bool
	Head         string
	GitDir       string
	gitDirInfo   os.FileInfo
	workDirInfo  os.FileInfo
}

// Same reports whether two captures refer to the same Git availability, worktree and revision.
func (id Identity) Same(other Identity) bool {
	if id.GitAvailable != other.GitAvailable || id.InWorkTree != other.InWorkTree || id.Head != other.Head || id.GitDir != other.GitDir {
		return false
	}
	return !id.InWorkTree || os.SameFile(id.gitDirInfo, other.gitDirInfo) && os.SameFile(id.workDirInfo, other.workDirInfo)
}

// Capture reads checkout identity without inspecting file status. Call it before reading deploy inputs.
func Capture(ctx context.Context, dir string) (Identity, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return Identity{}, nil
	}
	id := Identity{GitAvailable: true}
	co, err := Inspect(ctx, dir, nil)
	if err != nil {
		return Identity{}, err
	}
	id.InWorkTree, id.Head = co.InWorkTree, co.Head
	if !id.InWorkTree {
		return id, nil
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	gitDir, _, err := run(ctx, dir, "rev-parse", "--absolute-git-dir")
	if err != nil || strings.TrimSpace(gitDir) == "" {
		return Identity{}, fmt.Errorf("checkout identity unavailable")
	}
	id.GitDir = filepath.Clean(strings.TrimSpace(gitDir))
	id.gitDirInfo, err = capturedFileInfo(id.GitDir)
	if err != nil {
		return Identity{}, fmt.Errorf("checkout identity unavailable")
	}
	id.workDirInfo, err = capturedFileInfo(dir)
	if err != nil {
		return Identity{}, fmt.Errorf("checkout identity unavailable")
	}
	return id, nil
}

// capturedFileInfo takes the identity from an open handle; os.Stat defers it on Windows.
func capturedFileInfo(path string) (os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}

// gitTimeout bounds one inspection, which runs a few short git commands.
const gitTimeout = 20 * time.Second

// notRepository is git's message for a directory outside any repository. Any other failure is an error.
const notRepository = "not a git repository"

// Inspect asks git about the files a deploy reads, in dir. Files that do not exist are ignored. It never fails because git
// is missing or dir is not in a work tree; it reports InWorkTree false instead.
func Inspect(ctx context.Context, dir string, files []string) (Checkout, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return Checkout{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	inside, stderr, err := run(ctx, dir, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		if strings.Contains(stderr, notRepository) {
			return Checkout{}, nil
		}
		return Checkout{}, err
	}
	if strings.TrimSpace(inside) != "true" {
		return Checkout{}, nil
	}
	co := Checkout{InWorkTree: true}
	// A repository without a commit yet has no HEAD: git says so with no output and a failure, which is not an error here.
	if head, _, err := run(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD"); err == nil {
		co.Head = strings.TrimSpace(head)
	}
	if len(files) == 0 {
		return co, nil
	}
	// Porcelain paths are relative to the repository root, so the directory's place in it is taken off each one.
	prefix, _, err := run(ctx, dir, "rev-parse", "--show-prefix")
	if err != nil {
		return Checkout{}, err
	}
	pathspecs := make([]string, 0, len(files))
	for _, f := range files {
		pathspecs = append(pathspecs, path.Clean(filepath.ToSlash(f)))
	}
	status, stderr, err := run(ctx, dir, append([]string{"--literal-pathspecs", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--"}, pathspecs...)...)
	if err != nil {
		return Checkout{}, fmt.Errorf("git status: %s", strings.TrimSpace(stderr))
	}
	co.Changed = changedPaths(status, strings.TrimRight(prefix, "\r\n"))
	return co, nil
}

// changedPaths reads the NUL-separated entries of git status --porcelain=v1 -z. Each is "XY path", and a rename or a copy
// is followed by the path it came from, which is not a change of its own. The paths are made relative to prefix, the
// directory inside the repository, and a path outside it is left out.
func changedPaths(status, prefix string) []string {
	var changed []string
	entries := strings.Split(status, "\x00")
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if len(entry) < 4 {
			continue
		}
		code, name := entry[:2], entry[3:]
		if strings.ContainsAny(code, "RC") {
			i++
		}
		if strings.HasPrefix(name, prefix) {
			changed = append(changed, strings.TrimPrefix(name, prefix))
		}
	}
	sort.Strings(changed)
	return changed
}

// run runs git in dir and returns its standard output and error output. It runs with a minimal environment: the search path
// and the home directory, and the variables that keep its output and its locking predictable. GIT_DIR and the like are left
// out, so git finds the repository of dir and no other.
func run(ctx context.Context, dir string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = environment()
	cmd.WaitDelay = time.Second
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

// environment is what git runs with. GIT_OPTIONAL_LOCKS=0 keeps status from rewriting the index.
func environment() []string {
	env := []string{"LC_ALL=C", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0"}
	for _, name := range []string{"PATH", "HOME", "USERPROFILE", "SYSTEMROOT", "XDG_CONFIG_HOME"} {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}
