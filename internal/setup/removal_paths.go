package setup

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"berthy/internal/agent"
	"berthy/internal/scope"
	"berthy/internal/state"
)

type fileIdentity struct {
	Device uint64
	Inode  uint64
}

type removal struct {
	ID                         scope.ID
	StateDir, KeyFile          string
	StateIdentity              fileIdentity
	KeyIdentity                fileIdentity
	NextIdentity, PrevIdentity *fileIdentity
	Network, NetworkID         string
	Archive                    string
	OriginalSHA, CandidateSHA  string
	Activated                  bool
}

func removalPathOK(p string) bool {
	return p != "/" && path.IsAbs(p) && path.Clean(p) == p && !strings.ContainsAny(p, "\x00\r\n")
}

func pathContains(dir, p string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

func pathsOverlap(a, b string) bool { return pathContains(a, b) || pathContains(b, a) }

func (r removal) destination() string {
	if r.Archive != "" {
		return r.Archive
	}
	return r.StateDir + ".purge-" + r.CandidateSHA
}

func hostedRemovalPaths(stateDir, key string) []string {
	return []string{stateDir, key, key + state.KeyNextSuffix, key + state.KeyPrevSuffix}
}

// GNU stat without -L describes the directory entry. With a verified parent, only ENOENT establishes absence.
const removalStat = `export LC_ALL=C
stat_entry() {
  if entry=$(stat -c '%d %i %u %a %h %F' -- "$1" 2>&1); then return 0; fi
  case "$entry" in *": No such file or directory") entry=absent;; *) return 1;; esac
}
`

func inspectRemovalPaths(ctx context.Context, runner Runner, rec removal, original agent.File) (removal, error) {
	paths := hostedRemovalPaths(rec.StateDir, rec.KeyFile)
	dest := rec.destination()
	targets := append(paths, dest)
	for _, p := range targets {
		if !removalPathOK(p) {
			return rec, errors.New("removal requires absolute clean Linux paths")
		}
		for _, control := range []string{agentConfigDir, binPath, unitPath, guardState, guardLock, guardPath} {
			if pathsOverlap(p, control) {
				return rec, fmt.Errorf("removal path %s overlaps agent files", p)
			}
		}
	}
	for _, key := range paths[1:] {
		if pathContains(key, rec.StateDir) || pathsOverlap(key, dest) {
			return rec, fmt.Errorf("removal path %s overlaps a key path", dest)
		}
	}
	var peers []string
	for _, h := range original.Environments {
		if h.Project != rec.ID.Project || h.Environment != rec.ID.Environment {
			peers = append(peers, hostedRemovalPaths(h.StateDir, h.KeyFile)...)
		}
	}
	checkOverlap := func(peer string) error {
		if !removalPathOK(peer) {
			return errors.New("cannot verify another hosted environment's paths")
		}
		for _, p := range targets {
			if pathsOverlap(p, peer) {
				return fmt.Errorf("removal path %s overlaps another hosted environment", p)
			}
		}
		return nil
	}
	for _, peer := range peers {
		if err := checkOverlap(peer); err != nil {
			return rec, err
		}
	}
	script := "# inspect removal paths\nset -eu\n" + removalStat + `
canonical() { [ "$(realpath -e -- "$1")" = "$1" ]; }
owned_entry() {
  canonical "$(dirname -- "$1")"
  stat_entry "$1"
  if [ "$entry" = absent ]; then
    [ "$4" = optional ]
    printf 'absent\n'
    return
  fi
  canonical "$1"
  case "$entry" in *" $owner $2 "*" $3") ;; *) exit 1;; esac
  set -- $entry
  [ "$4" = 700 ] || [ "$5" -eq 1 ]
  printf '%s %s\n' "$1" "$2"
}
owner=$(id -u berthy)
`
	script += "owned_entry " + shellQuote(rec.StateDir) + " 700 directory required\n"
	for i, p := range paths[1:] {
		requirement := "optional"
		if i == 0 {
			requirement = "required"
		}
		script += "owned_entry " + shellQuote(p) + " 600 'regular file' " + requirement + "\n"
	}
	script += "stat_entry " + shellQuote(dest) + "\n[ \"$entry\" = absent ]\n"
	for _, peer := range peers {
		script += "realpath -zm -- " + shellQuote(peer) + "\n"
	}
	script += "awk '{print $5}' /proc/self/mountinfo\n"

	res, err := runner.Run(ctx, script, nil)
	if err != nil || res.Code != 0 || len(res.Stderr) != 0 || len(res.Stdout) >= maxCaptured || !strings.HasSuffix(string(res.Stdout), "\n") {
		return rec, errors.New("could not verify the removal state, key paths or unused destination")
	}
	lines := strings.SplitN(string(res.Stdout), "\n", 5)
	if len(lines) != 5 {
		return rec, errors.New("incomplete removal path check")
	}
	var identities [4]*fileIdentity
	for i, line := range lines[:4] {
		if line == "absent" && i >= 2 {
			continue
		}
		device, inode, ok := strings.Cut(line, " ")
		d, devErr := strconv.ParseUint(device, 10, 64)
		n, inoErr := strconv.ParseUint(inode, 10, 64)
		if !ok || devErr != nil || inoErr != nil || n == 0 {
			return rec, errors.New("could not verify removal file identities")
		}
		identities[i] = &fileIdentity{Device: d, Inode: n}
	}
	rest := lines[4]
	for range peers {
		peer, tail, ok := strings.Cut(rest, "\x00")
		if !ok {
			return rec, errors.New("incomplete removal path check")
		}
		if err := checkOverlap(peer); err != nil {
			return rec, err
		}
		rest = tail
	}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	rootMount := false
	for _, mount := range strings.Split(strings.TrimSuffix(rest, "\n"), "\n") {
		mount = unescape.Replace(mount)
		if !path.IsAbs(mount) {
			return rec, errors.New("could not verify the server's mount table")
		}
		rootMount = rootMount || mount == "/"
		if mount == rec.StateDir || (rec.Archive == "" && pathContains(rec.StateDir, mount)) {
			return rec, fmt.Errorf("cannot remove state across the mounted path %s", mount)
		}
	}
	if !rootMount {
		return rec, errors.New("could not verify the server's mount table")
	}
	rec.StateIdentity, rec.KeyIdentity = *identities[0], *identities[1]
	rec.NextIdentity, rec.PrevIdentity = identities[2], identities[3]
	return rec, nil
}
