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

// CapturedFile is one input as read by the caller. Absent optional files have Present false.
type CapturedFile struct {
	Path    string
	Present bool
	Bytes   []byte
}

// CompareCaptured reports inputs whose captured contents do not match the given HEAD tree.
func CompareCaptured(ctx context.Context, dir, head string, files []CapturedFile) (changed []string, err error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	prefix, _, err := run(ctx, dir, "rev-parse", "--show-prefix")
	if err != nil {
		return nil, fmt.Errorf("checkout prefix unavailable: %w", err)
	}
	prefix = strings.TrimRight(prefix, "\r\n")
	root, _, err := run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil || strings.TrimRight(root, "\r\n") == "" {
		return nil, fmt.Errorf("checkout root unavailable")
	}
	root = strings.TrimRight(root, "\r\n")
	gitDir, _, err := run(ctx, dir, "rev-parse", "--absolute-git-dir")
	if err != nil || strings.TrimRight(gitDir, "\r\n") == "" {
		return nil, fmt.Errorf("checkout git directory unavailable")
	}
	gitDir = strings.TrimRight(gitDir, "\r\n")
	temp, err := os.MkdirTemp("", "berthy-git-view-")
	if err != nil {
		return nil, fmt.Errorf("checkout view unavailable: %w", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(temp); cleanupErr != nil {
			changed = nil
			err = fmt.Errorf("checkout view cleanup failed: %w", cleanupErr)
		}
	}()
	view := filepath.Join(temp, "worktree")
	if err := os.Mkdir(view, 0700); err != nil {
		return nil, fmt.Errorf("checkout view unavailable: %w", err)
	}
	hooks := filepath.Join(temp, "hooks")
	if err := os.Mkdir(hooks, 0700); err != nil {
		return nil, fmt.Errorf("checkout view unavailable: %w", err)
	}
	env := append(environment(), "GIT_INDEX_FILE="+filepath.Join(temp, "index"), "BERTHY_GIT_SOURCE_ROOT="+root)
	args := []string{"-C", view, "--git-dir=" + gitDir, "--work-tree=" + view, "-c", "core.splitIndex=false", "-c", "core.sparseCheckout=false", "-c", "core.hooksPath=" + hooks}
	conversionArgs, err := sourceConversionConfig(ctx, root)
	if err != nil {
		return nil, err
	}
	args = append(args, conversionArgs...)
	cmd := exec.CommandContext(ctx, "git", append(append([]string{}, args...), "read-tree", head)...)
	cmd.Dir, cmd.Env, cmd.WaitDelay = view, env, time.Second
	diagnostics := &checkoutComparison{}
	cmd.Stdout, cmd.Stderr = diagnostics, diagnostics
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("checkout view unavailable: %w", err)
	}
	if diagnostics.different {
		return nil, fmt.Errorf("checkout view reported diagnostics")
	}
	attributes := make(map[string]bool)
	for _, f := range files {
		name := f.Path
		if name == "" || name != path.Clean(name) || name == "." || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00") || path.IsAbs(name) || strings.Contains(name, ":") {
			return nil, fmt.Errorf("invalid checkout input path")
		}
		rootName := prefix + name
		entry, _, err := run(ctx, root, "--literal-pathspecs", "ls-tree", "-z", head, "--", rootName)
		if err != nil {
			return nil, fmt.Errorf("checkout tree unavailable: %w", err)
		}
		if entry == "" {
			if f.Present {
				changed = append(changed, name)
			}
			continue
		}
		parts := strings.Split(entry, "\x00")
		if len(parts) != 2 || parts[1] != "" {
			return nil, fmt.Errorf("invalid checkout tree entry")
		}
		fields := strings.SplitN(parts[0], "\t", 2)
		if len(fields) != 2 || fields[1] != rootName {
			return nil, fmt.Errorf("invalid checkout tree path")
		}
		meta := strings.Fields(fields[0])
		if len(meta) != 3 || meta[1] != "blob" || len(meta[2]) < 40 {
			return nil, fmt.Errorf("invalid checkout tree object")
		}
		if !f.Present || (meta[0] != "100644" && meta[0] != "100755") {
			changed = append(changed, name)
			continue
		}
		if err := checkoutAttributes(ctx, root, view, head, rootName, attributes); err != nil {
			return nil, err
		}
		match, err := matchesCheckout(ctx, view, args, env, rootName, meta[2], f.Bytes)
		if err != nil {
			return nil, fmt.Errorf("checkout input conversion unavailable: %w", err)
		}
		if !match {
			changed = append(changed, name)
		}
	}
	return changed, nil
}

