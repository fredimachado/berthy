#!/bin/bash
# A public port belongs to the server even when one agent hosts two environments.
set -euo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"
umask 077

private=$(mktemp -d /dev/shm/berthy-cluster-alerts.XXXXXXXX)
export FS_HOME="$private/home" FS_DOCKER_HOST="unix://$SOCK" DOCKER_CONFIG="$E2E_ROOT/docker-config"
export HOME="$FS_HOME"
export PATH="$FS_HOME/bin:/usr/bin:/bin"
fixture="$REPO/test/fakeserver/fakeserver.sh"
scope=$(printf '%s' "$PROJECT/$ENVN" | sha256sum | cut -c1-8)
nonce=$(printf '%s' "$private" | sha256sum | cut -c1-16)
server="a${scope}-${nonce}"
export FS_OWNER="$nonce"
tunnel="$private/receiver-ssh"
started=0

cleanup() {
  if [ -S "$tunnel" ]; then
    ssh -S "$tunnel" -O exit "$server" >/dev/null 2>&1 || echo "could not stop the receiver forwards" >&2
  fi
  if [ "$started" -eq 1 ]; then
    bash "$fixture" rm "$server"
  fi
}
trap 'cleanup || echo "could not remove the alert server fixture" >&2; rm -rf -- "$private"' EXIT
die() { echo "cluster alerts fixture: $*" >&2; exit 1; }
inside() { bash "$fixture" exec "$server" "$@"; }
mkdir -p "$FS_HOME/bin" "$private/empty"
ln -s "$DOCKER_BIN" "$FS_HOME/bin/docker"

existing=$(d ps -aq --no-trunc --filter "name=^/${server}\$")
[ -z "$existing" ] || die "container name already in use"
volumes=$(d volume ls -q)
for volume in "$server-docker" "$server-containerd"; do
  if grep -Fxq -- "$volume" <<< "$volumes"; then die "volume name already in use"; fi
done
started=1
bash "$fixture" start "$server"
ip=$(bash "$fixture" ip "$server")
if inside sh -c 'command -v docker && command -v dockerd' >/dev/null; then
  inside systemctl start docker
else
  bash "$fixture" install-docker "$server" > "$LOG/cluster-alerts-install-docker.log" 2>&1 || die "could not install Docker"
fi
inside docker swarm init --advertise-addr "$ip" >/dev/null
ssh -M -S "$tunnel" -fNT -o BatchMode=yes -o ExitOnForwardFailure=yes \
  -R "$TG_LISTEN:$TG_LISTEN" -R "$HOOK_LISTEN:$HOOK_LISTEN" -R "$REG:$REG" "$server"

peer="$PROJECT-two"
cfg="$private/cli.json"
setup_on() {
  local project=$1; shift
  "$BIN" setup --project "$project" -e "$ENVN" --host "ops@$server" --manifest-dir "$private/empty" --json "$@"
}
setup_pair() {
  local project=$1 token variable
  if ! setup_on "$project" --listen "$ip:7710" --config "$cfg" --binary "$BIN" --no-install-docker \
    </dev/null > "$private/setup.json" 2> "$private/setup.stderr"; then
    jq -r '.steps[]? | select(.status == "failed") | .step' "$private/setup.json" >&2
    die "could not set up $project/$ENVN"
  fi
  token=$(jq -er '.token | strings' "$private/setup.json")
  variable=$(jq -er --arg p "$project" --arg e "$ENVN" '.projects[$p].environments[$e].token_env | strings' "$cfg")
  export "$variable=$token"
}
cli() { local project=$1; shift; "$BIN" "$@" --project "$project" -e "$ENVN" --config "$cfg"; }
setup_pair "$PROJECT"
setup_pair "$peer"

