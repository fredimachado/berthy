#!/bin/bash
# 53-hosted-start-failure checks that a wrong state key takes down only its own environment, and that doctor and webhook
# alerts report the failure and clear it after repair.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"

SECOND=$PROJECT-two
key=$RUN/key-$SECOND-$ENVN
check_name=hosted:$SECOND/$ENVN
reason='the key does not open this state'

hosted_post() {
  hook_requests | jq -ce --arg env "$ENVN" --arg name "$check_name" --arg state "$1" '
    first(.[] | select(.method == "POST") | .body | fromjson |
      select(.kind == "check" and .environment == $env and .name == $name and .state == $state))'
}

# The receiver records the POST before the monitor commits delivery. Wait for both before stopping the agent.
critical_delivered() {
  hosted_post critical >/dev/null || return 1
  python3 - "$RUN/state/state.db" "$ENVN" "$check_name" <<'PY'
import sqlite3, sys

path, environment, name = sys.argv[1:]
with sqlite3.connect("file:" + path + "?mode=ro", uri=True) as db:
    row = db.execute("SELECT notified_state FROM checks WHERE environment=? AND name=?", (environment, name)).fetchone()
sys.exit(row != ("critical",))
PY
}

add_environment "$SECOND" "$ENVN" || { fail "initialize $SECOND/$ENVN" "could not add environment"; finish; }
token_var=$(env_var "$SECOND" "$ENVN")

out=$(printf 'http://%s/hosted-start-failure' "$HOOK_LISTEN" |
  env_cmd "$PROJECT" "$ENVN" secret set ALERT_WEBHOOK_URL 2>&1); code=$?
