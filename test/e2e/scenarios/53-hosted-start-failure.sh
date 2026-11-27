#!/bin/bash
# 53-hosted-start-failure checks that a wrong state key takes down only its own hosted environment.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"

SECOND=$PROJECT-two
key=$RUN/key-$SECOND-$ENVN

add_environment "$SECOND" "$ENVN" || { fail "initialize $SECOND/$ENVN" "could not add environment"; finish; }
token_var=$(env_var "$SECOND" "$ENVN")

{
  base_manifest
  cat <<'YML'
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

stop_agent
mv -f -- "$saved_key" "$key" || exit 1
start_agent || { fail "agent starts with the restored key" "agent did not answer"; finish; }
for project in "$PROJECT" "$SECOND"; do
  out=$(env_cmd "$project" "$ENVN" agent-info --json 2>&1); code=$?
  expect_code "pair $project/$ENVN answers after key restoration" 0 "$code" "$out"
done

finish