first_chat=101 second_chat=102 server_chat=103
first_hook="/hooks/$PROJECT/$ENVN" second_hook="/hooks/$peer/$ENVN" server_hook="/hooks/$PROJECT/$ENVN/server"
cluster_dir=/var/lib/berthy/cluster
cluster_identity() { inside stat -c '%d:%i' "$cluster_dir/state/state.db" "$cluster_dir/key"; }
# docker exec -i would otherwise read the credential intended for setup.
invocation() { inside systemctl show -p InvocationID --value berthy </dev/null; }
check_both() {
  local project
  for project in "$PROJECT" "$peer"; do
    cli "$project" agent-info --json |
      jq -e --arg p "$project" --arg e "$ENVN" '.project == $p and .environment == $e' >/dev/null || die "$project/$ENVN did not answer"
  done
}
cluster_layout() { inside cat /etc/berthy/agent.json | jq -ec '.cluster'; }
cluster_before=$(cluster_identity)
expect_eq "setup creates private cluster state and key" $'700 berthy:berthy\n600 berthy:berthy' \
  "$(inside stat -c '%a %U:%G' "$cluster_dir/state" "$cluster_dir/key")"
for project in "$PROJECT" "$peer"; do
  code=0
  inside cmp -s "$cluster_dir/key" "/var/lib/berthy/environments/$project/$ENVN/key" || code=$?
  expect_eq "the cluster key differs from $project/$ENVN's key" 1 "$code"
done
expect_eq "fresh setup has no server destinations" false "$(cluster_layout | jq 'has("alerts")')"
check_both

cluster_request() {
  local method=$1 path=$2 token=$3
  printf 'Authorization: Bearer %s\n' "$token" |
    curl_local -X "$method" --header @- -o "$private/cluster-response.json" -w '%{http_code}' "http://$ip:7710$path"
}
first_scope="/v1/projects/$PROJECT/environments/$ENVN"
second_scope="/v1/projects/$peer/environments/$ENVN"
first_reader=$(cli "$PROJECT" token create cluster-reader --scope read --json | jq -er '.token')
second_reader=$(cli "$peer" token create cluster-reader --scope read --json | jq -er '.token')
expect_eq "the first project's read token can read cluster checks" 200 \
  "$(cluster_request GET "$first_scope/cluster/checks" "$first_reader")"
if [ "$E2E_FAILS" -ne 0 ]; then finish; fi
first_rows=$(jq -ec 'select(type == "array")' "$private/cluster-response.json")
expect_eq "the second project's read token can read cluster checks" 200 \
  "$(cluster_request GET "$second_scope/cluster/checks" "$second_reader")"
second_rows=$(jq -ec 'select(type == "array")' "$private/cluster-response.json")
expect_eq "both scoped reads return the same cluster rows" "$first_rows" "$second_rows"
expect_eq "$PROJECT/$ENVN checks --cluster matches its scoped GET" "$first_rows" \
  "$(cli "$PROJECT" checks --cluster --json | jq -ec 'select(type == "array")')"
expect_eq "$peer/$ENVN checks --cluster matches its scoped GET" "$second_rows" \
  "$(cli "$peer" checks --cluster --json | jq -ec 'select(type == "array")')"
if [ "$E2E_FAILS" -ne 0 ]; then finish; fi
expect_eq "a cluster read refuses the other project's token" 401 \
  "$(cluster_request GET "$second_scope/cluster/checks" "$first_reader")"
expect_eq "cluster checks refuse POST" 405 \
  "$(cluster_request POST "$first_scope/cluster/checks" "$first_reader")"

admin_env=$(jq -er --arg p "$PROJECT" --arg e "$ENVN" '.projects[$p].environments[$e].token_env' "$cfg")
for prefix in "$first_scope/cluster" /v1/cluster; do
  while read -r method path; do
    expect_eq "admin cannot $method $prefix/$path" 404 \
      "$(cluster_request "$method" "$prefix/$path" "${!admin_env}")"
  done <<'REQUESTS'
GET secrets
POST secrets
DELETE secrets/SERVER_TG
PUT settings
POST alerts/test
REQUESTS
done
unset first_reader second_reader

doctor_cluster() {
  local project
  for project in "$PROJECT" "$peer"; do
    cli "$project" doctor --json | jq -e "$1" >/dev/null || die "unexpected host or cluster readiness for $project/$ENVN"
    pass "$project/$ENVN doctor retains live host checks and reports cluster readiness"
  done
}
doctor_cluster '.checks | any(.name == "public_ports" and (.critical | not)) and
  any(.name == "cluster_alerts" and (.ok | not) and (.critical | not) and (.detail | contains("--cluster-alerts FILE")))'

