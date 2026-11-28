#!/bin/bash
# Unhost one of two projects over SSH, including rollback, archive and purge.
set -euo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"
umask 077
private=$(mktemp -d /dev/shm/berthy-removal.XXXXXXXX)
export FS_HOME="$private/home" FS_DOCKER_HOST="unix://$SOCK" DOCKER_CONFIG="$E2E_ROOT/docker-config"
export HOME="$FS_HOME"
export BERTHY_TOKEN_STORE=none
export PATH="$FS_HOME/bin:/usr/bin:/bin"
fixture="$REPO/test/fakeserver/fakeserver.sh"
scope=$(printf '%s' "$PROJECT/$ENVN" | sha256sum | cut -c1-8)
nonce=$(printf '%s' "$private" | sha256sum | cut -c1-16)
server="r${scope}-${nonce}"
export FS_OWNER="$nonce"
started=0 tunnel=0
cleanup() {
  if [ "$tunnel" -eq 1 ]; then
    /usr/bin/ssh -F "$FS_HOME/.ssh/config" -S "$private/registry-ssh" -O exit "$server" >/dev/null 2>&1 || true
  fi
  if [ "$started" -eq 1 ]; then
    bash "$fixture" rm "$server" >/dev/null 2>&1 || echo "could not remove the SSH fixture" >&2
  fi
  rm -rf -- "$private"
}
trap cleanup EXIT
mkdir -p "$FS_HOME/bin" "$private/target" "$private/peer"
ln -s "$DOCKER_BIN" "$FS_HOME/bin/docker"
check() { if ! "$@" >/dev/null; then fail "$label" "fixture assertion failed"; finish; exit 1; fi; }

label="SSH fixture names are unused"
existing=$(d ps -aq --no-trunc --filter "name=^/${server}\$")
check test -z "$existing"
volumes=$(d volume ls -q)
for volume in "$server-docker" "$server-containerd"; do
  if printf '%s\n' "$volumes" | grep -Fxq -- "$volume"; then
    fail "$label" "fixture volume already exists"; finish; exit 1
  fi
done
started=1
bash "$fixture" start "$server"
ip=$(bash "$fixture" ip "$server")
inside() { bash "$fixture" exec "$server" "$@"; }
label="nested Docker is ready"
docker_log="$LOG/remove-hosted-install-docker.log"
start_docker() {
  local available
  available=$(inside sh -c 'if command -v docker >/dev/null && command -v dockerd >/dev/null; then echo present; else echo missing; fi') || return 1
  case "$available" in
    present) inside systemctl start docker ;;
    missing) bash "$fixture" install-docker "$server" ;;
    *) return 1 ;;
  esac
}
if ! start_docker > "$docker_log" 2>&1; then
  fail "$label" "see remove-hosted-install-docker.log"; finish; exit 1
fi
check inside docker version --format '{{.Server.Version}}'
pass "$label"
ssh -M -S "$private/registry-ssh" -fNT -o ExitOnForwardFailure=yes -R "$REG:$REG" "$server"
tunnel=1
check inside curl -fsS -o /dev/null "http://$REG/v2/"

