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

finish