cluster_setup() {
  local what=$1; shift
  local before
  before=$(invocation)
  if ! setup_on "$PROJECT" "$@" > "$private/cluster-setup.json" 2> "$private/cluster-setup.stderr"; then
    jq -r '.steps[]? | select(.status == "failed") | .step' "$private/cluster-setup.json" >&2
    die "$what failed"
  fi
  check_both
  if [ "$what" != 'omit destination flags' ]; then
    [ "$(invocation)" != "$before" ] || die "$what did not start a new agent invocation"
  fi
  [ "$(cluster_identity)" = "$cluster_before" ] || die "$what changed the cluster database or key"
  pass "$what keeps the cluster database and key"
  pass "$what leaves both environments serving"
}
cluster_setup 'preload the Telegram credential' --cluster-secret SERVER_TG < "$RUN/tg-token"
printf 'http://%s%s-preload\r\n' "$HOOK_LISTEN" "$server_hook" |
  cluster_setup 'preload the webhook credential' --cluster-secret SERVER_HOOK
expect_eq "preloading credentials leaves destinations disabled" false "$(cluster_layout | jq 'has("alerts")')"
cat > "$private/cluster.yml" <<YML
reminder: 0s
telegram:
  bot_token_secret: SERVER_TG
  chat_id: "$server_chat"
webhook:
  url_secret: SERVER_HOOK
YML
cluster_setup 'enable server destinations' --config "$cfg" --cluster-alerts "$private/cluster.yml" </dev/null
destinations=$(cluster_layout | jq -ec '.alerts | select(.reminder == "0s" and .telegram.bot_token_secret == "SERVER_TG" and .webhook.url_secret == "SERVER_HOOK")')
expect_eq "setup installs the server chat" "$server_chat" "$(jq -r '.telegram.chat_id' <<< "$destinations")"
doctor_cluster '.checks | any(.name == "public_ports" and (.critical | not)) and
  any(.name == "cluster_telegram" and .ok and (.critical | not)) and
  any(.name == "cluster_webhook" and .ok and (.critical | not))'
jq -e '[.next_steps[] | select(contains("--cluster-secret")) | split(" ")[-1]] == ["SERVER_HOOK", "SERVER_TG"]' \
  "$private/cluster-setup.json" >/dev/null || die "setup did not report both server credential commands"
printf 'http://%s%s\r\n' "$HOOK_LISTEN" "$server_hook" |
  cluster_setup 'replace the webhook credential' --cluster-secret SERVER_HOOK
cluster_setup 'omit destination flags' --config "$cfg" </dev/null
expect_eq "omitting destination flags preserves the channels" "$destinations" "$(cluster_layout | jq -ec '.alerts')"
cluster_setup 'clear server destinations' --config "$cfg" --cluster-alerts none </dev/null
expect_eq "none clears the destinations" false "$(cluster_layout | jq 'has("alerts")')"
jq -e 'any(.next_steps[]; . == "host problems alert nobody yet: run setup again with --cluster-alerts FILE")' \
  "$private/cluster-setup.json" >/dev/null || die "setup did not report that delivery is disabled"
[ "$E2E_FAILS" -eq 0 ] || finish

public_ports_detail() {
  cli "$1" checks --json | jq -er '.[] | select(.name == "public_ports") | .detail | select(length > 0)'
}
for project in "$PROJECT" "$peer"; do
  wait_for "initial public_ports for $project/$ENVN" public_ports_detail "$project" || exit 1
done

marker_name="$PROJECT-$ENVN-public-port"
inside docker run -d --name "$marker_name" --label "berthy.fakeserver.owner=$FS_OWNER" \
  -p '0.0.0.0::80' --entrypoint sleep "$REG/e2e/web@$WEB_V1_DIGEST" infinity > "$private/marker-id"
port=$(inside docker inspect --format '{{(index (index .NetworkSettings.Ports "80/tcp") 0).HostPort}}' "$(cat "$private/marker-id")")
[[ $port =~ ^[0-9]+$ ]] || die "could not identify the marker port"
marker="$port on 0.0.0.0 (published by a container)"
echo "  marker port: $port"

inside cat /etc/berthy/agent.json > "$private/agent.json"
jq --arg url "http://$TG_LISTEN" '.telegram_api_url = $url' "$private/agent.json" |
  inside sh -ec 'cd /etc/berthy; cp -p agent.json agent.json.new; cat > agent.json.new; mv agent.json.new agent.json'