peer="$PROJECT-peer"
network="$PROJECT-$ENVN"
peer_network="$peer-$ENVN"
cfg="$private/config.json"
base_manifest > "$private/target/berthy.yml"
sed "s/^project: $PROJECT\$/project: $peer/" "$private/target/berthy.yml" > "$private/peer/berthy.yml"
setup_pair() {
  local project=$1 dir=$2 token variable
  if ! "$BIN" setup --project "$project" -e "$ENVN" --host "ops@$server" --listen "$ip:7710" \
    --config "$cfg" --manifest-dir "$dir" --binary "$BIN" --no-install-docker --json > "$private/setup.json"; then
    fail "ordinary setup of $project/$ENVN" "inspect the fixture setup steps"
    jq -r '.steps[]? | select(.status == "failed") | .step' "$private/setup.json" >&2
    finish
    exit 1
  fi
  token=$(jq -er '.token' "$private/setup.json")
  variable=$(jq -er --arg p "$project" --arg e "$ENVN" '.projects[$p].environments[$e].token_env' "$cfg")
  export "$variable=$token"
}
cli() { local project=$1; shift; "$BIN" "$@" --project "$project" -e "$ENVN" --config "$cfg"; }
remove() { cli "$PROJECT" setup --remove --host "ops@$server" --manifest-dir "$private/target" "$@"; }
agent_info() { cli "$1" agent-info --json >/dev/null; }
setup_pair "$PROJECT" "$private/target"
operator_key=$(jq -er '.key_copy' "$private/setup.json")
cp -- "$operator_key" "$private/saved-key"
setup_pair "$peer" "$private/peer"
label="both hosted pairs answer with their own credentials"
check agent_info "$PROJECT"
check agent_info "$peer"
pass "$label"

export REMOVAL_DROP="$private/removal-drop"
cat > "$FS_HOME/bin/ssh" <<'SH'
#!/bin/sh
case "$*" in *'# check applied apps'*) if [ ! -f "$REMOVAL_DROP" ]; then touch "$REMOVAL_DROP"; exit 255; fi;; esac
exec /usr/bin/ssh -F "$FS_HOME/.ssh/config" "$@"
SH
chmod 700 "$FS_HOME/bin/ssh"
code=0
remove --dry-run --json > "$private/dropped.json" 2> "$private/dropped.stderr" || code=$?
label="lost applied-app check connection"
check test -f "$REMOVAL_DROP"
check test "$code" -eq 1
jq -r '.steps[] | select(.status == "failed") | .detail' "$private/dropped.json" >&2
check jq -e --arg target "ops@$server" 'any(.steps[]; .step == "check" and .status == "failed" and (.detail | contains("SSH connection to " + $target + " failed")))' "$private/dropped.json"
check agent_info "$PROJECT"
rm -f "$FS_HOME/bin/ssh"

if ! remove --dry-run --json > "$private/initial.json" 2> "$private/initial.stderr"; then
  fail "preview hosted removal" "$(sed -n '1p' "$private/initial.stderr")"
  finish
  exit 1
fi
label="preview works on a fresh hosted pair"
check jq -e '.ok and .remove and .dry_run' "$private/initial.json"
pass "$label"

cli "$PROJECT" deploy web --version v1 --manifest-dir "$private/target" >/dev/null
cli "$peer" deploy web --version v1 --manifest-dir "$private/peer" >/dev/null
cli "$peer" history web --json > "$private/peer-history"
cp -- "$cfg" "$private/original-cli"
inside cat /etc/berthy/agent.json > "$private/original-agent"
config_unchanged() {
  cmp -s "$cfg" "$private/original-cli" && inside cat /etc/berthy/agent.json | cmp -s "$private/original-agent" -
}
for preview in --dry-run ''; do
  args=()
  if [ -n "$preview" ]; then args+=("$preview"); fi
  code=0
  remove --json "${args[@]}" > "$private/blocked.json" || code=$?
  label="applied releases block removal ${preview:-execution}"
  check test "$code" -eq 1
  check jq -e --arg want "berthy destroy --project $PROJECT -e $ENVN --confirm $PROJECT/$ENVN --allow-recreate" \
    '.remove and (any(.steps[]; .status == "failed" and (.detail | contains($want))))' "$private/blocked.json"
  check config_unchanged
  pass "$label"
done
cli "$PROJECT" destroy --confirm "$PROJECT/$ENVN" --allow-recreate >/dev/null
label="destroy clears applied rows while keeping release history"
cli "$PROJECT" history web --json > "$private/target-history"
check jq -e --arg digest "$WEB_V1_DIGEST" 'any(.[]; .app == "web" and (.digest? // "" | endswith($digest)))' "$private/target-history"
pass "$label"

