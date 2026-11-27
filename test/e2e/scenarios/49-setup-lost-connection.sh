#!/bin/bash
# Lose SSH after the upgrade swap and let the server recover the previous agent.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
export FS_HOME="$RUN/lost-connection-home" FS_DOCKER_HOST="unix://$SOCK" DOCKER_CONFIG="$E2E_ROOT/docker-config"
export HOME="$FS_HOME"
mkdir -p "$FS_HOME/bin" "$RUN/setup-empty"
ln -sf "$DOCKER_BIN" "$FS_HOME/bin/docker"
export PATH="$FS_HOME/bin:/usr/bin:/bin"
fixture="$REPO/test/fakeserver/fakeserver.sh"
private=$(mktemp -d /dev/shm/berthy-lost.XXXXXXXX) || exit 1
chmod 700 "$private" || exit 1
scope=$(printf '%s' "$PROJECT/$ENVN" | sha256sum | cut -c1-8)
nonce=$(printf '%s' "$private" | sha256sum | cut -c1-16)
server="l${scope}-${nonce}"
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
cfg="$RUN/lost-connection.json"
setup_failure() {
 jq -r '.steps[]? | select(.status == "failed") | "\(.step): \(.detail)"' <<<"$out" >&2 2>/dev/null || true
 cat "$1" >&2
}
out=$("$BIN" setup -e "$ENVN" --project "$PROJECT" --host "ops@$server" --listen "$ip:7710" --manifest-dir "$RUN/setup-empty" --config "$cfg" --binary "$BIN" --json 2>"$private/initial.stderr"); code=$?
if [ "$code" -ne 0 ]; then fail "initial setup" "setup failed"; setup_failure "$private/initial.stderr"; exit 1; fi
unset out
old=$(d exec "$server" sha256sum /usr/local/bin/berthy) || exit 1
# Make the timer finish before setup's restart, with a different but valid agent binary.
cp "$BIN" "$private/agent-next" || exit 1
printf '\nfixture upgrade\n' >> "$private/agent-next"
export GUARD_SERVER="$server" GUARD_DOCKER="$DOCKER_BIN" GUARD_SOCKET="$SOCK" GUARD_STATE="$private/guard-state"
cat > "$FS_HOME/bin/ssh" <<'SH'
#!/bin/sh
if [ -f "$GUARD_STATE.swap" ] && [ ! -f "$GUARD_STATE.fired" ]; then
 case "$*" in
  *'systemctl restart berthy'*)
   "$GUARD_DOCKER" --host "unix://$GUARD_SOCKET" exec "$GUARD_SERVER" systemctl stop berthy-activation-guard.timer || exit 255
   "$GUARD_DOCKER" --host "unix://$GUARD_SOCKET" exec "$GUARD_SERVER" /bin/sh /run/berthy-activation-guard.sh yes no || exit 255
   touch "$GUARD_STATE.fired" || exit 255
   ;;
  *) touch "$GUARD_STATE.wrong"; exit 255;;
 esac
fi
case "$*" in
 *'mv -f /usr/local/bin/berthy.new'*) /usr/bin/ssh -F "$FS_HOME/.ssh/config" "$@"; rc=$?; [ "$rc" -ne 0 ] || touch "$GUARD_STATE.swap"; exit "$rc";;