// sourceConversionConfig keeps relative Git conversion settings rooted at the source checkout.
func sourceConversionConfig(ctx context.Context, root string) ([]string, error) {
	config := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root, "config", "--null"}, args...)...)
		cmd.Env, cmd.WaitDelay = environment(), time.Second
		output := &attributeBuffer{}
		diagnostics := &checkoutComparison{}
		cmd.Stdout, cmd.Stderr = output, diagnostics
		err := cmd.Run()
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 && output.Len() == 0 && !diagnostics.different {
			return nil, nil
		}
		if err != nil || output.tooLarge || diagnostics.different {
			return nil, fmt.Errorf("checkout conversion configuration unavailable")
		}
		if output.Len() == 0 || output.Bytes()[output.Len()-1] != 0 {
			return nil, fmt.Errorf("invalid checkout conversion configuration")
		}
		return output.Bytes(), nil
	}
	var args []string
	attr, err := config("--path", "--get", "core.attributesFile")
	if err != nil {
		return nil, err
	}
	if len(attr) > 0 {
		value := string(attr[:len(attr)-1])
		if strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("invalid checkout conversion configuration")
		}
		if value != "" && !filepath.IsAbs(value) {
			value = filepath.Join(root, value)
		}
		args = append(args, "-c", "core.attributesFile="+value)
	}
	filters, err := config("--get-regexp", `^filter\..*\.(smudge|process)$`)
	if err != nil {
		return nil, err
	}
	commands := make(map[string]string)
	for _, record := range bytes.Split(filters, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		key, value, ok := bytes.Cut(record, []byte{'\n'})
		if !ok || len(key) == 0 {
			return nil, fmt.Errorf("invalid checkout conversion configuration")
		}
		commands[string(key)] = string(value)
	}
	keys := make([]string, 0, len(commands))
	for key := range commands {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if commands[key] == "" {
			continue
		}
		// Git runs filter commands through its shell; preserve the configured command verbatim.
		args = append(args, "-c", key+`=cd -- "$BERTHY_GIT_SOURCE_ROOT" && `+commands[key])
	}
	return args, nil
}

// checkoutAttributes supplies the HEAD attribute files because older Git does not
// consult the index for absent worktree attributes when running cat-file.
func checkoutAttributes(ctx context.Context, root, view, head, name string, done map[string]bool) error {
	for dir := path.Dir(name); ; dir = path.Dir(dir) {
		attr := path.Join(dir, ".gitattributes")
		if !done[attr] {
			entry, _, err := run(ctx, root, "--literal-pathspecs", "ls-tree", "-z", head, "--", attr)
			if err != nil {
				return fmt.Errorf("checkout attributes unavailable: %w", err)
			}
			if entry != "" {
				parts := strings.Split(entry, "\x00")
				if len(parts) != 2 || parts[1] != "" {
					return fmt.Errorf("invalid checkout attributes entry")
				}
				fields := strings.SplitN(parts[0], "\t", 2)
				if len(fields) != 2 || fields[1] != attr {
					return fmt.Errorf("invalid checkout attributes path")
				}
				meta := strings.Fields(fields[0])
				if len(meta) != 3 || (meta[0] != "100644" && meta[0] != "100755") || meta[1] != "blob" {
					return fmt.Errorf("unsupported checkout attributes")
				}
				cmd := exec.CommandContext(ctx, "git", "-C", root, "cat-file", "blob", meta[2])
				cmd.Env, cmd.WaitDelay = environment(), time.Second
				output := &attributeBuffer{}
				diagnostics := &checkoutComparison{}
				cmd.Stdout, cmd.Stderr = output, diagnostics
				if err := cmd.Run(); err != nil {
					return fmt.Errorf("checkout attributes unavailable: %w", err)
				}
				if output.tooLarge || diagnostics.different {
					return fmt.Errorf("checkout attributes unavailable")
				}
				target := filepath.Join(view, filepath.FromSlash(attr))
				if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					return fmt.Errorf("checkout attributes unavailable: %w", err)
				}
				if err := os.WriteFile(target, output.Bytes(), 0600); err != nil {
					return fmt.Errorf("checkout attributes unavailable: %w", err)
				}
			}
			done[attr] = true
		}
		if dir == "." {
			break
		}
	}
	return nil
}

// attributeBuffer caps files copied into the temporary Git view.
type attributeBuffer struct {
	bytes.Buffer
	tooLarge bool
}

func (b *attributeBuffer) Write(p []byte) (int, error) {
	const maxAttributes = 1 << 20
	n := len(p)
	if n > maxAttributes-b.Len() {
		b.tooLarge = true
		p = p[:maxAttributes-b.Len()]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

// matchesCheckout compares streamed checkout output without retaining filter output.
func matchesCheckout(ctx context.Context, view string, args, env []string, name, object string, captured []byte) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", append(append([]string{}, args...), "cat-file", "--filters", "--path="+name, object)...)
	cmd.Dir, cmd.Env = view, env
	cmd.WaitDelay = time.Second
	output := &checkoutComparison{expected: captured}
	diagnostics := &checkoutComparison{}
	cmd.Stdout, cmd.Stderr = output, diagnostics
	if err := cmd.Run(); err != nil {
		return false, err
	}
	if diagnostics.different {
		return false, fmt.Errorf("checkout conversion reported diagnostics")
	}
	return !output.different && output.offset == len(captured), nil
}

type checkoutComparison struct {
	expected  []byte
	offset    int
	different bool
}

func (c *checkoutComparison) Write(p []byte) (int, error) {
	n := len(p)
	remaining := len(c.expected) - c.offset
	if n > remaining {
		c.different = true
		p = p[:remaining]
	}
	if !bytes.Equal(p, c.expected[c.offset:c.offset+len(p)]) {
		c.different = true
	}
	c.offset += len(p)
	return n, nil
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
