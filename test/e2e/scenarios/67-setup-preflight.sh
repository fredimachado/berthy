#!/bin/bash
# Setup refuses inputs it can check before changing the server.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
mkdir -p "$E2E_ROOT/bin" || exit 1
release="$E2E_ROOT/bin/berthy-preflight-release"
stamped "$release" v9.0.0 "$SRC" || exit 1
export FS_HOME="$RUN/setup-preflight-home" FS_DOCKER_HOST="unix://$SOCK" DOCKER_CONFIG="$E2E_ROOT/docker-config"
export HOME="$FS_HOME"
mkdir -p "$FS_HOME/bin" "$RUN/setup-preflight-empty" || exit 1
ln -sf "$DOCKER_BIN" "$FS_HOME/bin/docker" || exit 1
export PATH="$FS_HOME/bin:/usr/bin:/bin"
fixture="$REPO/test/fakeserver/fakeserver.sh"
private=$(mktemp -d /dev/shm/berthy-preflight.XXXXXXXX) || exit 1
trap 'rm -rf "$private"' EXIT
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
# Report a foreign architecture without changing the real executable.
remote sh -ec 'mv /usr/bin/uname /usr/bin/uname.real; cat > /usr/bin/uname <<"EOF"
#!/bin/sh
if [ "$1" = -m ]; then
  case $(/usr/bin/uname.real -m) in x86_64) echo aarch64;; *) echo x86_64;; esac
else
  exec /usr/bin/uname.real "$@"
fi
EOF
chmod 755 /usr/bin/uname' || die "cannot install architecture wrapper"
base_args=(setup -e "$ENVN" --project "$PROJECT" --host "ops@$server" --listen "$ip:7710" --manifest-dir "$RUN/setup-preflight-empty" --config "$private/config.json" --json)
run_refusal() {
  local name=$1 cli=$2 expected=$3
  baseline >"$private/$name.before"
  BERTHY_RELEASE_URL=http://127.0.0.1:1 "$cli" "${base_args[@]}" </dev/null >"$private/$name.json" 2>"$private/$name.err"; local code=$?
  expect_code "$name rejected" 1 "$code"
  if jq -e --arg expected "$expected" '(.steps | length == 2) and .steps[0].step == "connect" and .steps[0].status == "ok" and .steps[1].step == "check" and .steps[1].status == "failed" and (.steps[1].detail | contains($expected))' "$private/$name.json" >/dev/null; then pass "$name fails at check"; else fail "$name fails at check" "unexpected steps"; jq -r '.steps[] | "\(.step)=\(.status) \(.detail)"' "$private/$name.json"; fi
  baseline >"$private/$name.after"
  if cmp -s "$private/$name.before" "$private/$name.after"; then pass "$name leaves server unchanged"; else fail "$name leaves server unchanged" "server changed"; diff -u "$private/$name.before" "$private/$name.after" || :; fi
}
run_refusal dev "$BIN" "pass --binary with a Linux"
# SSH is listening, and port 1 has no listener on the fixture.
remote ss -lnt >"$private/listeners" || die "cannot inspect listeners"
if grep -Eq ":1[[:space:]]" "$private/listeners"; then die "loopback test port is occupied"; fi
run_refusal release "$release" "cannot fetch release v9.0.0 asset"
remote sh -ec 'mv -f /usr/bin/uname.real /usr/bin/uname' || die "cannot restore uname"
"$BIN" "${base_args[@]}" --binary "$BIN" </dev/null >"$private/success.json" 2>"$private/success.err"; code=$?
expect_code "native setup succeeds" 0 "$code"
if [ "$code" -ne 0 ]; then jq -r '.steps[]? | "\(.step)=\(.status) \(.detail)"' "$private/success.json"; fi
finish
