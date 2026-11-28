package setup

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"berthy/internal/agent"
	"berthy/internal/scope"
)

func TestRemovalPreservesKeysAtActivationControlPaths(t *testing.T) {
	for _, control := range []string{guardProbePath, guardReason} {
		for _, owner := range []struct{ name, key string }{
			{"target", "/custom/target/key"}, {"peer", "/peer/key"},
		} {
			t.Run(owner.name+"/"+filepath.Base(control), func(t *testing.T) {
				r, opts := newRemovalRunner(t)
				r.live = bytes.Replace(r.live, []byte(`"`+owner.key+`"`), []byte(`"`+control+`"`), 1)
				r.paths = strings.ReplaceAll(r.paths, owner.key, control)
				keyPath := filepath.Join(t.TempDir(), "key")
				key := []byte("fixture key")
				if err := os.WriteFile(keyPath, key, 0600); err != nil {
					t.Fatal(err)
				}
				fixture := funcRunner{run: func(script string, stdin io.Reader) Result {
					// Model the activation write at the configured key path on a private stand-in.
					var err error
					switch {
					case control == guardProbePath && script == prepareProbeScript(Staged{Config: true}):
						err = os.WriteFile(keyPath, []byte("retained probe"), 0600)
					case control == guardReason && strings.Contains(script, "rm -f "+guardReason+"\n"):
						err = os.Remove(keyPath)
					}
					if err != nil {
						t.Fatal(err)
					}
					res, err := r.Run(context.Background(), script, stdin)
					if err != nil {
						t.Fatal(err)
					}
					return res
				}}
				s := &Session{R: fixture, Opts: opts}
				var failure string
				Run(context.Background(), s, RemovalSteps(false), func(result StepResult) {
					if result.Status == StatusFailed {
						failure = result.Detail
					}
				})
				got, err := os.ReadFile(keyPath)
				if !strings.Contains(failure, control+" overlaps agent files") || err != nil || !bytes.Equal(got, key) {
					t.Fatalf("failure = %q, fixture key intact = %t, mutations = %v", failure, err == nil && bytes.Equal(got, key), r.mutations)
				}
				r.checkMutations("")
			})
		}
	}
}

func TestRemovalRejectsUnsafePathsBeforeStaging(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses Linux stat and mount information")
	}
	for _, name := range []string{"disjoint", "archive exists", "destination overlaps peer", "peer alias overlaps", "ambiguous peer alias", "target alias", "purge crosses mount", "unreadable key stat"} {
		t.Run(name, func(t *testing.T) {
			root, tools := shellFixture(t)
			stateDir := filepath.Join(root, "custom", "state")
			keyFile := filepath.Join(root, "keys", "target")
			peerDir := filepath.Join(root, "peer")
			peerKey := filepath.Join(root, "peer-key")
			archive := stateDir + ".removed-20261128T020000Z"
			mkdir := func(p string) {
				t.Helper()
				if err := os.MkdirAll(p, 0700); err != nil {
					t.Fatal(err)
				}
			}
			write := func(p string) {
				t.Helper()
				if err := os.WriteFile(p, []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			link := func(p, target string) {
				t.Helper()
				if err := os.Symlink(target, p); err != nil {
					t.Fatal(err)
				}
			}
			mkdir(stateDir)
			mkdir(filepath.Dir(keyFile))
			mkdir(peerDir)
			write(keyFile)
			write(keyFile + ".next")
			write(peerKey)
			shellTool(t, tools, "id", "exec /usr/bin/id -u")
			mounts := ""
			switch name {
			case "archive exists":
				mkdir(archive)
			case "destination overlaps peer":
				peerDir = archive + "/child"
			case "peer alias overlaps":
				peerDir = filepath.Join(root, "peer-link")
				link(peerDir, stateDir)
			case "ambiguous peer alias":
				peerDir = filepath.Join(root, "peer-link")
				link(peerDir, filepath.Join(root, "unrelated\n", "peer"))
			case "target alias":
				alias := filepath.Join(root, "alias")
				link(alias, filepath.Dir(stateDir))
				stateDir = filepath.Join(alias, "state")
				archive = stateDir + ".removed-20261128T020000Z"
			case "purge crosses mount":
				mounts = "41 40 0:2 / " + stateDir + "/nested\\040mount rw - tmpfs tmpfs rw\n"
				archive = ""
			case "unreadable key stat":
				shellTool(t, tools, "stat", `case "$*" in *"target.next") echo "Permission denied" >&2; exit 1;; esac; exec /usr/bin/stat "$@"`)
			}
			mountFile := filepath.Join(root, "mountinfo")
			if err := os.WriteFile(mountFile, []byte("40 39 0:1 / / rw - ext4 test rw\n"+mounts), 0600); err != nil {
				t.Fatal(err)
			}
			id := scope.ID{Project: "alpha", Environment: "dev"}
			rec := removal{ID: id, StateDir: stateDir, KeyFile: keyFile, Archive: archive, CandidateSHA: strings.Repeat("a", 64)}
			config := agent.File{Environments: []agent.Hosted{
				{Project: id.Project, Environment: id.Environment, StateDir: stateDir, KeyFile: keyFile},
				{Project: "peer", Environment: "dev", StateDir: peerDir, KeyFile: peerKey},
			}}
			r := funcRunner{run: func(script string, stdin io.Reader) Result {
				if stdin != nil || !strings.HasPrefix(script, "# inspect removal paths\n") {
					t.Fatalf("unexpected remote command %q", script)
				}
				script = strings.ReplaceAll(script, "/proc/self/mountinfo", shellQuote(mountFile))
				cmd := exec.Command("sh", "-c", script)
				cmd.Env = append(os.Environ(), "PATH="+tools+":"+os.Getenv("PATH"))
				stdout, err := cmd.Output()
				if err == nil {
					return Result{Stdout: stdout}
				}
				if exit, ok := err.(*exec.ExitError); ok {
					return Result{Stdout: stdout, Stderr: exit.Stderr, Code: exit.ExitCode()}
				}
				t.Fatal(err)
				return Result{}
			}}
			observed, err := inspectRemovalPaths(context.Background(), r, rec, config)
			if name == "disjoint" {
				if err != nil || observed.StateIdentity.Inode == 0 || observed.KeyIdentity.Inode == 0 ||
					observed.NextIdentity == nil || observed.PrevIdentity != nil {
					t.Fatalf("custom layout returned %+v, %v", observed, err)
				}
			} else if err == nil {
				t.Fatalf("unsafe layout %q passed preflight", name)
			}
		})
	}
}
