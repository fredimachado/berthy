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
out=$("$BIN" setup -e "$ENVN" --project "$PROJECT" --host "ops@$server" --listen "$ip:7710" --manifest-dir "$RUN/setup-empty" --config "$cfg" --binary "$BIN" --json 2>/dev/null); code=$?
if [ "$code" -ne 0 ]; then fail "initial setup" "setup failed"; exit 1; fi
unset out
old=$(d exec "$server" sha256sum /usr/local/bin/berthy) || exit 1
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
out=$("$BIN" setup -e "$ENVN" --project "$PROJECT" --host "ops@$server" --listen "$ip:7710" --manifest-dir "$RUN/setup-empty" --config "$cfg" --binary /bin/false --json 2>/dev/null); code=$?
expect_code "upgrade loses connection" 1 "$code"
if [ ! -f "$SSH_STATE.dropped" ] || [ -f "$SSH_STATE.wrong" ]; then fail "restart connection dropped" "unexpected SSH call order"; fi
if [[ $out == *"server restarts the agent"* && $out != *"may have armed"* ]]; then pass "guard reported"; else fail "guard reported" "missing confirmed guard description"; fi
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