state="/var/lib/berthy/environments/$PROJECT/$ENVN/state"
key="/var/lib/berthy/environments/$PROJECT/$ENVN/key"
peer_state="/var/lib/berthy/environments/$peer/$ENVN/state"
peer_key="/var/lib/berthy/environments/$peer/$ENVN/key"
record="/etc/berthy/remove-$PROJECT.$ENVN.json"
inside mkdir -p "$state/nested"
inside sh -c 'printf "archive marker\n" > "$1/nested/marker"; printf "peer marker\n" > "$2/peer-marker"' sh "$state" "$peer_state"
inside cp -p -- "$peer_key" /root/removal-peer-key
inside cp -p -- "$peer_state/peer-marker" /root/removal-peer-marker
net_id=$(inside docker network inspect --format '{{.Id}}' "$network")
peer_net_id=$(inside docker network inspect --format '{{.Id}}' "$peer_network")
target_stat=$(inside stat -c %d:%i:%u:%a -- "$state" "$key")
service_invocation=$(inside systemctl show -p InvocationID --value berthy)
peer_bytes() {
  python3 -c '
import json, re, sys
text = sys.stdin.read()
at = re.search(r"\x22environments\x22\s*:\s*\[", text).end()
decoder = json.JSONDecoder()
while True:
    at = re.compile(r"[\s,]*").match(text, at).end()
    if text[at] == "]":
        sys.exit(1)
    obj, end = decoder.raw_decode(text, at)
    if (obj["project"], obj["environment"]) == tuple(sys.argv[1:]):
        sys.stdout.write(text[at:end])
        break
    at = end
' "$peer" "$ENVN"
}
peer_bytes < "$private/original-agent" > "$private/peer-object"
jq -c --arg p "$peer" --arg e "$ENVN" '.projects[$p].environments[$e]' "$cfg" > "$private/peer-context"
remove --dry-run --verbose --json > "$private/preview.json" 2> "$private/preview.stderr"
label="preview reports archive, keys, network and context without writes"
check jq -e '.ok and .remove and .dry_run and (["state","network","cli_config"] - [.steps[] | select(.status == "would_change") | .step] == [])' "$private/preview.json"
check config_unchanged
check inside test ! -e "$record"
check test "$(inside stat -c %d:%i:%u:%a -- "$state" "$key")" = "$target_stat"
check test "$(inside docker network inspect --format '{{.Id}}' "$network")" = "$net_id"
check test "$(inside systemctl show -p InvocationID --value berthy)" = "$service_invocation"
pass "$label"

export REMOVAL_TEST_STATE="$private/restart"
cat > "$FS_HOME/bin/ssh" <<'SH'
#!/bin/sh
case "$*" in
  *'mv -f /etc/berthy/agent.json.new /etc/berthy/agent.json'*)
    /usr/bin/ssh -F "$FS_HOME/.ssh/config" "$@"; rc=$?
    [ "$rc" -ne 0 ] || touch "$REMOVAL_TEST_STATE.swapped"
    exit "$rc";;
  *'systemctl restart berthy'*)
    if [ -f "$REMOVAL_TEST_STATE.swapped" ] && [ ! -f "$REMOVAL_TEST_STATE.failed" ]; then
      touch "$REMOVAL_TEST_STATE.failed"
      exit 1
    fi;;
esac
exec /usr/bin/ssh -F "$FS_HOME/.ssh/config" "$@"
SH
chmod 700 "$FS_HOME/bin/ssh"
code=0
remove --json > "$private/rollback.json" || code=$?
label="failed candidate restart restores both hosted pairs and their files"
check test "$code" -eq 1
check test -f "$REMOVAL_TEST_STATE.failed"
check jq -e '.remove and (any(.steps[]; .step == "service" and .status == "failed" and (.detail | contains("the previous one was put back"))))' "$private/rollback.json"
check config_unchanged
check test "$(inside stat -c %d:%i:%u:%a -- "$state" "$key")" = "$target_stat"
check test "$(inside docker network inspect --format '{{.Id}}' "$network")" = "$net_id"
check agent_info "$PROJECT"
check agent_info "$peer"
pass "$label"

