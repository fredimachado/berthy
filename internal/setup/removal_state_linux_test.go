package setup

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"berthy/internal/agent"
	"berthy/internal/scope"
	"berthy/internal/state"
)

func removalIdentity(t *testing.T, path string) fileIdentity {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	return fileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}
}

func removalFixtureDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
}

func removalFixtureWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func removalFixtureMove(t *testing.T, from, to string) {
	t.Helper()
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
}

func removalFixtureHas(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("%s = %q, %v, want %q", path, got, err, want)
	}
}

func removalFixtureAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s should be absent, stat error: %v", path, err)
	}
}

func newRemovalStateFixture(t *testing.T, purge, nestedKey bool) (*Session, string) {
	t.Helper()
	dir, tools := shellFixture(t)
	shellTool(t, tools, "id", "exec /usr/bin/id -u")
	stateDir := filepath.Join(dir, "custom state")
	keyFile := filepath.Join(dir, "custom key")
	if nestedKey {
		keyFile = filepath.Join(stateDir, "nested", "custom key")
	}
	removalFixtureDir(t, filepath.Join(stateDir, "nested"))
	removalFixtureWrite(t, filepath.Join(stateDir, "nested", "compose.yml"), "compose data")
	removalFixtureWrite(t, filepath.Join(stateDir, "state.db-wal"), "sqlite data")
	removalFixtureWrite(t, keyFile, "target key")
	next := keyFile + state.KeyNextSuffix
	removalFixtureWrite(t, next, "next key")
	nextIdentity := removalIdentity(t, next)
	rec := &removal{
		ID:       scope.ID{Project: "alpha", Environment: "dev"},
		StateDir: stateDir, KeyFile: keyFile, StateIdentity: removalIdentity(t, stateDir),
		KeyIdentity: removalIdentity(t, keyFile), NextIdentity: &nextIdentity,
		CandidateSHA: strings.Repeat("a", 64), Activated: true,
	}
	if !purge {
		rec.Archive = stateDir + ".removed-20261128T020000Z"
	}
	r := funcRunner{run: func(script string, input io.Reader) Result {
		res, err := runProcess(context.Background(), "env",
			[]string{"PATH=" + tools + ":" + os.Getenv("PATH"), "sh", "-c", script}, input)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}}
	return &Session{R: r, removal: rec}, tools
}

func TestRemovalArchivesStateAndUnlinksOnlyTargetKeys(t *testing.T) {
	s, _ := newRemovalStateFixture(t, false, false)
	r := s.removal
	old := r.StateDir + ".removed-20261127T020000Z"
	peer := filepath.Join(filepath.Dir(r.StateDir), "peer", "key")
	removalFixtureDir(t, old)
	removalFixtureDir(t, filepath.Dir(peer))
	removalFixtureWrite(t, filepath.Join(old, "sentinel"), "older archive")
	removalFixtureWrite(t, peer, "peer key")
	prev := r.KeyFile + state.KeyPrevSuffix
	removalFixtureWrite(t, prev, "previous key")
	prevIdentity := removalIdentity(t, prev)
	r.PrevIdentity = &prevIdentity
	status, detail, err := removeStateStep(context.Background(), s)
	if err != nil || status != StatusChanged || !strings.Contains(detail, r.Archive) {
		t.Fatalf("archive reported %q %q %v", status, detail, err)
	}
	info, err := os.Stat(r.Archive)
	if err != nil || info.Mode().Perm() != 0700 || removalIdentity(t, r.Archive) != r.StateIdentity {
		t.Fatalf("archive did not retain mode and identity: %v, %v", info, err)
	}
	removalFixtureHas(t, filepath.Join(r.Archive, "nested", "compose.yml"), "compose data")
	removalFixtureHas(t, filepath.Join(r.Archive, "state.db-wal"), "sqlite data")
	removalFixtureHas(t, filepath.Join(old, "sentinel"), "older archive")
	removalFixtureHas(t, peer, "peer key")
	for _, path := range hostedRemovalPaths(r.StateDir, r.KeyFile) {
		removalFixtureAbsent(t, path)
	}
}