esac
exec /usr/bin/ssh -F "$FS_HOME/.ssh/config" "$@"
SH
chmod 700 "$FS_HOME/bin/ssh"
out=$("$BIN" setup -e "$ENVN" --project "$PROJECT" --host "ops@$server" --listen "$ip:7710" --manifest-dir "$RUN/setup-empty" --config "$cfg" --binary "$private/agent-next" --json 2>"$private/delayed.stderr"); code=$?
expect_code "guard finishes before restart" 0 "$code"
if [ "$code" -ne 0 ]; then setup_failure "$private/delayed.stderr"; fi
if [ ! -f "$GUARD_STATE.fired" ] || [ -f "$GUARD_STATE.wrong" ]; then fail "guard ran before restart" "unexpected SSH call order"; fi
expected=$(sha256sum "$private/agent-next" | cut -d' ' -f1)
actual=$(d exec "$server" sha256sum /usr/local/bin/berthy | cut -d' ' -f1)
expect_eq "healthy upgrade retained" "$expected" "$actual"
if d exec "$server" curl -fsS "http://$ip:7710/healthz" >/dev/null; then pass "healthy upgraded agent answers"; else fail "healthy upgraded agent answers" "health check failed"; fi
unset out
# Record the installed generation for the connection-loss case.
old=$(d exec "$server" sha256sum /usr/local/bin/berthy) || exit 1
# A connected failed upgrade runs recovery synchronously on the server.
cat > "$FS_HOME/bin/ssh" <<'SH'
#!/bin/sh
exec /usr/bin/ssh -F "$FS_HOME/.ssh/config" "$@"
SH
chmod 700 "$FS_HOME/bin/ssh"
out=$("$BIN" setup -e "$ENVN" --project "$PROJECT" --host "ops@$server" --listen "$ip:7710" --manifest-dir "$RUN/setup-empty" --config "$cfg" --binary /bin/false --json 2>"$private/connected.stderr"); code=$?
expect_code "connected failed upgrade" 1 "$code"
if [[ $out == *"the new agent did not come up; the previous one was put back"* ]]; then pass "connected rollback reported"; else fail "connected rollback reported" "missing rollback message"; setup_failure "$private/connected.stderr"; fi
unset out
current=$(d exec "$server" sha256sum /usr/local/bin/berthy) || exit 1
expect_eq "connected rollback restored binary" "$old" "$current"
state=$(d exec "$server" cat /run/berthy-activation-guard.state) || exit 1
expect_eq "connected rollback state" "restored" "$state"
if d exec "$server" curl -fsS "http://$ip:7710/healthz" >/dev/null; then pass "connected rollback answers"; else fail "connected rollback answers" "health check failed"; fi
export SSH_STATE="$private/state"
cat > "$FS_HOME/bin/ssh" <<'SH'
#!/bin/sh
if [ -f "$SSH_STATE.dropped" ]; then exit 255; fi
if [ -f "$SSH_STATE.swapped" ]; then
 case "$*" in *'systemctl restart berthy'*) touch "$SSH_STATE.dropped"; exit 255;; *) touch "$SSH_STATE.wrong"; exit 255;; esac
fi
case "$*" in *'mv -f /usr/local/bin/berthy.new'*) /usr/bin/ssh -F "$FS_HOME/.ssh/config" "$@"; rc=$?; [ "$rc" -ne 0 ] || touch "$SSH_STATE.swapped"; exit "$rc";; esac
exec /usr/bin/ssh -F "$FS_HOME/.ssh/config" "$@"
SH
chmod 700 "$FS_HOME/bin/ssh"
out=$("$BIN" setup -e "$ENVN" --project "$PROJECT" --host "ops@$server" --listen "$ip:7710" --manifest-dir "$RUN/setup-empty" --config "$cfg" --binary /bin/false --json 2>"$private/upgrade.stderr"); code=$?
expect_code "upgrade loses connection" 1 "$code"
if [ "$code" -ne 1 ]; then setup_failure "$private/upgrade.stderr"; fi
if [ ! -f "$SSH_STATE.dropped" ] || [ -f "$SSH_STATE.wrong" ]; then fail "restart connection dropped" "unexpected SSH call order"; fi
if [[ $out == *"setup may have started recovery; the server finishes it if it started, and the timer decides otherwise"* ]]; then pass "guard reported"; else fail "guard reported" "missing guard description"; setup_failure "$private/upgrade.stderr"; fi
unset out
rm -f "$SSH_STATE.dropped"
deadline=$((SECONDS+300))
while :; do
 active=$(d exec "$server" systemctl is-active berthy-activation-guard.service 2>/dev/null || true)
 timers=$(d exec "$server" systemctl list-timers --all --no-legend 2>/dev/null) || { fail "guard completion" "cannot list timers"; break; }
 if [ "$active" != active ] && [[ $timers != *berthy-activation-guard* ]]; then break; fi
 if [ "$SECONDS" -ge "$deadline" ]; then fail "guard completion" "timed out"; break; fi
 sleep 3
done
current=$(d exec "$server" sha256sum /usr/local/bin/berthy) || exit 1
expect_eq "previous binary restored" "$old" "$current"
if d exec "$server" systemctl restart berthy && d exec "$server" sh -c 'i=0; while [ "$i" -lt 30 ]; do curl -fsS http://"$1":7710/healthz >/dev/null && exit 0; sleep 1; i=$((i+1)); done; exit 1' sh "$ip"; then pass "restored agent answers"; else fail "restored agent answers" "health check failed"; fi
finish
