#!/bin/bash
# 36-hosted-environments proves that one agent hosts several (project, environment) pairs under the project URL. The checks,
# in order:
#   1. the agent says its project, and a deploy to the only environment works;
#   2. an unknown project is 401 like a bad token, and the URL of an earlier release says to upgrade the CLI;
#   3. with two more hosted environments (the same environment name in another project, and another environment of the first
#      project) each admin token answers only on its own URL, and a command that does not say which project is refused;
#   4. the restarts lost nothing: the release of step 1 is in the history;
#   5. an agent.json of an earlier release is refused with a message that says so;
#   6. a state directory twice stops the agent. A claimed state given to another pair fails that pair with HTTP 503,
#      while the healthy pair still answers;
#   7. a hosted environment without a key file stops the agent.
# Then the fixture's own agent.json comes back, and the agent starts and serves the release of step 1.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"
# adm needs no manifest here, and a berthy.yml in the current directory would name a project.
cd "$E2E_ROOT" || exit 1

SECOND=$PROJECT-two # the same environment name in another project
THIRD=${ENVN}b      # another environment of the first project

# status URL TOKEN: the HTTP status of a GET with that bearer token.
status() { curl_local -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $2" "$1"; }

# api PROJECT ENV: the URL of a hosted environment's API.
api() { printf 'http://%s/v1/projects/%s/environments/%s/' "$AGENT_LISTEN" "$1" "$2"; }

# entry_of PROJECT ENV: the token entry of that environment's fragment of agent.json.
entry_of() { sed -n 's/.*"tokens":\[\(.*\)\]}$/\1/p' "$RUN/environments.d/$1-$2.json"; }

# refused NAME WANT_TEXT: starts the agent on the agent.json that is written now and expects it to exit at once, non-zero, with
# WANT_TEXT in its log. The agent must not start, so it is stopped after 30 seconds if it does.
refused() {
  local code out
  : > "$RUN/refused.log"
  timeout 30 "$BIN" agent --config "$RUN/agent.json" > "$RUN/refused.log" 2>&1; code=$?
  cat "$RUN/refused.log" >> "$LOG/agent.log"
  out=$(head -n 5 "$RUN/refused.log")
  if [ "$code" -ne 0 ] && [ "$code" -ne 124 ]; then pass "$1: the agent exits non-zero"; else fail "$1: the agent exits non-zero" "exit $code"; fi
  expect_contains "$1: the log says why" "$out" "$2"
}

# 1. The agent names its project, and the only environment deploys.
out=$(adm agent-info --json 2>&1); code=$?
expect_code "agent-info" 0 "$code" "$out"
expect_contains "agent-info has a project" "$out" "\"project\""
expect_contains "agent-info names the project" "$out" "$PROJECT"
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1" 0 "$code" "$out"

# 2. An unknown project is 401 like a bad token. The URL of an earlier release is 404 and says what to do.
expect_eq "an unknown project is 401" 401 "$(status "$(api nope "$ENVN")agent" "$BERTHY_ADMIN_TOKEN")"
expect_eq "a bad token is 401" 401 "$(status "$(api "$PROJECT" "$ENVN")agent" "bty.$PROJECT.$ENVN.$(printf '0%.0s' $(seq 1 64))")"
expect_eq "the right token is 200" 200 "$(status "$(api "$PROJECT" "$ENVN")agent" "$BERTHY_ADMIN_TOKEN")"
out=$(curl_local "http://$AGENT_LISTEN/v1/environments/$ENVN/agent" -H "Authorization: Bearer $BERTHY_ADMIN_TOKEN" -w ' %{http_code}')
expect_contains "the old URL is 404" "$out" " 404"
expect_contains "the old URL says to upgrade the CLI" "$out" "upgrade the CLI"

# 3. Two more hosted environments. Each restart returns as soon as /healthz answers.
add_environment "$SECOND" "$ENVN"; code=$?
expect_code "add $SECOND/$ENVN" 0 "$code"
add_environment "$PROJECT" "$THIRD"; code=$?
expect_code "add $PROJECT/$THIRD" 0 "$code"
ids=("$PROJECT $ENVN" "$SECOND $ENVN" "$PROJECT $THIRD")
v2=$(env_var "$SECOND" "$ENVN")
v3=$(env_var "$PROJECT" "$THIRD")
tokens=("$BERTHY_ADMIN_TOKEN" "${!v2}" "${!v3}")
for i in 0 1 2; do
  for j in 0 1 2; do
    # shellcheck disable=SC2086
    url=$(api ${ids[$j]})agent
    want=401
    [ "$i" = "$j" ] && want=200
    expect_eq "the admin token of ${ids[$i]// //} on ${ids[$j]// //}: $want" "$want" "$(status "$url" "${tokens[$i]}")"
  done
done
out=$(adm agent-info 2>&1); code=$?
expect_code "agent-info without a project is ambiguous" 1 "$code" "$out"
expect_contains "the refusal says the environment is in several projects" "$out" "exists in several projects"

# 4. The restarts lost nothing.
out=$(env_cmd "$PROJECT" "$ENVN" history web 2>&1); code=$?
expect_code "history of $PROJECT/$ENVN" 0 "$code" "$out"
expect_contains "the release of step 1 is still there" "$out" "$WEB_V1_DIGEST"
out=$(env_cmd "$SECOND" "$ENVN" history web 2>&1)
expect_not_contains "another project's history does not hold it" "$out" "$WEB_V1_DIGEST"

# The agent is stopped for the refusals below. The fragments are kept to put the fixture back.
stop_agent
rm -rf "$RUN/environments.d.orig" && cp -a "$RUN/environments.d" "$RUN/environments.d.orig" || exit 1
restore() {
  rm -rf "$RUN/environments.d" && cp -a "$RUN/environments.d.orig" "$RUN/environments.d" && write_agent_config
}

# 5. An agent.json of an earlier release, with a top-level environment, is refused.
printf '{"environment":"%s","listen":"%s","docker_socket":"%s","state_dir":"%s","key_file":"%s","tokens":[%s]}\n' \
  "$ENVN" "$AGENT_LISTEN" "$SOCK" "$RUN/state" "$RUN/state.key" "$(entry_of "$PROJECT" "$ENVN")" > "$RUN/agent.json" && chmod 600 "$RUN/agent.json"
refused "an earlier layout" "layout of an earlier Berthy release"
restore || exit 1

# 6. A state directory used twice. Then a claimed state, copied to a new path with a copy of its key, is given to another
#    project and to another environment of its own project.
write_fragment "$PROJECT" "$THIRD" "$RUN/state" "$RUN/key-$PROJECT-$THIRD" "$(entry_of "$PROJECT" "$THIRD")" && write_agent_config || exit 1
refused "a state directory twice" "duplicate state_dir"
restore || exit 1

cp -a "$RUN/state-$SECOND-$ENVN" "$RUN/state-copy" && cp "$RUN/key-$SECOND-$ENVN" "$RUN/key-copy" || exit 1
misassigned() {
  local project=$1 env=$2 credentials token entry reply out code
  credentials=$("$BIN" agent token --project "$project" -e "$env" --name admin --scope admin) || return 1
  token=$(sed -n 's/^token: //p' <<< "$credentials")
  entry=$(sed -n '3p' <<< "$credentials")
  write_fragment "$project" "$env" "$RUN/state-copy" "$RUN/key-copy" "$entry" && write_agent_config || return 1
  start_agent || return 1
  out=$(env_cmd "$PROJECT" "$ENVN" agent-info --json 2>&1); code=$?
  expect_code "healthy pair answers with $project/$env misassigned" 0 "$code" "$out"
  reply=$(curl_local -w '\n%{http_code}' -H "Authorization: Bearer $token" "$(api "$project" "$env")agent")
  expect_eq "$project/$env returns HTTP 503" 503 "${reply##*$'\n'}"
  expect_eq "$project/$env returns its startup failure" "environment_failed: $project/$env: state belongs to another environment" \
    "$(jq -r '.code + ": " + .message' <<< "${reply%$'\n'*}")"
  stop_agent
}
misassigned "$PROJECT-three" "$ENVN" || exit 1
restore || exit 1
misassigned "$SECOND" "${ENVN}c" || exit 1
restore || exit 1

# 7. A hosted environment without a key file.
write_fragment "$PROJECT" "$THIRD" "$RUN/state-$PROJECT-$THIRD" "" "$(entry_of "$PROJECT" "$THIRD")" && write_agent_config || exit 1
refused "no key file" "needs state_dir and key_file"
restore || exit 1

# The fixture's own file comes back, and what the refusals left alone is still there.
start_agent || exit 1
out=$(env_cmd "$PROJECT" "$ENVN" history web 2>&1); code=$?
expect_code "history after the refusals" 0 "$code" "$out"
expect_contains "the release of step 1 survived the refusals" "$out" "$WEB_V1_DIGEST"

finish
