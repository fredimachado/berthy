#!/bin/bash
# Install over SSH with Docker present and neither curl nor wget on the server.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
export FS_HOME="$RUN/setup-health-home" FS_DOCKER_HOST="unix://$SOCK" DOCKER_CONFIG="$E2E_ROOT/docker-config"
export HOME="$FS_HOME"
mkdir -p "$FS_HOME/bin" "$RUN/setup-empty" || exit 1
ln -sf "$DOCKER_BIN" "$FS_HOME/bin/docker" || exit 1
export PATH="$FS_HOME/bin:/usr/bin:/bin"
fixture="$REPO/test/fakeserver/fakeserver.sh"
private=$(mktemp -d /dev/shm/berthy-setup-health.XXXXXXXX) || exit 1
trap 'rm -rf "$private"' EXIT
scope=$(printf '%s' "$PROJECT/$ENVN" | sha256sum | cut -c1-8)
nonce=$(printf '%s' "$private" | sha256sum | cut -c1-16)
server="h${scope}-${nonce}"
export FS_OWNER="$nonce"

die() { echo "setup health fixture: $*" >&2; exit 1; }
existing=$(d ps -aq --no-trunc --filter "name=^/${server}\$") || exit 1
[ -z "$existing" ] || die "container name already in use"
volumes=$(d volume ls -q) || exit 1
for volume in "$server-docker" "$server-containerd"; do
  if grep -Fxq -- "$volume" <<<"$volumes"; then die "volume name already in use"; fi
done
cleanup() {
  bash "$fixture" rm "$server" >/dev/null 2>&1 || echo "could not remove setup health fixture" >&2
  rm -rf "$private"
}
trap cleanup EXIT
bash "$fixture" start "$server" >/dev/null || die "could not start server"
ip=$(bash "$fixture" ip "$server") || exit 1

remote() { bash "$fixture" exec "$server" "$@"; }
ssh_server() { ssh -o BatchMode=yes -T -- "ops@$server" "$1"; }
if [ -n "${FS_IMAGE:-}" ]; then
  remote systemctl start docker || die "could not start Docker in $FS_IMAGE"
else
  bash "$fixture" install-docker "$server" >"$private/install.log" 2>&1 || die "could not install Docker"
fi
ssh_server 'sudo -n sh -c "docker version"' >/dev/null || die "Docker daemon unavailable over SSH"

remote sh -ec '
  hidden=$(mktemp -d /root/setup-health-tools.XXXXXXXX)
  for tool in curl wget; do
    while path=$(command -v "$tool"); do
      mv -- "$path" "$(mktemp "$hidden/$tool.XXXXXXXX")"
    done
  done
' || die "could not move fetch tools"
ssh_server 'if command -v curl || command -v wget; then exit 1; fi
sudo -n sh -c "! command -v curl && ! command -v wget"' || die "fetch tools still resolve or SSH failed"
pass "Docker answers over SSH and neither fetch tool resolves"

cfg="$private/cli.json"
"$BIN" setup -e "$ENVN" --project "$PROJECT" --host "ops@$server" \
  --listen "$ip:7710" --manifest-dir "$RUN/setup-empty" --config "$cfg" \
  --no-install-docker --binary "$BIN" --json </dev/null >"$private/initial.json" 2>"$private/initial.stderr"
code=$?
expect_code "setup without curl or wget" 0 "$code"
if [ "$code" -ne 0 ]; then
  # The report can contain an admin token. Print only status fields and the fixed failure message.
  jq -r '.steps[] | "\(.step): \(.status)"' "$private/initial.json"
  if jq -e 'any(.steps[]; .step == "service" and (.detail | contains("the agent did not come up and is left stopped")))' \
    "$private/initial.json" >/dev/null; then
    echo "  service: the agent did not come up and is left stopped"
  fi
fi
if jq -e 'any(.steps[]; .step == "service" and .status == "changed")' "$private/initial.json" >/dev/null; then
  pass "setup starts the service"
else
  fail "setup starts the service" "service step did not report a successful start"
fi
if jq -e 'any(.steps[]; .step == "verify" and .status == "ok")' "$private/initial.json" >/dev/null; then
  pass "setup verifies the agent"
else
  fail "setup verifies the agent" "verify step did not succeed"
fi
active=$(remote systemctl is-active berthy) || true
expect_eq "berthy.service is active" active "$active"
http_code=$(curl --noproxy '*' --max-time 30 -sS -o /dev/null -w '%{http_code}' "http://$ip:7710/healthz") || true
expect_eq "agent answers /healthz from the operator" 200 "$http_code"
finish