func TestRemovalRetriesArchiveAndPurgeWithoutTakingAnotherTree(t *testing.T) {
	for _, name := range []string{"archive resumes", "archive replacement", "purge resumes", "purge replacement", "purge mount on retry"} {
		t.Run(name, func(t *testing.T) {
			purge := strings.HasPrefix(name, "purge")
			s, tools := newRemovalStateFixture(t, purge, false)
			r := s.removal
			dest := r.destination()
			if purge {
				shellTool(t, tools, "rm", "if [ \"$1\" = --one-file-system ]; then /bin/rm -- "+shellQuote(filepath.Join(dest, "state.db-wal"))+"; exit 1; fi; exec /bin/rm \"$@\"")
			} else {
				shellTool(t, tools, "mv", "/bin/mv \"$@\" && exit 1")
			}
			if _, _, err := removeStateStep(context.Background(), s); err == nil {
				t.Fatal("interruption reported success")
			}
			removalFixtureAbsent(t, r.StateDir)
			if removalIdentity(t, dest) != r.StateIdentity {
				t.Fatal("interruption did not leave the recorded tree at the destination")
			}
			if purge {
				removalFixtureAbsent(t, filepath.Join(dest, "state.db-wal"))
			}
			if err := os.Remove(filepath.Join(tools, map[bool]string{true: "rm", false: "mv"}[purge])); err != nil {
				t.Fatal(err)
			}
			if strings.HasSuffix(name, "replacement") {
				removalFixtureMove(t, dest, dest+".saved")
				removalFixtureDir(t, dest)
				removalFixtureWrite(t, filepath.Join(dest, "sentinel"), "replacement")
			}
			if name == "purge mount on retry" {
				escaped := strings.ReplaceAll(dest, " ", `\040`)
				shellTool(t, tools, "awk", "printf '%s\\n' '40 39 0:1 / / rw - ext4 test rw' "+
					shellQuote("41 40 0:2 / "+escaped+"/nested rw - tmpfs tmpfs rw")+" | /usr/bin/awk \"$1\"")
			}
			fresh := &Session{R: s.R, removal: r}
			_, _, err := removeStateStep(context.Background(), fresh)
			switch {
			case strings.HasSuffix(name, "replacement"):
				if err == nil || !strings.Contains(err.Error(), dest) {
					t.Fatalf("replacement refusal: %v", err)
				}
				removalFixtureHas(t, filepath.Join(dest, "sentinel"), "replacement")
				removalFixtureHas(t, filepath.Join(dest+".saved", "nested", "compose.yml"), "compose data")
			case name == "purge mount on retry":
				if err == nil {
					t.Fatal("retry traversed a mount in the staged directory")
				}
				removalFixtureHas(t, filepath.Join(dest, "nested", "compose.yml"), "compose data")
			default:
				if err != nil {
					t.Fatal(err)
				}
				if purge {
					removalFixtureAbsent(t, dest)
				} else {
					removalFixtureHas(t, filepath.Join(dest, "nested", "compose.yml"), "compose data")
				}
				removalFixtureAbsent(t, r.KeyFile)
				if status, _, err := removeStateStep(context.Background(), fresh); status != StatusOK || err != nil {
					t.Fatalf("completed retry = %q, %v", status, err)
				}
			}
		})
	}
}

func TestRemovalRetryPreservesReplacementKeys(t *testing.T) {
	for _, name := range []string{"external key", "nested key", "unexpected sidecar"} {
		t.Run(name, func(t *testing.T) {
			nested := name != "external key"
			s, _ := newRemovalStateFixture(t, true, nested)
			r := s.removal
			dest := r.destination()
			removalFixtureMove(t, r.StateDir, dest)
			key := r.KeyFile
			if nested {
				key = dest + strings.TrimPrefix(r.KeyFile, r.StateDir)
			}
			replacement := key
			if name == "unexpected sidecar" {
				replacement += state.KeyPrevSuffix
			} else {
				removalFixtureMove(t, key, key+".original")
			}
			removalFixtureWrite(t, replacement, "replacement key")
			_, _, err := removeStateStep(context.Background(), s)
			if err == nil || !strings.Contains(err.Error(), replacement) {
				t.Fatalf("key conflict = %v", err)
			}
			removalFixtureHas(t, replacement, "replacement key")
			removalFixtureHas(t, key+state.KeyNextSuffix, "next key")
			removalFixtureHas(t, filepath.Join(dest, "nested", "compose.yml"), "compose data")
		})
	}
}

