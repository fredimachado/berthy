#!/bin/bash
# Upgrade an installed unit and recover when its listen address appears late.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
export FS_HOME="$RUN/setup-late-home" FS_DOCKER_HOST="unix://$SOCK" DOCKER_CONFIG="$E2E_ROOT/docker-config"
export HOME="$FS_HOME"
mkdir -p "$FS_HOME/bin" "$RUN/setup-empty" || exit 1
ln -sf "$DOCKER_BIN" "$FS_HOME/bin/docker" || exit 1
export PATH="$FS_HOME/bin:/usr/bin:/bin"
fixture="$REPO/test/fakeserver/fakeserver.sh"
private=$(mktemp -d /dev/shm/berthy-late-listen.XXXXXXXX) || exit 1
trap 'rm -rf "$private"' EXIT
scope=$(printf '%s' "$PROJECT/$ENVN" | sha256sum | cut -c1-8)
nonce=$(printf '%s' "$private" | sha256sum | cut -c1-16)
server="l${scope}-${nonce}"
export FS_OWNER="$nonce"
die() { echo "late listen fixture: $*" >&2; exit 1; }
existing=$(d ps -aq --no-trunc --filter "name=^/${server}\$") || exit 1
[ -z "$existing" ] || die "container name already in use"
volumes=$(d volume ls -q) || exit 1
for volume in "$server-docker" "$server-containerd"; do
  if grep -Fxq -- "$volume" <<<"$volumes"; then die "volume name already in use"; fi
done
cleanup() {
  bash "$fixture" rm "$server" >/dev/null 2>&1 || echo "could not remove late listen fixture" >&2
  rm -rf "$private"
}
trap cleanup EXIT
bash "$fixture" start "$server" >/dev/null || die "could not start server"
ip=$(bash "$fixture" ip "$server") || exit 1
remote() { bash "$fixture" exec "$server" "$@"; }
ssh_server() { ssh -o BatchMode=yes -T -- "ops@$server" "$1"; }
if [ -n "${FS_IMAGE:-}" ]; then
  remote systemctl start docker || die "could not start Docker"
else
  bash "$fixture" install-docker "$server" >"$private/install.log" 2>&1 || die "could not install Docker"
fi
ssh_server 'sudo -n sh -c "docker version"' >/dev/null || die "Docker unavailable over SSH"
setup=("$BIN" setup --project "$PROJECT" --host "ops@$server" --listen "$ip:7710" \
  --manifest-dir "$RUN/setup-empty" --config "$private/cli.json" --no-install-docker --binary "$BIN" --json -e "$ENVN")
run_setup() { "${setup[@]}" </dev/null >"$private/$1.json" 2>"$private/$1.stderr"; }
health() { remote /usr/local/bin/berthy agent setup-probe health --listen "$1:7710" --timeout 3s >/dev/null 2>&1; }
run_setup initial || die "initial setup failed"
health "$ip" || die "initial health check failed"
remote cp -p /etc/berthy/agent.json /root/agent-original.json || die "could not save configuration"
# Recreate the previously shipped policy on an otherwise healthy installation.
remote sed -i -e '/^StartLimitIntervalSec=/d' -e '/^RestartSec=/d' -e 's/^Restart=.*/Restart=on-failure/' /etc/systemd/system/berthy.service || die "could not install old policy"
remote systemctl daemon-reload || die "could not load old policy"
remote systemctl is-active --quiet berthy && remote systemctl is-enabled --quiet berthy || die "old service is not active and enabled"
echo "old policy: $(remote systemctl show berthy -p Restart -p RestartUSec -p StartLimitIntervalUSec -p StartLimitBurst | tr '\n' ' ')"
late=10.254.90.61
remote ip address show to "$late/32" | grep -q . && die "late address already assigned"
set_late() {
  remote sh -ec "sed '/\"listen\"[[:space:]]*:/s/$ip:7710/$late:7710/' /root/agent-original.json > /root/agent-late.json; cat /root/agent-late.json > /etc/berthy/agent.json" || die "could not change listen address"
}
cursor() { remote journalctl -u berthy -n 0 --show-cursor --no-pager | sed -n 's/^-- cursor: //p'; }
wait_failures() {
  local after=$1 limit=$2
  remote timeout 240s sh -c '
    dir=$(mktemp -d) || exit 1
    mkfifo "$dir/journal" || exit 1
    stdbuf -oL journalctl -u berthy --after-cursor "$1" -f -o cat >"$dir/journal" &
    follower=$!
    trap '\''kill "$follower" 2>/dev/null; wait "$follower" 2>/dev/null; rm -rf "$dir"'\'' EXIT
    awk -v address="$2" -v limit="$3" '\''
      /[Ss]tart request repeated too quickly/ { print "unexpected start limit"; exit 1 }
      index($0, address) && /cannot assign requested address/ { n++; print "bind failure " n; fflush(); if (n >= limit) exit 0 }
      END { if (n < limit) exit 1 }
    '\'' <"$dir/journal"
  ' sh "$after" "$late:7710" "$limit"
}
run_setup upgrade || die "upgrade setup failed"
jq -e 'any(.steps[]; .step == "service" and .status == "changed" and .detail == "restarted berthy.service (unit changed)")' "$private/upgrade.json" >/dev/null || die "setup did not report a unit change"
health "$ip" || die "upgraded service did not answer"
settings=$(remote systemctl show berthy -p Restart -p RestartUSec -p StartLimitIntervalUSec) || die "could not read loaded settings"
echo "upgraded policy: $settings"
grep -Fxq 'Restart=always' <<<"$settings" && grep -Fxq 'RestartUSec=5s' <<<"$settings" && grep -Fxq 'StartLimitIntervalUSec=0' <<<"$settings" || die "incorrect loaded policy"
pass "setup upgrades the loaded restart policy"
remote systemctl stop berthy || die "could not stop upgraded service"
set_late
new_cursor=$(cursor) && [ -n "$new_cursor" ] || die "could not capture journal cursor"
remote systemctl start berthy >/dev/null 2>&1 || true
wait_failures "$new_cursor" 7 || die "retries did not continue"
remote ip address add "$late/32" dev lo || die "could not assign late address"
# The journal signals startup, then the probe establishes readiness.
remote timeout 120s sh -c 'while ! /usr/local/bin/berthy agent setup-probe health --listen "$1:7710" --timeout 3s >/dev/null 2>&1; do :; done' sh "$late" || die "agent did not recover"
remote systemctl is-active --quiet berthy || die "agent is not active"
pass "agent recovers after the address appears without intervention"
finish
