#!/bin/bash
# A blocked hook keeps the admitted deploy alive while its terminal watcher is interrupted.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"
unset BERTHY_CONFIG
selected_config="$RUN/selected config.json"
cp "$RUN/admin.json" "$selected_config" || exit 1

id=
driver_pid=
started=
cleanup() {
  # This fixture is exclusive to this project and environment.
  if [ "$started" = yes ]; then
    active=$(adm lock status --json 2>/dev/null | jq -r '.operation_id // empty')
    if [ -n "$active" ] && { [ -z "$id" ] || [ "$active" = "$id" ]; }; then
      adm cancel "$active" >/dev/null 2>&1 || echo 'could not cancel fixture operation' >&2
    fi
  fi
  if [ -n "$driver_pid" ]; then
    kill "$driver_pid" 2>/dev/null || true
    wait "$driver_pid" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# The orchestrator compares exact cross-version transcripts separately.
text=$(ci deploy web --version "$WEB_V1_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
expect_code 'direct digest deploy (text)' 0 "$code" "$text"
expect_contains 'text includes accepted' "$text" $'accepted\n'
expect_contains 'text includes succeeded' "$text" $'succeeded\n'
expect_contains 'text includes terminal' "$text" $'terminal\n'
if [[ $text == *$'\033'* ]] || [[ $text == *'deploying ('* ]]; then fail 'text has no animation' 'unexpected terminal progress'; else pass 'text has no animation'; fi
json=$(ci deploy web --version "$WEB_V2_DIGEST" --manifest-dir "$MAN" --json 2>"$RUN/progress-json.err"); code=$?
expect_code 'direct digest deploy (JSON)' 0 "$code" "$(cat "$RUN/progress-json.err")"
if printf '%s' "$json" | jq -e '.state == "succeeded"' >/dev/null && [[ $json != *$'\033'* ]] && [[ $json != *'deploying ('* ]]; then pass 'JSON is one result without animation'; else fail 'JSON is one result without animation' 'invalid result or terminal progress'; fi

cat <<YML | write_manifest
project: $PROJECT
network: $NETWORK
apps:
  web:
    kind: image
    image: $REG/e2e/web
    hooks:
      pre_deploy:
        - run: ["sh", "-c", "mkfifo /tmp/berthy-progress; echo fixture-fifo-ready; cat /tmp/berthy-progress"]
    roles:
      main:
        health:
          test: ["CMD", "true"]
          interval: 2s
          timeout: 2s
          start_period: 1s
          retries: 3
YML
record_deploy "$PROJECT" "$ENVN" web --version "$WEB_V1_DIGEST" --manifest-dir "$MAN" || exit 1
mkdir -p "$RUN/progress-pty" "$LOG/progress-pty"
cat > "$RUN/progress-steps.json" <<'JSON'
[
  {"wait_text":"following deploy (", "timeout":"100s"},
  {"snapshot":"pending"},
  {"keys":["ctrl+c"]},
  {"expect_exit":1, "timeout":"100s"},
  {"snapshot":"interrupted"}
]
JSON
stamp=$(date +%s)
started=yes
"$TUI_BIN" --command "$BIN deploy web -e $ENVN --config '$selected_config' --version $WEB_V1_DIGEST --manifest-dir $MAN" \
  --size 400x40 --script "$RUN/progress-steps.json" --out "$RUN/progress-pty" --timeout 180s \
  --env TERM=xterm --env NO_COLOR=1 --redact-env BERTHY_ADMIN_TOKEN >"$RUN/progress-driver.out" 2>&1 &
driver_pid=$!

# Replay Docker's start events from just before the watcher started, limited to this fixture.
coproc EVENTS { exec timeout 100 "$DOCKER_BIN" --host "unix://$SOCK" events --since "$stamp" \
  --filter type=container --filter event=start --filter "label=berthy.project=$PROJECT" \
  --filter "label=berthy.environment=$ENVN" --filter label=berthy.oneoff=pre_hook \
  --format '{{json .Actor}}'; }
events_pid=$EVENTS_PID
event=
read -r -t 100 event <&"${EVENTS[0]}" || true
kill "$events_pid" 2>/dev/null || true
wait "$events_pid" 2>/dev/null || true
id=$(printf '%s' "$event" | jq -r '.Attributes["berthy.operation"] // empty' 2>/dev/null)
container=$(printf '%s' "$event" | jq -r '.ID // empty' 2>/dev/null)
if [[ ! $id =~ ^[0-9a-f]{32}$ || ! $container =~ ^[0-9a-f]{64}$ ]]; then
  fail 'observe the scoped pre-deploy hook' 'no matching Docker start event'
  finish
fi
pass 'observe the scoped pre-deploy hook'
coproc GATE { exec timeout 90 "$DOCKER_BIN" --host "unix://$SOCK" logs --follow "$container" 2>/dev/null; }
gate_pid=$GATE_PID
gate=
read -r -t 90 gate <&"${GATE[0]}" || true
kill "$gate_pid" 2>/dev/null || true
wait "$gate_pid" 2>/dev/null || true
if [ "$gate" = fixture-fifo-ready ]; then pass 'hook reached its FIFO'; else fail 'hook reached its FIFO' 'ready event absent'; fi

wait "$driver_pid"; driver_code=$?
driver_pid=
expect_code 'interrupt terminal deploy watcher' 0 "$driver_code" "$(cat "$RUN/progress-driver.out")"
if [ "$driver_code" -eq 0 ]; then
  interrupted=$(cat "$RUN/progress-pty/interrupted.txt")
  if grep -Eq '^[|/\\-] following deploy \([0-9]+\.[0-9]s\)' "$RUN/progress-pty/pending.txt"; then
    grep -E '^[|/\\-] following deploy \([0-9]+\.[0-9]s\)' "$RUN/progress-pty/pending.txt" > "$LOG/progress-pty/pending.txt"
    pass 'delayed pending frame'
  else fail 'delayed pending frame' 'missing spinner frame'; fi
  expect_contains 'interruption names the operation' "$interrupted" "$id"
  expect_contains 'interruption gives inspection path' "$interrupted" "berthy diagnostics $id -e $ENVN --project $PROJECT --config '$selected_config'"
  expect_contains 'interruption gives cancellation path' "$interrupted" "berthy cancel $id -e $ENVN --project $PROJECT --config '$selected_config'"
  expect_contains 'interruption says operation may continue' "$interrupted" 'may continue'
  printf 'operation <id> may continue; diagnostics and cancel paths shown\n' > "$LOG/progress-pty/interrupted.txt"
fi

# The scoped status GET proves the admitted operation survived closing the watcher.
status=$(curl --noproxy '*' --fail --silent --show-error --max-time 30 -H "Authorization: Bearer $BERTHY_ADMIN_TOKEN" \
  "http://$AGENT_LISTEN/v1/projects/$PROJECT/environments/$ENVN/operations/$id")
if printf '%s' "$status" | jq -e --arg id "$id" '.id == $id and .state == "running"' >/dev/null; then pass 'operation remains running and inspectable'; else fail 'operation remains running and inspectable' 'status did not identify the running operation'; fi
out=$("$BIN" diagnostics "$id" -e "$ENVN" --project "$PROJECT" --config "$selected_config" 2>&1); code=$?
expect_code 'run the shown inspection command' 0 "$code" "$out"
expect_contains 'inspection identifies the operation' "$out" "$id"
out=$("$BIN" cancel "$id" -e "$ENVN" --project "$PROJECT" --config "$selected_config" 2>&1); code=$?
expect_code 'cancel the admitted operation' 0 "$code" "$out"
expect_contains 'agent accepted cancellation' "$out" 'cancel accepted'
if [ "$code" -eq 0 ] && [[ $out == *'cancel accepted'* ]]; then started=; fi
finish