func TestRemovalRefusesBindMountedPeerPaths(t *testing.T) {
	if err := exec.Command("unshare", "-Ur", "-m", "true").Run(); err != nil {
		t.Skipf("unprivileged mount namespaces unavailable: %v", err)
	}
	for _, name := range []string{"key parent", "key inside peer tree", "key alias before cleanup", "state tree alias before cleanup"} {
		t.Run(name, func(t *testing.T) {
			root, tools := shellFixture(t)
			shellTool(t, tools, "id", "exec /usr/bin/id -u")
			stateDir := filepath.Join(root, "target", "state")
			keyFile := filepath.Join(root, "target", "keys", "key")
			peerDir := filepath.Join(root, "peer", "state")
			peerKey := filepath.Join(root, "peer", "keys", "key")
			for _, dir := range []string{stateDir, filepath.Dir(keyFile), peerDir, filepath.Dir(peerKey)} {
				removalFixtureDir(t, dir)
			}
			removalFixtureWrite(t, keyFile, "target key")
			removalFixtureWrite(t, peerKey, "peer key")
			removalFixtureWrite(t, filepath.Join(stateDir, "sentinel"), "target state")
			from, to := filepath.Dir(peerKey), filepath.Dir(keyFile)
			switch name {
			case "key inside peer tree":
				from = filepath.Join(peerDir, "nested")
				removalFixtureDir(t, from)
				removalFixtureWrite(t, filepath.Join(from, "key"), "peer state")
			case "key alias before cleanup":
				from, to = to, from
			case "state tree alias before cleanup":
				from, to = stateDir, peerDir
			}
			rec := removal{ID: scope.ID{Project: "alpha", Environment: "dev"}, StateDir: stateDir, KeyFile: keyFile,
				StateIdentity: removalIdentity(t, stateDir), KeyIdentity: removalIdentity(t, keyFile),
				Archive: stateDir + ".removed-20261128T020000Z", Activated: true}
			cfg := agent.File{Environments: []agent.Hosted{
				{Project: "peer", Environment: "dev", StateDir: peerDir, KeyFile: peerKey},
			}}
			r := funcRunner{run: func(script string, stdin io.Reader) Result {
				cmd := exec.Command("unshare", "-Ur", "-m", "--", "sh", "-c", `mount --bind "$1" "$2" || exit 99; exec sh -c "$3"`, "bind-fixture", from, to, script)
				cmd.Env = append(os.Environ(), "PATH="+tools+":"+os.Getenv("PATH"))
				out, err := cmd.Output()
				if err == nil {
					return Result{Stdout: out}
				}
				if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() != 99 {
					return Result{Code: exit.ExitCode(), Stdout: out, Stderr: exit.Stderr}
				}
				t.Fatalf("bind fixture: %v", err)
				return Result{}
			}}
			var err error
			if strings.HasSuffix(name, "cleanup") {
				s := &Session{R: r, removal: &rec, existing: &cfg}
				_, _, err = removeStateStep(context.Background(), s)
			} else {
				_, err = inspectRemovalPaths(context.Background(), r, rec, cfg)
			}
			if err == nil {
				t.Errorf("%s accepted the bind-mounted peer alias", name)
			}
			removalFixtureHas(t, keyFile, "target key")
			removalFixtureHas(t, peerKey, "peer key")
			removalFixtureHas(t, filepath.Join(stateDir, "sentinel"), "target state")
			removalFixtureAbsent(t, rec.Archive)
		})
	}
}

func TestActivatedRemovalPreviewChecksCleanupProgress(t *testing.T) {
	for _, name := range []string{"pending archive", "archived with keys pending", "already archived", "already purged", "replacement destination"} {
		t.Run(name, func(t *testing.T) {
			s, _ := newRemovalStateFixture(t, name == "already purged", false)
			s.DryRun = true
			r := s.removal
			dest := r.destination()
			if name != "pending archive" {
				removalFixtureMove(t, r.StateDir, dest)
			}
			if name == "already archived" || name == "already purged" {
				for _, p := range []string{r.KeyFile, r.KeyFile + state.KeyNextSuffix} {
					if err := os.Remove(p); err != nil {
						t.Fatal(err)
					}
				}
			}
			switch name {
			case "replacement destination":
				removalFixtureMove(t, dest, dest+".saved")
				removalFixtureDir(t, dest)
				removalFixtureWrite(t, filepath.Join(dest, "sentinel"), "replacement")
			case "already purged":
				if err := os.RemoveAll(dest); err != nil {
					t.Fatal(err)
				}
			}
			status, detail, err := removeStateStep(context.Background(), s)
			switch name {
			case "replacement destination":
				if err == nil || !strings.Contains(err.Error(), dest) {
					t.Fatalf("conflicting preview = %q %q %v", status, detail, err)
				}
				removalFixtureHas(t, filepath.Join(dest, "sentinel"), "replacement")
			case "already archived", "already purged":
				if err != nil || status != StatusOK || !strings.Contains(detail, "already") {
					t.Fatalf("completed preview = %q %q %v", status, detail, err)
				}
				removalFixtureAbsent(t, r.KeyFile)
			case "archived with keys pending":
				if err != nil || status != StatusWouldChange || !strings.Contains(detail, "already archived") {
					t.Fatalf("key cleanup preview = %q %q %v", status, detail, err)
				}
				removalFixtureHas(t, r.KeyFile, "target key")
			default:
				if err != nil || status != StatusWouldChange || !strings.Contains(detail, "would archive") {
					t.Fatalf("pending preview = %q %q %v", status, detail, err)
				}
				removalFixtureHas(t, r.KeyFile, "target key")
				removalFixtureAbsent(t, dest)
			}
		})
	}
}
