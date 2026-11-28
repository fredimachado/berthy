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
    </dev/null > "$private/setup-$project.json" 2> "$private/setup-$project.stderr"; then
    jq -r '.steps[]? | select(.status == "failed") | .step' "$private/setup-$project.json" >&2
    die "could not set up $project/$ENVN"
  fi
  token=$(jq -er '.token | strings' "$private/setup-$project.json")
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
    check_credentials_hidden "$private/cluster-setup.json" "$private/cluster-setup.stderr"
    jq -r '.steps[]? | select(.status == "failed") | .step' "$private/cluster-setup.json" >&2
    die "$what failed"
  fi
  check_credentials_hidden "$private/cluster-setup.json" "$private/cluster-setup.stderr"
  check_both
  if [ "$what" != 'omit destination flags' ]; then
    [ "$(invocation)" != "$before" ] || die "$what did not start a new agent invocation"
  fi
  [ "$(cluster_identity)" = "$cluster_before" ] || die "$what changed the cluster database or key"
  pass "$what keeps the cluster database and key"
  pass "$what leaves both environments serving"
}
{
  tr -d '\r\n' < "$RUN/tg-token"; printf '\n'
  printf 'http://%s%s\n' "$HOOK_LISTEN" "$server_hook" "$HOOK_LISTEN" "$first_hook" "$HOOK_LISTEN" "$second_hook"
} > "$private/credential-patterns"
check_credentials_hidden() {
  if grep -Fq -f "$private/credential-patterns" -- "$@"; then
    die "setup output or agent logs exposed a fixture credential"
  fi
}

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
inside sh -ec 'umask 077; cat > /root/fixture-cluster; chmod 700 /root/fixture-cluster' < "$CLUSTER_BIN"
inside systemctl restart berthy
wait_for "agent API after installing the Telegram endpoint" cli "$PROJECT" agent-info --json
hostname=$(cli "$PROJECT" agent-info --json | jq -er '.hostname | select(length > 0)')
marker_checked() {
  cli "$1" checks --cluster --json | jq -e --arg marker "$marker" --arg host "$hostname" '
    any(.[]; .name == "public_ports" and .server == $host and .state == "warning" and (.detail | contains($marker)))'
}
for project in "$PROJECT" "$peer"; do
  wait_for "public_ports sees the marker for $project/$ENVN" marker_checked "$project"
  expect_eq "$project/$ENVN agent-info names the server" "$hostname" "$(cli "$project" agent-info --json | jq -er '.hostname')"
  cli "$project" doctor --json | jq -e --arg host "$hostname" --arg marker "$marker" '
    any(.checks[]; .name == "host" and .detail == $host) and
    any(.checks[]; .name == "public_ports" and (.critical | not) and (.detail | contains($marker)))' >/dev/null
  pass "$project/$ENVN live doctor and stored cluster checks identify the same server and marker"
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
YML
  if [ "$project" = "$PROJECT" ]; then
    cat >> "$dir/berthy.yml" <<'YML'
  failed:
    tcp: 127.0.0.1:1
    failures: 1
YML
  fi
  cat >> "$dir/berthy.yml" <<YML
alerts:
  reminder: 0s
  telegram:
    bot_token_secret: TELEGRAM_BOT_TOKEN
    chat_id: "$chat"
  webhook:
    url_secret: ALERT_WEBHOOK_URL
YML
  cli "$project" apply --manifest-dir "$dir" >/dev/null
}
enable_environment_alerts "$PROJECT" "$first_chat" "$first_hook"
enable_environment_alerts "$peer" "$second_chat" "$second_hook"
[ "$E2E_FAILS" -eq 0 ] || finish
cluster_setup 'preload the Telegram credential' --cluster-secret SERVER_TG < "$RUN/tg-token"
printf 'http://%s%s-preload\r\n' "$HOOK_LISTEN" "$server_hook" |
  cluster_setup 'preload the webhook credential' --cluster-secret SERVER_HOOK
printf 'http://%s%s\r\n' "$HOOK_LISTEN" "$server_hook" |
  cluster_setup 'replace the webhook credential' --cluster-secret SERVER_HOOK
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
cluster_setup 'omit destination flags' --config "$cfg" </dev/null
expect_eq "omitting destination flags preserves the channels" "$destinations" "$(cluster_layout | jq -ec '.alerts')"