expect_code "configure the healthy pair's webhook secret" 0 "$code" "$out"
[ "$code" -eq 0 ] || finish
{
  base_manifest
  cat <<'YML'
alerts:
  webhook:
    url_secret: ALERT_WEBHOOK_URL
jobs:
  smoke:
    app: web
    command: ["true"]
YML
} | write_manifest || exit 1
out=$(env_cmd "$PROJECT" "$ENVN" deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy the healthy pair's volume-free app" 0 "$code" "$out"
[ "$code" -eq 0 ] || finish

stop_agent
saved_key=$(mktemp "$RUN/hosted-start-key.XXXXXX") || exit 1
cp "$key" "$saved_key" || { rm -f "$saved_key"; exit 1; }
trap '[ ! -f "$saved_key" ] || mv -f -- "$saved_key" "$key"' EXIT
head -c 32 /dev/urandom > "$key" || exit 1
if cmp -s "$saved_key" "$key"; then
  fail "substitute the second pair's key" "replacement matches the original"
  finish
fi

if start_agent; then
  pass "agent starts with a wrong key for $SECOND/$ENVN"
else
  fail "agent starts with a wrong key for $SECOND/$ENVN" "agent did not answer"
  detail_block "$(tail -n 10 "$LOG/agent.log")"
fi
out=$(env_cmd "$PROJECT" "$ENVN" agent-info --json 2>&1); code=$?
expect_code "healthy pair $PROJECT/$ENVN answers" 0 "$code" "$out"
out=$(env_cmd "$PROJECT" "$ENVN" ps --json 2>&1); code=$?
expect_code "healthy pair lists its apps" 0 "$code" "$out"
if jq -e 'length == 1 and .[0].ref == {"kind":"image","name":"web"} and .[0].roles[0].running == 1' <<< "$out" >/dev/null; then
  pass "healthy pair lists its own running image app"
else
  fail "healthy pair lists its own running image app" "unexpected app inventory"
  detail_block "$out"
fi
out=$(env_cmd "$PROJECT" "$ENVN" job run smoke 2>&1); code=$?
expect_code "healthy pair runs its volume-free job" 0 "$code" "$out"
expect_contains "the job reached terminal success" "$out" "status: succeeded"

expect_eq "unauthenticated health returns the exact body and HTTP 200" '{"ok":true} 200' \
  "$(curl_local -w ' %{http_code}' "http://$AGENT_LISTEN/healthz")"

body=$RUN/hosted-start-failure.body
: > "$body" || exit 1
status=$(curl_local -o "$body" -w '%{http_code}' -H "Authorization: Bearer ${!token_var}" \
  "http://$AGENT_LISTEN/v1/projects/$SECOND/environments/$ENVN/agent")
expect_eq "failed pair $SECOND/$ENVN returns authenticated HTTP 503" 503 "$status"
expect_eq "failed pair returns the environment_failed code" environment_failed "$(jq -r '.code' "$body")"

doc=$(env_cmd "$PROJECT" "$ENVN" doctor --json 2>&1); code=$?
expect_code "doctor succeeds for the healthy pair" 0 "$code" "$doc"
expect_eq "doctor gives the failed peer's noncritical reason" "false false $reason" \
  "$(jq -r --arg name "$check_name" '.checks[] | select(.name == $name) | "\(.ok) \(.critical) \(.detail)"' <<< "$doc")"
if wait_for "critical startup webhook" critical_delivered; then
  alert=$(hosted_post critical)
  expect_eq "webhook gives the same startup failure reason" "$reason" "$(jq -r '.detail' <<< "$alert")"
else
  fail "critical startup webhook" "delivery was not recorded for $check_name"
fi

stop_agent
mv -f -- "$saved_key" "$key" || exit 1
start_agent || { fail "agent starts with the restored key" "agent did not answer"; finish; }
for project in "$PROJECT" "$SECOND"; do
  out=$(env_cmd "$project" "$ENVN" agent-info --json 2>&1); code=$?
  expect_code "pair $project/$ENVN answers after key restoration" 0 "$code" "$out"
done
doc=$(env_cmd "$PROJECT" "$ENVN" doctor --json 2>&1); code=$?
expect_code "doctor succeeds after key restoration" 0 "$code" "$doc"
expect_eq "doctor clears the hosted startup finding" 0 \
  "$(jq '[.checks[] | select(.name | startswith("hosted:"))] | length' <<< "$doc")"
if wait_for "startup recovery webhook" hosted_post ok; then
  alert=$(hosted_post ok)
  expect_contains "webhook reports recovery for the same check" "$(jq -r '.text' <<< "$alert")" "$check_name recovered"
else
  fail "startup recovery webhook" "missing recovery for $check_name"
fi

# Run setup against a disposable server with its own SSH home.
private=$(mktemp -d "$RUN/hosted-setup.XXXXXX") || exit 1
export FS_HOME="$private/home" FS_DOCKER_HOST="unix://$SOCK" DOCKER_CONFIG="$E2E_ROOT/docker-config"
export HOME="$FS_HOME"
mkdir -p "$FS_HOME/bin" "$private/empty" || exit 1
ln -s "$DOCKER_BIN" "$FS_HOME/bin/docker" || exit 1
export PATH="$FS_HOME/bin:$PATH"
fixture="$REPO/test/fakeserver/fakeserver.sh"
scope=$(printf '%s' "$PROJECT/$ENVN" | sha256sum | cut -c1-8)
nonce=$(printf '%s' "$private" | sha256sum | cut -c1-16)
export FS_OWNER="$nonce"
server="h$scope-$nonce"
existing=$(d ps -aq --no-trunc --filter "name=^/${server}\$") || exit 1
[ -z "$existing" ] || { fail "start disposable setup server" "container name already exists"; finish; }
volumes=$(d volume ls -q) || exit 1
for volume in "$server-docker" "$server-containerd"; do
  if grep -Fxq -- "$volume" <<< "$volumes"; then fail "start disposable setup server" "volume name already exists"; finish; fi
done
cleanup_setup() {
  bash "$fixture" rm "$server" >/dev/null 2>&1 || echo "could not remove setup fixture" >&2
  rm -rf "$private"
}
trap cleanup_setup EXIT
bash "$fixture" start "$server" || { fail "start disposable setup server" "fixture did not start"; finish; }
if ! bash "$fixture" install-docker "$server" > "$LOG/install-docker.log" 2>&1; then
  fail "install Docker on the setup fixture" "see $LOG/install-docker.log"
  finish
fi
remote() { bash "$fixture" exec "$server" "$@"; }
ip=$(bash "$fixture" ip "$server") || exit 1

cfg="$private/cli.json"
setup_cmd=("$BIN" setup -e "$ENVN" --host "ops@$server" --listen "$ip:7710" --binary "$BIN"
  --manifest-dir "$private/empty" --config "$cfg" --no-install-docker --json)
setup_report() {
  local token_pattern='"token":[[:space:]]*"(bty\.[a-z0-9._-]+)"'
  setup_token=
  if [[ $out =~ $token_pattern ]]; then
    setup_token=${BASH_REMATCH[1]}
    out=${out//"$setup_token"/[redacted]}
  fi
}
out=$("${setup_cmd[@]}" --project "$PROJECT" </dev/null); code=$?
setup_report
expect_code "install the first hosted pair through setup" 0 "$code" "$out"
[ "$code" -eq 0 ] && [ -n "$setup_token" ] || { fail "read first setup credential" "setup did not return a credential"; finish; }
first_token_env=$(jq -er --arg p "$PROJECT" --arg e "$ENVN" '.projects[$p].environments[$e].token_env' "$cfg") || exit 1
declare -n first_credential="$first_token_env"
first_credential=$setup_token
export "$first_token_env"
unset -n first_credential
unset setup_token
remote cat /etc/berthy/agent.json > "$private/previous.json" || exit 1

pair=/var/lib/berthy/environments/$SECOND/$ENVN
remote sh -ec '
  install -d -o berthy -g berthy -m 0700 "$1" "$1/state"
  install -o berthy -g berthy -m 0600 /dev/null "$1/key"
  head -c 32 /dev/urandom > "$1/key"
  install -o berthy -g berthy -m 0600 /dev/null "$1/state/state.db"
  printf "invalid SQLite database\n" > "$1/state/state.db"
' sh "$pair" || exit 1
out=$("${setup_cmd[@]}" --project "$SECOND" </dev/null); code=$?
setup_report
expect_code "setup rejects the addition with unreadable state" 1 "$code" "$out"
service=$(jq -r '.steps[] | select(.step == "service" and .status == "failed") | .detail' <<< "$out")
expect_contains "service reports the added pair and startup reason" "$service" "$SECOND/$ENVN: cannot open state"
if [ -z "$setup_token" ]; then
  pass "setup does not report a live token for the rolled-back addition"
else
  fail "setup does not report a live token for the rolled-back addition" "failed setup returned a token"
fi
unset setup_token
if cmp -s "$private/previous.json" <(remote cat /etc/berthy/agent.json); then
  pass "setup restores the previous server configuration byte for byte"
else
  fail "setup restores the previous server configuration byte for byte" "configuration differs"
fi
if jq -e --arg p "$SECOND" --arg e "$ENVN" '.projects[$p].environments[$e] == null' "$cfg" >/dev/null; then
  pass "the rolled-back pair is absent from the CLI configuration"
else
  fail "the rolled-back pair is absent from the CLI configuration" "context exists"
fi
out=$("$BIN" agent-info -e "$ENVN" --project "$PROJECT" --config "$cfg" 2>&1); code=$?
expect_code "the first pair's credential still works after rollback" 0 "$code" "$out"

startup_dir=/run/berthy-startup
group=$(remote id -g berthy) || exit 1
expect_eq "startup directory ownership and mode" "0:$group:750" "$(remote stat -c '%u:%g:%a' "$startup_dir")"
expect_eq "startup result ownership and mode" "0:$group:620" "$(remote stat -c '%u:%g:%a' "$startup_dir/result")"
invocation=$(remote systemctl show --property=InvocationID --value berthy) || exit 1
snapshot=$(remote cat "$startup_dir/result") || exit 1
expect_eq "restored service writes its invocation and started pair" \
  "$(printf 'berthy-startup-v1\t%s\t1\n%s/%s\tstarted\t\ncomplete' "$invocation" "$PROJECT" "$ENVN")" "$snapshot"

remote rm -- "$pair/state/state.db" || exit 1
out=$("${setup_cmd[@]}" --project "$SECOND" </dev/null); code=$?
setup_report
unset setup_token "$first_token_env"
expect_code "setup confirms the repaired addition" 0 "$code" "$out"
if jq -e --arg p "$SECOND" --arg e "$ENVN" '.projects[$p].environments[$e].url | type == "string"' "$cfg" >/dev/null; then
  pass "the repaired pair is present in the CLI configuration"
else
  fail "the repaired pair is present in the CLI configuration" "context missing"
fi

finish
