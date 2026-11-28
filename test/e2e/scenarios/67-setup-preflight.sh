#!/bin/bash
# Setup refuses inputs it can check before changing the server.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
export FS_HOME="$RUN/setup-preflight-home" FS_DOCKER_HOST="unix://$SOCK" DOCKER_CONFIG="$E2E_ROOT/docker-config"
export HOME="$FS_HOME"
mkdir -p "$FS_HOME/bin" "$RUN/setup-preflight-empty" || exit 1
ln -sf "$DOCKER_BIN" "$FS_HOME/bin/docker" || exit 1
export PATH="$FS_HOME/bin:/usr/bin:/bin"
fixture="$REPO/test/fakeserver/fakeserver.sh"
private=$(mktemp -d /dev/shm/berthy-preflight.XXXXXXXX) || exit 1
scope=$(printf '%s' "$PROJECT/$ENVN" | sha256sum | cut -c1-8)
nonce=$(printf '%s' "$private" | sha256sum | cut -c1-16)
server="p${scope}-${nonce}"
export FS_OWNER="$nonce"
die() { echo "setup preflight fixture: $*" >&2; exit 1; }
existing=$(d ps -aq --no-trunc --filter "name=^/${server}\$") || die "cannot list containers"
[ -z "$existing" ] || die "container name already in use"
volumes=$(d volume ls -q) || die "cannot list volumes"
for volume in "$server-docker" "$server-containerd"; do
  if grep -Fxq -- "$volume" <<<"$volumes"; then die "volume name already in use"; fi
done
cleanup() { bash "$fixture" rm "$server" >/dev/null 2>&1 || echo "could not remove fixture" >&2; rm -rf "$private"; }
trap cleanup EXIT
bash "$fixture" start "$server" >/dev/null || die "could not start server"
ip=$(bash "$fixture" ip "$server") || die "cannot find fixture IP"
remote() { bash "$fixture" exec "$server" "$@"; }
baseline() {
  remote sh -c 'command -v docker || :; systemctl is-active docker || :; docker info --format "{{.Swarm.LocalNodeState}}" 2>/dev/null || :; docker network ls --format "{{.Name}}" 2>/dev/null | sort || :; id berthy 2>/dev/null || :; for p in /usr/local/bin/berthy /usr/local/bin/berthy.new /etc/berthy /var/lib/berthy; do test ! -e "$p" || ls -ld "$p"; done'
}
baseline >"$private/before"
"$BIN" setup -e "$ENVN" --project "$PROJECT" --host "ops@$server" --listen "$ip:7710" --manifest-dir "$RUN/setup-preflight-empty" --config "$private/config.json" --binary "$BIN" --key-file "$private/missing" --json </dev/null >"$private/result.json" 2>"$private/error"; code=$?
expect_code "missing key rejected" 1 "$code"
if jq -e '(.steps | length == 2) and .steps[0].step == "connect" and .steps[0].status == "ok" and .steps[1].step == "check" and .steps[1].status == "failed" and .steps[1].detail == "cannot read --key-file"' "$private/result.json" >/dev/null; then pass "missing key fails at check"; else fail "missing key fails at check" "unexpected steps"; jq -r '.steps[] | "\(.step)=\(.status) \(.detail)"' "$private/result.json"; fi
baseline >"$private/after"
if cmp -s "$private/before" "$private/after"; then pass "server baseline unchanged"; else fail "server baseline unchanged" "server changed"; diff -u "$private/before" "$private/after" || :; fi
finish
