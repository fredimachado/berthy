package setup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"berthy/internal/agent"
)

func cleanupReady(s *Session) error {
	if s.removal == nil || (!s.DryRun && !s.removal.Activated) {
		return errors.New("the agent has not confirmed this removal")
	}
	return nil
}

func removeStateStep(ctx context.Context, s *Session) (string, string, error) {
	if err := cleanupReady(s); err != nil {
		return "", "", err
	}
	rec := *s.removal
	dest := rec.destination()
	preview := func() (string, string, error) {
		if rec.Archive == "" {
			return StatusWouldChange, "would purge " + rec.StateDir + " and remove its server key and rotation files", nil
		}
		return StatusWouldChange, "would archive state at " + dest + " and remove its server key and rotation files", nil
	}
	if s.DryRun && !rec.Activated {
		return preview()
	}
	script, paths := removeStateScript(rec, removalPeers(s.existing, rec.ID), s.DryRun)
	res, err := s.R.Run(ctx, script, nil)
	var transport *SSHTransportError
	if errors.As(err, &transport) {
		return "", "", err
	}
	if err == nil && len(res.Stderr) == 0 {
		switch {
		case s.DryRun && res.Code == 0 && string(res.Stdout) == "keys\n":
			if rec.Archive == "" {
				return StatusWouldChange, "state already purged. Would remove the remaining server keys", nil
			}
			return StatusWouldChange, "state already archived at " + dest + ". Would remove the remaining server keys", nil
		case res.Code == 0 && string(res.Stdout) == "changed\n":
			if s.DryRun {
				return preview()
			}
			if rec.Archive == "" {
				return StatusChanged, "purged " + rec.StateDir + " and removed its server key and rotation files", nil
			}
			return StatusChanged, "archived state at " + dest + " and removed its server key and rotation files", nil
		case res.Code == 0 && string(res.Stdout) == "clean\n":
			if rec.Archive == "" {
				return StatusOK, "state and server keys already removed", nil
			}
			return StatusOK, "state already archived at " + dest + " and server keys removed", nil
		case res.Code == 4:
			for _, path := range paths {
				if string(res.Stdout) == path+"\n" {
					return "", "", fmt.Errorf("could not safely clean up %s. Check its identity or mount status before retrying", path)
				}
			}
		}
	}
	return "", "", fmt.Errorf("could not finish state and key cleanup for %s", rec.ID)
}

// Inspect the entire set of keys before touching any. Recheck each one just before unlinking it.
func removeStateScript(rec removal, peers []agent.Hosted, preview bool) (string, []string) {
	dest := rec.destination()
	identity := func(id fileIdentity) string { return fmt.Sprintf("%d %d", id.Device, id.Inode) }
	mountEscape := strings.NewReplacer(`\`, `\134`, " ", `\040`, "\t", `\011`, "\n", `\012`)
	purge := rec.Archive == ""
	script := "# remove state and keys\nset -eu\n" + removalStat + removalPeerCheckScript(peers) +
		"source=" + shellQuote(rec.StateDir) + "\ndest=" + shellQuote(dest) + "\n" +
		"source_mount=" + shellQuote(mountEscape.Replace(rec.StateDir)) + "\ndest_mount=" + shellQuote(mountEscape.Replace(dest)) + "\n" +
		"expected_state=" + shellQuote(identity(rec.StateIdentity)) + "\n" +
		fmt.Sprintf("purge=%t\npreview=%t\n", purge, preview) + `
fail() { printf '%s\n' "$1"; exit 4; }
canonical() {
  resolved=$(realpath -m -- "$1" && printf x)
  [ "$resolved" = "$(printf '%s\nx' "$1")" ]
}
directory() {
  canonical "$1" && stat_entry "$1" || fail "$1"
  case "$entry" in
    absent|"$expected_state $owner 700 "*" directory") ;;
    *) fail "$1";;
  esac
}
unmounted() {
  removal_mount="$2" removal_purge="$purge" awk '
    $5 == "/" { root = 1 }
    $5 == ENVIRON["removal_mount"] ||
      (ENVIRON["removal_purge"] == "true" && index($5, ENVIRON["removal_mount"] "/") == 1) { mounted = 1 }
    END { exit (!root || mounted) }
  ' /proc/self/mountinfo || fail "$1"
}
key_location() {
  case "$1" in "$source"/*) printf '%s\n' "$base${1#"$source"}";; *) printf '%s\n' "$1";; esac
}
key() {
  key_path=$(key_location "$1")
  canonical "$key_path" && stat_entry "$key_path" || fail "$key_path"
  [ "$entry" != absent ] || return 0
  case "$entry" in
    "$2 $owner 600 1 regular file") ;;
    *) fail "$key_path";;
  esac
  if [ "$action" = unlink ]; then
    rm -- "$key_path" || fail "$key_path"
    changed=yes
  else
    keys_pending=yes
  fi
}
keys() {
`
	paths := []string{rec.StateDir, dest}
	checkPeers := "  peer_checks \"$base\""
	for i, key := range hostedRemovalPaths(rec.StateDir, rec.KeyFile)[1:] {
		id := []*fileIdentity{&rec.KeyIdentity, rec.NextIdentity, rec.PrevIdentity}[i]
		want := "absent"
		if id != nil {
			want = identity(*id)
		}
		script += "  key " + shellQuote(key) + " " + shellQuote(want) + "\n"
		checkPeers += " \"$(key_location " + shellQuote(key) + ")\""
		paths = append(paths, key)
		if pathContains(rec.StateDir, key) {
			paths = append(paths, dest+strings.TrimPrefix(key, rec.StateDir))
		}
	}
	script += `
}
check_peers() {
` + checkPeers + ` || fail "$base"
}
owner=$(id -u berthy)
changed=no
keys_pending=no
state_pending=no
directory "$source"
base=$dest
[ "$entry" = absent ] || base=$source
directory "$dest"
if [ "$base" = "$source" ]; then
  [ "$entry" = absent ] || fail "$dest"
  unmounted "$source" "$source_mount"
  state_pending=yes
elif [ "$entry" = absent ]; then
  [ "$purge" = true ] || fail "$dest"
else
  unmounted "$dest" "$dest_mount"
  [ "$purge" != true ] || state_pending=yes
fi

action=inspect
keys
check_peers
if [ "$preview" = true ]; then
  if [ "$state_pending" = yes ]; then printf 'changed\n';
  elif [ "$keys_pending" = yes ]; then printf 'keys\n';
  else printf 'clean\n'; fi
  exit 0
fi
if [ "$base" = "$source" ]; then
  directory "$source"
  [ "$entry" != absent ] || fail "$source"
  mv -nT -- "$source" "$dest" || fail "$dest"
  directory "$source"
  [ "$entry" = absent ] || fail "$source"
  directory "$dest"
  [ "$entry" != absent ] || fail "$dest"
  base=$dest
  changed=yes
  keys
  check_peers
fi
action=unlink
keys
if [ "$purge" = true ]; then
  directory "$dest"
  if [ "$entry" != absent ]; then
    unmounted "$dest" "$dest_mount"
    check_peers
    rm --one-file-system -rf -- "$dest" || fail "$dest"
    directory "$dest"
    [ "$entry" = absent ] || fail "$dest"
    changed=yes
  fi
fi
if [ "$changed" = yes ]; then printf 'changed\n'; else printf 'clean\n'; fi
`
	return script, paths
}
