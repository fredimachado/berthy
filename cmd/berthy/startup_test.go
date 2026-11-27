//go:build unix

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// buildBerthy builds the command into a temporary directory and returns the binary's path.
func buildBerthy(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		goBin = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	bin := filepath.Join(t.TempDir(), "berthy")
	cmd := exec.Command(goBin, "build", "-o", bin, "berthy/cmd/berthy")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// initLine matches one line of GODEBUG=inittrace=1.
var initLine = regexp.MustCompile(`^init (\S+) @[0-9.]+ ms, [0-9.]+ ms clock, [0-9]+ bytes, ([0-9]+) allocs$`)

// initAllocs runs `berthy version` with the given PATH and returns how many allocations each package's init made.
// The child's environment is exactly PATH, GODEBUG=inittrace=1 and HOME, so nothing inherited can change the result.
func initAllocs(t *testing.T, bin, path string) map[string]int {
	t.Helper()
	cmd := exec.Command(bin, "version")
	cmd.Env = []string{"PATH=" + path, "GODEBUG=inittrace=1", "HOME=" + t.TempDir()}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("berthy version: %v\n%s", err, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "berthy ") {
		t.Fatalf("berthy version printed %q", stdout.String())
	}
	allocs := make(map[string]int)
	for _, line := range strings.Split(stderr.String(), "\n") {
		m := initLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatal(err)
		}
		allocs[m[1]] = n
	}
	return allocs
}

// TestStartupDoesNotSearchPATH: a command that needs no external tool does the same work at start whatever the length of PATH.
func TestStartupDoesNotSearchPATH(t *testing.T) {
	bin := buildBerthy(t)
	dir := t.TempDir()
	// The directories need not exist: a search looks in each entry either way.
	entries := make([]string, 300)
	for i := range entries {
		entries[i] = filepath.Join(dir, fmt.Sprintf("p%03d", i))
	}
	short := initAllocs(t, bin, entries[0])
	long := initAllocs(t, bin, strings.Join(entries, string(os.PathListSeparator)))
	// Guard against a vacuous pass if the runtime's trace format changes.
	if len(short) < 20 || len(long) < 20 {
		t.Fatalf("inittrace listed %d and %d packages: the trace format may have changed", len(short), len(long))
	}
	const slack = 100 // a few allocations differ between runs; a search of 300 entries costs thousands
	for pkg, n := range long {
		if extra := n - short[pkg]; extra > slack {
			t.Errorf("%s allocates %d more at start with a 300-entry PATH than with one entry: it searches PATH before main runs, which costs seconds where PATH is long (WSL)", pkg, extra)
		}
	}
}

func TestMountedProbeAndOrdinaryProbeAlias(t *testing.T) {
	bin := buildBerthy(t)
	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	manifest := `project: demo
network: demo
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    roles:
      worker: {}
aliases:
  probe: config show --json
  validate: config validate
`
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, store, want string
		args              []string
		code              int
	}{
		{"mounted usage", "invalid", "usage: berthy probe uptime DURATION", []string{"probe"}, 2},
		{"mounted bad duration", "invalid", "usage: berthy probe uptime DURATION", []string{"probe", "uptime", "-1s"}, 2},
		{"validate manifest", "none", "", []string{"config", "validate"}, 0},
		{"probe alias", "none", `"project": "demo"`, []string{"probe"}, 0},
		{"other alias", "none", "", []string{"validate"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(bin, tc.args...)
			cmd.Dir = dir
			cmd.Env = []string{"HOME=" + dir, "BERTHY_CONFIG=" + config, "BERTHY_TOKEN_STORE=" + tc.store, "BERTHY_ENV=dev"}
			if tc.store == "invalid" {
				cmd.Args[0] = "/.berthy/berthy"
			}
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				exit, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatal(err)
				}
				code = exit.ExitCode()
			}
			if code != tc.code || !strings.Contains(string(out), tc.want) {
				t.Fatalf("exit %d, output %q, want exit %d and %q", code, out, tc.code, tc.want)
			}
		})
	}
}