inside systemctl restart berthy
# Both monitors must see the marker on this start, while delivery is still disabled.
marker_checked() { public_ports_detail "$1" | grep -F -- "$marker" >/dev/null; }
for project in "$PROJECT" "$peer"; do
  wait_for "public_ports sees the marker for $project/$ENVN" marker_checked "$project" || exit 1
done

enable_environment_alerts() {
  local project=$1 chat=$2 hook=$3 dir="$private/$1"
  mkdir "$dir"
  cli "$project" secret set TELEGRAM_BOT_TOKEN < "$RUN/tg-token" >/dev/null
  printf 'http://%s%s' "$HOOK_LISTEN" "$hook" | cli "$project" secret set ALERT_WEBHOOK_URL >/dev/null
  PROJECT="$project" base_manifest > "$dir/berthy.yml"
  cat >> "$dir/berthy.yml" <<YML
network: $project-$ENVN
probes:
  scope-$chat:
    tcp: $TG_LISTEN
alerts:
  reminder: 0s
  telegram:
    bot_token_secret: TELEGRAM_BOT_TOKEN
    chat_id: "$chat"
  webhook:
    url_secret: ALERT_WEBHOOK_URL
YML
  cli "$project" apply --manifest-dir "$dir" >/dev/null
  local out
  out=$(cli "$project" alerts test)
  expect_contains "$project/$ENVN test alert reaches Telegram" "$out" 'telegram: ok'
  expect_contains "$project/$ENVN test alert reaches the webhook" "$out" 'webhook: ok'
}
enable_environment_alerts "$PROJECT" "$first_chat" "$first_hook"
enable_environment_alerts "$peer" "$second_chat" "$second_hook"
[ "$E2E_FAILS" -eq 0 ] || finish
inside systemctl restart berthy

has_own_probe() {
  cli "$1" checks --json | jq -e --arg own "probe:scope-$2" --arg other "probe:scope-$3" '
    any(.[]; .name == $own and .state == "ok") and all(.[]; .name != $other and (has("server") | not))' >/dev/null
}
for pair in "$PROJECT:$first_chat:$second_chat" "$peer:$second_chat:$first_chat"; do
  IFS=: read -r project own other <<< "$pair"
  wait_for "ordinary checks for $project/$ENVN" has_own_probe "$project" "$own" "$other" || exit 1
  pass "$project/$ENVN ordinary checks show its own probe"
done

telegram_warnings() {
  tg_messages | jq -e --arg chat "$1" --arg marker "$marker" '
    [.[] | select(.chat_id == $chat and (.text | startswith("🟠 ") and contains(" public_ports: ") and contains($marker)))] | length'
}
webhook_warnings() {
  hook_requests | jq -e --arg path "$1" --arg marker "$marker" '
    [.[] | select(.method == "POST" and .path == $path) | .body | fromjson |
      select(.kind == "check" and .name == "public_ports" and .state == "warning" and (.detail | contains($marker)))] | length'
}
marker_delivered() {
  [ "$(telegram_warnings "$server_chat")" -ge 1 ] && [ "$(webhook_warnings "$server_hook")" -ge 1 ] || {
    [ "$(telegram_warnings "$first_chat")" -ge 1 ] && [ "$(webhook_warnings "$first_hook")" -ge 1 ] &&
      [ "$(telegram_warnings "$second_chat")" -ge 1 ] && [ "$(webhook_warnings "$second_hook")" -ge 1 ]
  }
}
if ! wait_for "public_ports warning delivery" marker_delivered; then
  fail "public_ports warning delivery" "the receivers did not record the marker warning on both channels"
fi

expect_eq "$PROJECT/$ENVN Telegram host warnings" 0 "$(telegram_warnings "$first_chat")"
expect_eq "$PROJECT/$ENVN webhook host warnings" 0 "$(webhook_warnings "$first_hook")"
expect_eq "$peer/$ENVN Telegram host warnings" 0 "$(telegram_warnings "$second_chat")"
expect_eq "$peer/$ENVN webhook host warnings" 0 "$(webhook_warnings "$second_hook")"

if cleanup; then
  started=0
else
  fail "remove the alert server fixture" "fakeserver cleanup failed"
fi
finish
