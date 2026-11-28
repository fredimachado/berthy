#!/bin/bash
# An older release CLI refuses to downgrade a newer agent, and doctor says to upgrade the CLI.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
mkdir -p "$E2E_ROOT/bin" "$RUN/no-downgrade-empty"
CLI910=$E2E_ROOT/bin/berthy-v910
CLI900=$E2E_ROOT/bin/berthy-v900
# Stamp before HOME is moved into the fixture home, so go does not write its module cache there.
stamped "$CLI910" v9.1.0 "$SRC" || exit 1
stamped "$CLI900" v9.0.0 "$SRC" || exit 1
export FS_HOME="$RUN/no-downgrade-home" FS_DOCKER_HOST="unix://$SOCK" DOCKER_CONFIG="$E2E_ROOT/docker-config"
export HOME="$FS_HOME"
mkdir -p "$FS_HOME/bin"
ln -sf "$DOCKER_BIN" "$FS_HOME/bin/docker"
export PATH="$FS_HOME/bin:/usr/bin:/bin"
fixture="$REPO/test/fakeserver/fakeserver.sh"
private=$(mktemp -d /dev/shm/berthy-no-downgrade.XXXXXXXX) || exit 1
chmod 700 "$private" || exit 1
scope=$(printf '%s' "$PROJECT/$ENVN" | sha256sum | cut -c1-8)
nonce=$(printf '%s' "$private" | sha256sum | cut -c1-16)
server="n${scope}-${nonce}"
export FS_OWNER="$nonce"
existing=$(d ps -aq --no-trunc --filter "name=^/${server}\$") || { rm -rf "$private"; exit 1; }
if [ -n "$existing" ]; then echo "fixture name already in use" >&2; rm -rf "$private"; exit 1; fi
volumes=$(d volume ls -q) || { rm -rf "$private"; exit 1; }
for volume in "$server-docker" "$server-containerd"; do
  if printf '%s\n' "$volumes" | grep -Fxq -- "$volume"; then echo "fixture volume already in use" >&2; rm -rf "$private"; exit 1; fi
done
cleanup() { bash "$fixture" rm "$server" >/dev/null 2>&1 || echo "could not remove setup fixture" >&2; rm -rf "$private"; }
trap cleanup EXIT
bash "$fixture" start "$server" >/dev/null || exit 1
ip=$(bash "$fixture" ip "$server") || exit 1

cfg="$RUN/no-downgrade.json"
setup_failure() {
  jq -r '.steps[]? | select(.status == "failed") | "\(.step): \(.detail)"' <<<"$out" >&2 2>/dev/null || true
  cat "$1" >&2
}

# Install the newer release on the server by uploading that stamped CLI as --binary.
out=$("$CLI910" setup -e "$ENVN" --project "$PROJECT" --host "ops@$server" --listen "$ip:7710" --manifest-dir "$RUN/no-downgrade-empty" --config "$cfg" --binary "$CLI910" --json 2>"$private/initial.stderr"); code=$?
if [ "$code" -ne 0 ]; then fail "initial setup" "setup failed"; setup_failure "$private/initial.stderr"; exit 1; fi
token=$(jq -er '.token | select(startswith("bty."))' <<<"$out") || { fail "initial setup" "no token"; exit 1; }
token_env=$(jq -er --arg p "$PROJECT" --arg e "$ENVN" '.projects[$p].environments[$e].token_env | strings' "$cfg") || { fail "token_env" "missing"; exit 1; }
export "$token_env=$token"
unset out

server_ver=$(d exec "$server" /usr/local/bin/berthy version) || exit 1
expect_contains "server starts on v9.1.0" "$server_ver" "berthy v9.1.0"

# Older release CLI uploads itself on Linux of the server's arch; it must refuse instead of downgrading.
out=$("$CLI900" setup -e "$ENVN" --project "$PROJECT" --host "ops@$server" --listen "$ip:7710" --manifest-dir "$RUN/no-downgrade-empty" --config "$cfg" --json 2>"$private/older.stderr"); code=$?
expect_code "older CLI refuses to downgrade" 1 "$code"
if [ "$code" -ne 1 ]; then setup_failure "$private/older.stderr"; fi
expect_contains "check step names the newer agent" "$out" "the server runs berthy v9.1.0, newer than this CLI (v9.0.0): upgrade this CLI, or pass --binary to install another build"
if ! jq -e '[.steps[] | select(.step == "check" and .status == "failed")] | length == 1' <<<"$out" >/dev/null; then
  fail "check step failed" "missing failed check step"
  setup_failure "$private/older.stderr"
fi
unset out

server_ver=$(d exec "$server" /usr/local/bin/berthy version) || exit 1
expect_contains "server still runs v9.1.0" "$server_ver" "berthy v9.1.0"

out=$("$CLI900" doctor -e "$ENVN" --config "$cfg" 2>&1); code=$?
expect_code "doctor from the older CLI" 0 "$code" "$out"
expect_contains "doctor says upgrade this CLI" "$out" "note: the agent runs v9.1.0, this CLI is v9.0.0; upgrade this CLI"
expect_not_contains "doctor does not recommend setup" "$out" "berthy setup upgrades the agent"
unset out token
unset "$token_env"
finish
