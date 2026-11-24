#!/bin/bash
# Run setup on a disposable SSH server without a machine credential store.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"

server="s${PROJECT}-${ENVN}-${FIXTURE_ID:0:6}"
if [[ ! $server =~ ^[a-z][a-z0-9-]{0,30}$ ]]; then
  echo "fixture server name is invalid or too long" >&2
  exit 1
fi
export FS_HOME="$RUN/setup-home"
export HOME="$FS_HOME"
export FS_DOCKER_HOST="unix://$SOCK"
export DOCKER_CONFIG="$E2E_ROOT/docker-config"
mkdir -p "$FS_HOME/bin"
ln -sf "$DOCKER_BIN" "$FS_HOME/bin/docker"
export PATH="$FS_HOME/bin:/usr/bin:/bin"
fixture="$REPO/test/fakeserver/fakeserver.sh"
if [ -n "$(d ps -aq --filter "name=^/${server}\$" --filter 'label=berthy.fakeserver=1')" ]; then
  echo "setup fixture name is already in use" >&2
  exit 1
fi
cleanup() { bash "$fixture" rm "$server" >/dev/null 2>&1 || echo "could not remove setup fixture" >&2; }
trap cleanup EXIT
if ! bash "$fixture" start "$server" >/dev/null; then
  echo "could not start the setup fixture" >&2
  exit 1
fi
ip=$(bash "$fixture" ip "$server") || exit 1
cfg="$RUN/setup-cli.json"
mkdir -p "$RUN/setup-empty"
# Capture the generated token only in memory. Never include setup output in a failure log.
out=$("$BIN" setup -e "$ENVN" --project "$PROJECT" --host "ops@$server" --listen "$ip:7710" --manifest-dir "$RUN/setup-empty" --config "$cfg" --json 2>/dev/null); code=$?
expect_code "setup on a fresh server" 0 "$code"
if [ "$code" -eq 0 ]; then
  jq -e --arg prefix "$FS_HOME/.berthy/keys/berthy.$PROJECT.$ENVN." '.ok and (.key_copy | startswith($prefix)) and ([.steps[] | select(.step == "cli_key" and .status == "changed")] | length == 1) and (.next_steps[0] | contains("keep the copy of the encryption key")) and (.token | startswith("bty.")) and ([.steps[] | select(.step == "cli_token" and .status == "skipped" and (.detail | contains("no credential store on this machine")))] | length == 1) and ([.next_steps[] | select(contains("store the admin token above"))] | length == 1)' <<<"$out" >/dev/null || {
    echo "setup did not report the unstored token" >&2
    exit 1
  }
  key_copy=$(jq -er .key_copy <<<"$out") || exit 1
  if [ "$(stat -c '%a %s' "$key_copy")" != "600 32" ] || [ "$(stat -c %a "$FS_HOME/.berthy")" != 700 ] || [ "$(stat -c %a "$FS_HOME/.berthy/keys")" != 700 ]; then echo "key copy permissions or size are wrong" >&2; exit 1; fi
fi
unset out
finish
