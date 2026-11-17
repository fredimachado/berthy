#!/bin/bash
# 08c-interactive proves `exec -i` and `run -i` on the real Swarm. With a terminal (a pseudo-terminal that tuidriver drives)
# the person types into a shell in the container, the size follows the terminal, and the remote exit code is the CLI's. Piped
# (no terminal) stdin streams to the command, stdout and stderr stay apart, and the exit code still comes through. The
# session is in the audit log by its start and end and never by its keystrokes, an agent token is refused one on a protected
# environment, a run session leaves no container behind, and an agent that dies leaves none for long: the next one removes it.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

cat > "$MAN/compose.yml" <<YML
services:
  postgres:
    image: postgres:17-alpine
    environment:
      POSTGRES_PASSWORD: \${POSTGRES_PASSWORD}
YML

{
  base_manifest
  cat <<YML
  db:
    kind: compose
    file: compose.yml
    env:
      plain:
        POSTGRES_PASSWORD: e2e-pass
YML
} | write_manifest

out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1" 0 "$code" "$out"

out=$(adm deploy db --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy the Compose app" 0 "$code" "$out"

# drive NAME COMMAND: runs COMMAND in a pseudo-terminal of 120 columns by 40 rows and plays the steps on stdin. The snapshots and
# the result of each step stay under $LOG/tui-NAME, which run.sh keeps with the logs of the scenario.
drive() {
  local name=$1 dir=$LOG/tui-$1
  rm -rf "$dir"
  mkdir -p "$dir"
  cat > "$dir/script.json"
  "$TUI_BIN" --command "$2" --size 120x40 --script "$dir/script.json" --out "$dir/out" --timeout 90s --redact-env BERTHY_ADMIN_TOKEN 2>&1
}

# session_containers: the ids of the containers of run sessions on the daemon, one per line.
session_containers() { d ps -aq --filter label=berthy.environment=$ENVN --filter label=berthy.session; }

# wait_running_session: waits until a run session's container runs, and then a moment more, so the session is open and its
# start is recorded.
wait_running_session() {
  local i
  for i in $(seq 1 40); do
    [ -n "$(d ps -q --filter label=berthy.environment=$ENVN --filter label=berthy.session --filter status=running)" ] && break
    sleep 0.25
  done
  sleep 1
}

# no_session_containers: waits until no run session container is left, which happens just after the session's last frame.
no_session_containers() {
  local i
  for i in $(seq 1 40); do
    [ -z "$(session_containers)" ] && return 0
    sleep 0.25
  done
  return 1
}

# A terminal: exec.
out=$(drive exec "$BIN exec web -i -e $ENVN --config $RUN/admin.json -- sh" <<'JSON'
[
  {"wait_text": "#", "timeout": "20s"},
  {"sleep": "500ms"},
  {"type": "echo hello-$((40+2))"}, {"keys": ["enter"]},
  {"wait_text": "hello-42"},
  {"type": "stty size"}, {"keys": ["enter"]},
  {"wait_text": "40 120"},
  {"snapshot": "before-resize"},
  {"resize": "100x30"}, {"sleep": "500ms"},
  {"type": "stty size"}, {"keys": ["enter"]},
  {"wait_text": "30 100"},
  {"snapshot": "after-resize"},
  {"type": "exit 5"}, {"keys": ["enter"]},
  {"expect_exit": 5}
]
JSON
); code=$?
expect_code "exec -i in a terminal: typing, output, resize, exit 5" 0 "$code" "$out"

# A terminal: run, in a new container of the applied release.
out=$(drive run "$BIN run web -i -e $ENVN --config $RUN/admin.json -- sh" <<'JSON'
[
  {"wait_text": "#", "timeout": "30s"},
  {"sleep": "500ms"},
  {"type": "echo role-$BERTHY_ROLE v-$BERTHY_VERSION"}, {"keys": ["enter"]},
  {"wait_text": "role-main v-v1"},
  {"type": "stty size"}, {"keys": ["enter"]},
  {"wait_text": "40 120"},
  {"resize": "100x30"}, {"sleep": "500ms"},
  {"type": "stty size"}, {"keys": ["enter"]},
  {"wait_text": "30 100"},
  {"snapshot": "run-resized"},
  {"type": "exit 5"}, {"keys": ["enter"]},
  {"expect_exit": 5}
]
JSON
); code=$?
expect_code "run -i in a terminal: the role's environment, resize, exit 5" 0 "$code" "$out"
if no_session_containers; then pass "the run session's container is removed"; else fail "the run session's container is removed" "$(session_containers)"; fi

# Leaving a terminal run session by closing the connection (Ctrl-C in the terminal reaches the shell, so the driver kills the CLI
# with its own SIGTERM at the end of the script) leaves nothing behind either.
out=$(drive run-left "$BIN run web -i -e $ENVN --config $RUN/admin.json -- sleep 300" <<'JSON'
[
  {"sleep": "3s"},
  {"snapshot": "running"}
]
JSON
); code=$?
expect_code "a run session left by killing the CLI" 0 "$code" "$out"
if no_session_containers; then pass "a run session whose client left leaves no container"; else fail "a run session whose client left leaves no container" "$(session_containers)"; fi

# No terminal: stdin streams, stdout and stderr stay apart, the exit code comes through.
ready=1
for _ in $(seq 1 60); do
  out=$(adm exec db --service postgres -- pg_isready -U postgres 2>&1); code=$?
  if [ "$code" -eq 0 ]; then
    ready=0
    break
  fi
  sleep 1
done
expect_code "postgres accepts connections" 0 "$ready" "$out"

out=$(printf 'select 6*7;\n' | adm exec db --service postgres -i -- psql -U postgres -tA 2>&1); code=$?
expect_code "piped stdin reaches psql" 0 "$code" "$out"
expect_eq "psql prints the answer and nothing else" "42" "$out"

out=$(printf 'abc\ndef\n' | adm exec web -i -- cat 2>&1); code=$?
expect_code "cat copies piped stdin" 0 "$code" "$out"
expect_eq "cat's output is exactly the input" "$(printf 'abc\ndef')" "$out"

size=$(head -c 3000000 /dev/zero | adm exec web -i -- wc -c 2>&1)
expect_eq "three megabytes of stdin arrive whole" "3000000" "$(printf '%s' "$size" | tr -d ' ')"

adm exec web -i -- sh -c 'echo out; echo err >&2' </dev/null >"$LOG/split.out" 2>"$LOG/split.err"; code=$?
expect_code "exec -i with stdout and stderr" 0 "$code"
expect_eq "stdout holds only the command's stdout" "out" "$(cat "$LOG/split.out")"
expect_eq "stderr holds only the command's stderr" "err" "$(cat "$LOG/split.err")"

adm exec web -i -- sh -c 'exit 9' </dev/null >/dev/null 2>&1; code=$?
expect_code "exec -i exits with the command's own code" 9 "$code"

printf 'abc' | adm run web -i -- cat >"$LOG/run.out" 2>&1; code=$?
expect_code "run -i with piped stdin" 0 "$code"
expect_eq "the run's output is the input" "abc" "$(cat "$LOG/run.out")"
adm run web -i -- sh -c 'exit 4' </dev/null >/dev/null 2>&1; code=$?
expect_code "run -i exits with the command's own code" 4 "$code"
if no_session_containers; then pass "no run session container is left"; else fail "no run session container is left" "$(session_containers)"; fi

out=$(adm run web -i --timeout 2s -- sleep 30 </dev/null 2>&1); code=$?
expect_code "a run session past --timeout fails" 1 "$code" "$out"
expect_contains "the failure says it timed out" "$out" "run timed out after 2s"
if no_session_containers; then pass "the timed-out session's container is removed"; else fail "the timed-out session's container is removed" "$(session_containers)"; fi

out=$(adm deploy web --version v2 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "a deploy right after sessions succeeds: a session holds no lock" 0 "$code" "$out"

# A session that lasts does not block a deploy.
adm exec web -i -- sleep 20 </dev/null >/dev/null 2>&1 &
sleeper=$!
sleep 2
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "a deploy while a session is open" 0 "$code" "$out"
kill "$sleeper" 2>/dev/null
wait "$sleeper" 2>/dev/null

# A dry run ignores -i.
out=$(adm exec web -i --dry-run -- true 2>&1); code=$?
expect_code "exec -i --dry-run plans" 0 "$code" "$out"
expect_contains "the plan names the role's service" "$out" "target: e2e-web-main"

# Refusals from the agent are shown.
out=$(adm run db -i -- true </dev/null 2>&1); code=$?
expect_code "run -i of a Compose app is refused" 1 "$code" "$out"
expect_contains "the refusal says to use exec --service" "$out" "use exec --service"

# The audit log has the start with its command and the end with the exit code.
out=$(adm audit 2>&1)
expect_contains "the audit log has a session's start with its command" "$out" "started: sh -c exit 9"
expect_contains "the audit log has a session's end with its exit code" "$out" "ended: exit 9"
expect_contains "the audit log names the action" "$out" "session exec web"
expect_contains "the audit log has a run session" "$out" "session run web"
expect_not_contains "the audit log holds no input" "$out" "select 6*7"
expect_not_contains "the audit log holds no output" "$out" "hello-42"

# An agent that stops closes its sessions first: the client is told, and the container of a run session is removed.
adm run web -i -- sleep 300 </dev/null >"$LOG/stopping.out" 2>&1 &
stopping=$!
wait_running_session
stop_agent
wait "$stopping"; code=$?
expect_code "a client of a stopping agent exits 1" 1 "$code"
expect_contains "and is told why" "$(cat "$LOG/stopping.out")" "the agent is stopping"
if no_session_containers; then pass "a stopping agent removed the run session's container"; else fail "a stopping agent removed the run session's container" "$(session_containers)"; fi
start_agent || fail "restart the agent" "the agent did not answer"
out=$(adm audit 2>&1)
expect_contains "the audit log says the agent was stopping" "$out" "ended: the agent is stopping"

# An agent that dies leaves the container of its run session; the next agent removes it.
adm run web -i -- sleep 300 </dev/null >/dev/null 2>&1 &
orphaned=$!
wait_running_session
if [ -n "$(session_containers)" ]; then pass "the run session has a container"; else fail "the run session has a container" "none appeared"; fi
kill -9 "$(cat "$RUN/agent.pid")"
sleep 1
if [ -n "$(session_containers)" ]; then pass "the container outlives a killed agent"; else fail "the container outlives a killed agent" "it is gone already"; fi
wait "$orphaned" 2>/dev/null
rm -f "$RUN/agent.pid"
start_agent || fail "restart the agent" "the agent did not answer"
if no_session_containers; then pass "the restarted agent removed the orphan"; else fail "the restarted agent removed the orphan" "$(session_containers)"; fi

# An agent token: an admin one may open a session on an unprotected environment, and is refused on a protected one.
token_out=$("$BIN" agent token -e "$ENVN" --name ci-admin --scope admin --agent) || fail "make an agent admin token" "agent token failed"
ciadmin_token=$(printf '%s\n' "$token_out" | sed -n 's/^token: //p')
ciadmin_entry=$(printf '%s\n' "$token_out" | sed -n '3p')
sed -i "s|]}\$|,$ciadmin_entry]}|" "$RUN/agent.json"
printf "export BERTHY_CIADMIN_TOKEN='%s'\n" "$ciadmin_token" >> "$RUN/env.sh"
export BERTHY_CIADMIN_TOKEN=$ciadmin_token
printf '{"environments":{"%s":{"url":"http://%s","token_env":"BERTHY_CIADMIN_TOKEN"}}}\n' "$ENVN" "$AGENT_LISTEN" > "$RUN/ciadmin.json"
unset token_out ciadmin_token ciadmin_entry
cia() { local c=$1; shift; "$BIN" "$c" -e "$ENVN" --config "$RUN/ciadmin.json" "$@"; }
stop_agent
start_agent || fail "restart the agent" "the agent did not answer"

out=$(cia exec web -i -- echo agent-ok </dev/null 2>&1); code=$?
expect_code "an agent token with admin scope opens a session on an unprotected environment" 0 "$code" "$out"
expect_eq "and gets the output" "agent-ok" "$out"

{ echo "protected: true"; base_manifest; } | write_manifest
out=$(adm deploy web --version "$WEB_V2_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "protect the environment with a digest deploy" 0 "$code" "$out"

before=$(adm audit 2>&1)
out=$(cia exec web -i -- echo nope </dev/null 2>&1); code=$?
expect_code "an agent token is refused a session on a protected environment" 1 "$code" "$out"
expect_contains "the refusal says to use a person's token" "$out" "an interactive session cannot be held for approval; use a person's token"
expect_not_contains "the command did not run" "$out" "nope"
expect_eq "the refusal left nothing in the audit log" "$before" "$(adm audit 2>&1)"
out=$(adm approvals 2>&1)
expect_not_contains "nothing was held for approval" "$out" "exec web"

out=$(adm exec web -i -- echo person-ok </dev/null 2>&1); code=$?
expect_code "a person's token still opens one there" 0 "$code" "$out"
expect_eq "and gets the output" "person-ok" "$out"

finish