has_own_probe() {
  cli "$1" checks --json | jq -e --arg own "probe:scope-$2" --arg other "probe:scope-$3" '
    any(.[]; .name == $own and .state == "ok") and
    all(.[]; .name != $other and (has("server") | not) and
      (.name | test("^(disk:|memory$|docker_disk$|public_ports$|reboot_required$)") | not))' >/dev/null
}
for pair in "$PROJECT:$first_chat:$second_chat" "$peer:$second_chat:$first_chat"; do
  IFS=: read -r project own other <<< "$pair"
  wait_for "ordinary checks for $project/$ENVN" has_own_probe "$project" "$own" "$other" || exit 1
  pass "$project/$ENVN ordinary checks show its own probe"
done

telegram_count() {
  tg_messages | jq -e --arg chat "$1" --arg text "$2" --arg detail "$3" '
    [.[] | select(.chat_id == $chat and (.text | contains($text) and contains($detail)))] | length'
}
webhook_count() {
  hook_requests | jq -e --arg path "$1" --arg name "$2" --arg state "$3" --arg detail "$4" \
    --arg host "$5" --arg env "$ENVN" '
    [.[] | select(.method == "POST" and .path == $path) | .body | fromjson |
      select(.name == $name and .state == $state and (.detail | contains($detail))) |
      select(if $name == "test" then .kind == "test" else .kind == "check" end) |
      select(if $host == "" then .environment == $env and (has("server") | not)
        else .server == $host and .environment == "" end)] | length'
}
notified() { inside /root/fixture-cluster notified "$cluster_dir/state/state.db" "$1" "$2" </dev/null; }
server_delivered() {
  local name=$1 state=$2 text=$3 detail=$4
  [ "$(telegram_count "$server_chat" "server $hostname $name$text" "$detail")" -ge 1 ] &&
    [ "$(webhook_count "$server_hook" "$name" "$state" "$detail" "$hostname")" -ge 1 ] &&
    notified "$name" "$state"
}
expect_server_delivery() {
  local name=$1 state=$2 text=$3 detail=$4
  expect_eq "server Telegram $name $state" 1 "$(telegram_count "$server_chat" "server $hostname $name$text" "$detail")"
  expect_eq "server webhook $name $state" 1 "$(webhook_count "$server_hook" "$name" "$state" "$detail" "$hostname")"
}

wait_for "public_ports marker delivery and checkpoint" server_delivered public_ports warning ': ' "$marker"
expect_server_delivery public_ports warning ': ' "$marker"
for pair in "$first_chat:$first_hook" "$second_chat:$second_hook"; do
  IFS=: read -r chat hook <<< "$pair"
  expect_eq "environment Telegram $chat has no host marker warning" 0 "$(telegram_count "$chat" ' public_ports: ' "$marker")"
  expect_eq "environment webhook $hook has no host marker warning" 0 \
    "$(hook_requests | jq -e --arg path "$hook" --arg marker "$marker" '
      [.[] | select(.method == "POST" and .path == $path) | .body | fromjson |
        select(.name == "public_ports" and .state == "warning" and (.detail | contains($marker)))] | length')"
done
test_alert() {
  local project=$1 out
  out=$(cli "$project" alerts test)
  expect_contains "$project/$ENVN test alert reaches Telegram" "$out" 'telegram: ok'
  expect_contains "$project/$ENVN test alert reaches the webhook" "$out" 'webhook: ok'
}
expect_environment_tests() {
  local chat=$1 hook=$2 count=$3
  expect_eq "environment Telegram $chat has $count test alerts" "$count" "$(telegram_count "$chat" "$ENVN test alert from berthy" '')"
  expect_eq "environment webhook $hook has $count test alerts" "$count" "$(webhook_count "$hook" test ok '' '')"
}
test_alert "$PROJECT"
expect_environment_tests "$first_chat" "$first_hook" 1
expect_environment_tests "$second_chat" "$second_hook" 0
test_alert "$peer"
expect_environment_tests "$first_chat" "$first_hook" 1
expect_environment_tests "$second_chat" "$second_hook" 1
expect_eq "server Telegram receives no environment test alerts" 0 "$(telegram_count "$server_chat" 'test alert from berthy' '')"
expect_eq "server webhook receives no environment test alerts" 0 \
  "$(hook_requests | jq -e --arg path "$server_hook" '[.[] | select(.method == "POST" and .path == $path) | .body | fromjson | select(.kind == "test")] | length')"