target_env=$(jq -er --arg p "$PROJECT" --arg e "$ENVN" '.projects[$p].environments[$e].token_env' "$cfg")
remove --json > "$private/archive.json"
archive=$(inside find "$(dirname "$state")" -mindepth 1 -maxdepth 1 -name 'state.removed-*' -print)
label="ordinary removal archives only the target and removes its vacant network"
check test -n "$archive"
check test "$(printf '%s\n' "$archive" | wc -l)" -eq 1
check jq -e --arg a "$archive" '.ok and .remove and (any(.steps[]; .step == "state" and (.detail | contains($a))))' "$private/archive.json"
check test "$(inside stat -c %a -- "$archive")" = 700
check inside test -f "$archive/state.db"
check test "$(inside cat "$archive/nested/marker")" = "archive marker"
check inside test ! -e "$state"
check inside test ! -e "$key"
check inside test ! -e "$record"
remaining=$(inside docker network ls --no-trunc --filter "name=^${network}\$" -q)
check test -z "$remaining"
code=$(printf 'Authorization: Bearer %s\n' "${!target_env}" |
  curl_local -sS -H @- -o /dev/null -w '%{http_code}' "http://$ip:7710/v1/projects/$PROJECT/environments/$ENVN/agent")
check test "$code" = 401
pass "$label"
label="survivor keeps its exact config, state, key, network, local context and history"
inside cat /etc/berthy/agent.json | peer_bytes | cmp -s "$private/peer-object" -
check inside cmp -s "$peer_key" /root/removal-peer-key
check inside cmp -s "$peer_state/peer-marker" /root/removal-peer-marker
check test "$(inside docker network inspect --format '{{.Id}}' "$peer_network")" = "$peer_net_id"
jq -c --arg p "$peer" --arg e "$ENVN" '.projects[$p].environments[$e]' "$cfg" | cmp -s "$private/peer-context" -
"$BIN" context list --config "$cfg" --json > "$private/contexts"
check jq -e --arg p "$peer" --arg e "$ENVN" 'length == 1 and .[0].project == $p and .[0].environment == $e' "$private/contexts"
check agent_info "$peer"
cli "$peer" history web --json | cmp -s "$private/peer-history" -
pass "$label"

archive_bytes() { inside sh -c 'cd "$1"; find . -type f -print0 | sort -z | xargs -0 -r sha256sum' sh "$archive"; }
archive_bytes > "$private/archive-before"
setup_pair "$PROJECT" "$private/target"
new_net_id=$(inside docker network inspect --format '{{.Id}}' "$network")
attachment="$PROJECT-$ENVN-attached"
cid=$(inside docker run -d --network "$network" --name "$attachment" "$REG/e2e/web:v1")
remove --purge --json > "$private/purge.json"
label="purge keeps an attached network and the previous archive and key copy"
check jq -e '.ok and .remove and (any(.steps[]; .step == "state" and (.detail | startswith("purged ")))) and
  (any(.steps[]; .step == "network" and .status == "skipped" and (.detail | contains("because it is in use"))))' "$private/purge.json"
check inside test ! -e "$state"
check inside test ! -e "$key"
check inside test ! -e "$record"
check test "$(inside docker network inspect --format '{{.Id}}' "$network")" = "$new_net_id"
inside docker container inspect "$cid" | jq -e --arg n "$network" --arg id "$new_net_id" \
  '.[0].State.Running and (.[0].NetworkSettings.Networks[$n].NetworkID == $id)' >/dev/null
archive_bytes | cmp -s "$private/archive-before" -
check cmp -s "$operator_key" "$private/saved-key"
check agent_info "$peer"
pass "$label"
finish
