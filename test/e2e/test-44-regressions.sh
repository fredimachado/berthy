#!/bin/bash
# Synthetic checks for scenario 44's secret scan and copy records.
set -euo pipefail
scenario="$(dirname "$0")/scenarios/44-setup-key-copy.sh"
private=$(mktemp -d) || exit 1
trap 'rm -rf "$private"' EXIT
chmod 700 "$private"
eval "$(sed -n '/^record_files() /p; /^record() /p; /^absent() /p' "$scenario")"
# The old grep invocation reports an option error for this valid encoding.
printf '\373\357' > "$private/key"
head -c 30 /dev/zero >> "$private/key"
encoded=$(base64 -w0 "$private/key" | tr '+/' '-_')
printf 'unrelated\n' > "$private/input"
status=0
grep -Fq "$encoded" "$private/input" >/dev/null 2>&1 || status=$?
[ "$status" -eq 2 ] || { echo "old scan did not reproduce" >&2; exit 1; }
absent "$encoded" "$private/input" || { echo "safe scan failed" >&2; exit 1; }
printf '%s\n' "$encoded" > "$private/input"
if absent "$encoded" "$private/input"; then echo "safe scan missed a match" >&2; exit 1; fi
if absent "$encoded" "$private/missing"; then echo "safe scan accepted an error" >&2; exit 1; fi
keys="$private/keys"
mkdir "$keys"
head -c 32 /dev/zero | tr '\000' '\001' > "$keys/a"
head -c 32 /dev/zero > "$keys/b"
record > "$private/original"
{ stat -c '%n %i %y' "$keys/a" "$keys/b" | sort; sha256sum "$keys/a" "$keys/b" | sort; } > "$private/old"
if cmp -s "$private/original" "$private/old"; then echo "old ordering did not reproduce" >&2; exit 1; fi
record_files "$keys/b" "$keys/a" > "$private/new"
cmp -s "$private/original" "$private/new" || { echo "copy record ordering failed" >&2; exit 1; }
# Exercise owner filtering and partial cleanup without a Docker daemon.
(
  export FS_OWNER=our-run FS_HOME="$private/home"
  . "$(dirname "$0")/../fakeserver/fakeserver.sh"
  d() {
    case "$1 $2" in
      'image inspect') return 0 ;;
      'ps -aq')
        case " $* " in *' label=berthy.fakeserver.owner=our-run '*) return 0 ;; *) printf 'foreign-container\n' ;; esac ;;
      'volume inspect')
        case "$*" in
          *'{{index .Labels "berthy.fakeserver.owner"}}'*owned-docker*) printf 'our-run\n' ;;
          *'{{index .Labels "berthy.fakeserver.owner"}}'*) printf 'other-run\n' ;;
          *'{{index .Labels "berthy.fakeserver"}}'*) printf '1\n' ;;
        esac ;;
      'volume rm') printf '%s\n' "${*: -1}" >> "$private/removed" ;;
      *) echo "unexpected mock call" >&2; return 1 ;;
    esac
  }
  ensure_network() { :; }
  ensure_home() { :; }
  if (cmd_start foreign >/dev/null 2>&1); then echo "foreign container was adopted" >&2; exit 1; fi
  cmd_rm foreign >/dev/null 2>&1
  [ ! -e "$private/removed" ] || { echo "foreign volume was removed" >&2; exit 1; }
  cmd_rm owned >/dev/null 2>&1
  [ "$(cat "$private/removed")" = owned-docker ] || { echo "partial fixture cleanup failed" >&2; exit 1; }
)
echo "scenario 44 synthetic regressions passed"