failed_probe_delivered() {
  [ "$(telegram_count "$first_chat" "$ENVN probe:failed: " '')" -ge 1 ] &&
    [ "$(webhook_count "$first_hook" probe:failed critical '' '')" -ge 1 ]
}
wait_for "first environment's failing probe reaches both destinations" failed_probe_delivered
expect_eq "first environment receives one failed probe on Telegram" 1 "$(telegram_count "$first_chat" "$ENVN probe:failed: " '')"
expect_eq "first environment receives one failed probe on its webhook" 1 "$(webhook_count "$first_hook" probe:failed critical '' '')"
for chat in "$second_chat" "$server_chat"; do
  expect_eq "Telegram $chat receives no failed probe" 0 "$(telegram_count "$chat" ' probe:failed: ' '')"
done
for hook in "$second_hook" "$server_hook"; do
  expect_eq "webhook $hook receives no failed probe" 0 \
    "$(hook_requests | jq -e --arg path "$hook" '[.[] | select(.method == "POST" and .path == $path) | .body | fromjson | select(.name == "probe:failed")] | length')"
done

reboot_marker() {
  inside sh -ec 'mkdir -p /usr/share/update-notifier; touch /var/run/reboot-required; printf "%s\n" "$1" > /var/run/reboot-required.pkgs' sh "$1" </dev/null
}
reboot_detail() {
  cli "$1" checks --cluster --json |
    jq -e --arg host "$hostname" --arg package "$2" 'any(.[]; .server == $host and .name == "reboot_required" and
      .state == "warning" and (.detail | contains($package)))'
}

inside systemctl stop berthy
reboot_marker "$PROJECT-before"
inside systemctl start berthy
wait_for "reboot warning delivery and checkpoint" server_delivered reboot_required warning ': ' "$PROJECT-before"

inside systemctl stop berthy
reboot_marker "$PROJECT-after"
inside systemctl start berthy
for project in "$PROJECT" "$peer"; do
  wait_for "fresh reboot detail for $project/$ENVN" reboot_detail "$project" "$PROJECT-after"
  pass "$project/$ENVN sees the changed reboot detail after restart"
done
notified reboot_required warning
expect_server_delivery reboot_required warning ': ' ''

inside systemctl stop berthy
inside rm -f /var/run/reboot-required /var/run/reboot-required.pkgs
inside systemctl start berthy
wait_for "reboot recovery delivery and checkpoint" server_delivered reboot_required ok ' recovered' ''
expect_server_delivery reboot_required warning ': ' ''
expect_server_delivery reboot_required ok ' recovered' ''
expect_server_delivery public_ports warning ': ' "$marker"
for chat in "$first_chat" "$second_chat"; do
  expect_eq "environment Telegram $chat receives no reboot alert" 0 "$(telegram_count "$chat" ' reboot_required' '')"
done
expect_eq "environment webhooks receive no reboot alert" 0 \
  "$(hook_requests | jq -e --arg first "$first_hook" --arg second "$second_hook" '
    [.[] | select(.method == "POST" and (.path == $first or .path == $second)) | .body | fromjson |
      select(.name == "reboot_required")] | length')"

cluster_setup 'clear server destinations' --config "$cfg" --cluster-alerts none </dev/null
expect_eq "none clears the destinations" false "$(cluster_layout | jq 'has("alerts")')"
jq -e 'any(.next_steps[]; . == "host problems alert nobody yet: run setup again with --cluster-alerts FILE")' \
  "$private/cluster-setup.json" >/dev/null || die "setup did not report that delivery is disabled"
doctor_cluster '.checks | any(.name == "cluster_alerts" and (.ok | not) and (.critical | not) and (.detail | contains("--cluster-alerts FILE")))'
inside journalctl -u berthy --no-pager > "$private/agent.log" 2> "$private/agent-log.stderr"
check_credentials_hidden "$private"/setup-*.json "$private"/setup-*.stderr "$private/agent.log" "$private/agent-log.stderr"
pass "setup output and agent logs keep fixture credentials private"

if cleanup; then
  started=0
else
  fail "remove the alert server fixture" "fakeserver cleanup failed"
fi
finish
