#!/bin/bash
# 27-app-down proves the down check of a Compose service. The service's container is stopped by hand: its check
# down:<project>/<service> is unknown for the first two minutes and critical after them, one alert reaches the fake Telegram,
# and the alert gives the exit code (137, killed after the stop timeout). Starting the container again sends the recovery.
# A deploy and a restart of an image app, which change its containers, send no down alert, and the image app's own check
# stays ok. A Compose one-shot that finished with code 0 is not wanted, so its check reads ok and never alerts.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

proj="$PROJECT-db"
check="down:$proj/side"

# check_state NAME: the stored state of the check NAME, or nothing while the monitor does not list it.
check_state() { adm checks --json 2>/dev/null | jq -r --arg n "$1" '.[] | select(.name == $n) | .state' | head -n 1; }

# wait_state NAME STATE SECONDS: waits up to SECONDS for the check NAME to read STATE. It returns 0 when it does.
wait_state() {
  local i
  for i in $(seq 1 "$3"); do
    [ "$(check_state "$1")" = "$2" ] && return 0
    sleep 1
  done
  return 1
}

# down_messages TEXT: how many messages the fake Telegram holds about a down check and contain TEXT.
down_messages() { tg_messages | jq -r --arg t "$1" '[.[] | select((.text | contains("down:")) and (.text | contains($t)))] | length'; }

# wait_messages TEXT N SECONDS: waits up to SECONDS for N down messages that contain TEXT, then prints the count.
wait_messages() {
  local i
  for i in $(seq 1 "$3"); do
    [ "$(down_messages "$1")" -ge "$2" ] && break
    sleep 1
  done
  down_messages "$1"
}

adm secret set TELEGRAM_BOT_TOKEN < "$RUN/tg-token" >/dev/null

# The manifest has alerts, the image app web, and the Compose app db with one service, side, that sleeps.
write_manifest <<YML
project: $PROJECT
network: $NETWORK
alerts:
  telegram:
    bot_token_secret: TELEGRAM_BOT_TOKEN
    chat_id: "1"
apps:
  web:
    kind: image
    image: $REG/e2e/web
    roles:
      main:
        health:
          test: ["CMD", "true"]
          interval: 2s
          timeout: 2s
          start_period: 1s
          retries: 3
  db:
    kind: compose
    file: compose.yml
YML
cat > "$MAN/compose.yml" <<YML
services:
  side:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sleep", "3600"]
YML
# The one-shot is a service of its own file, started in the project by hand after the deploy: Berthy's deploy runs
# compose up --wait, which refuses any service whose container has already exited, so a deploy cannot carry one.
cat > "$MAN/job.yml" <<YML
services:
  job:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["true"]
YML

out=$(adm deploy db --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy the Compose app db" 0 "$code" "$out"
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy the image app web at v1" 0 "$code" "$out"

# The check reads unknown for the two minutes after the deploy of db ends, so it reads ok only after that.
if wait_state "$check" ok 300; then pass "the service's down check reads ok while its container runs"; else fail "the service's down check reads ok while its container runs" "$(adm checks 2>&1)"; fi
cid=$(d ps -q --filter label=com.docker.compose.project="$proj" --filter label=com.docker.compose.service=side | head -n 1)
expect_eq "the service has one running container" "1" "$(printf '%s\n' "$cid" | grep -c .)"

# The one-shot runs true and finishes with code 0: nothing is wanted of it, so its check never alerts.
out=$(DOCKER_HOST="unix://$SOCK" DOCKER_CONFIG="$E2E_ROOT/docker-config" "$COMPOSE_BIN" -p "$proj" -f "$MAN/compose.yml" -f "$MAN/job.yml" up -d job 2>&1); code=$?
expect_code "start the one-shot in the project" 0 "$code" "$out"
jid=$(d ps -aq --filter label=com.docker.compose.project="$proj" --filter label=com.docker.compose.service=job | head -n 1)
for _ in $(seq 1 30); do
  [ "$(d inspect --format '{{.State.Status}}' "$jid" 2>/dev/null)" = exited ] && break
  sleep 1
done
expect_eq "the one-shot finished with code 0" "0" "$(d inspect --format '{{.State.ExitCode}}' "$jid" 2>/dev/null)"

# Stop the container by hand. Its service has no restart policy, so nothing starts it again.
tg_reset
start=$SECONDS
d stop -t 1 "$cid" >/dev/null 2>&1
if wait_state "$check" critical 300; then pass "the down check turns critical"; else fail "the down check turns critical" "$(adm checks 2>&1)"; fi
elapsed=$((SECONDS - start))
# The confirmation counts from the first monitor run that sees the stop, so the check cannot be critical before two minutes.
if [ "$elapsed" -ge 119 ]; then pass "the check turned critical after the two-minute confirmation (${elapsed}s)"; else fail "the check turned critical after the two-minute confirmation" "after ${elapsed}s"; fi

expect_eq "one critical alert reaches the chat" "1" "$(wait_messages "🔴" 1 30)"
text=$(tg_messages | jq -r --arg c "$check" '[.[] | select(.text | contains($c))][0].text // ""')
echo "  message: $(printf '%s' "$text" | tr '\n' '|')"
expect_contains "the alert names the environment and the check" "$text" "$ENVN $check"
expect_contains "the alert gives the exit code of the stopped container" "$text" "exited with code 137 for "
expect_contains "the alert is critical" "$text" "🔴"

# Still stopped: no second alert, since reminders are every four hours by default.
sleep 30
expect_eq "no second alert while the service stays down" "1" "$(down_messages "")"

d start "$cid" >/dev/null 2>&1
if wait_state "$check" ok 150; then pass "the down check reads ok again once the container runs"; else fail "the down check reads ok again once the container runs" "$(adm checks 2>&1)"; fi
expect_eq "the recovery reaches the chat" "1" "$(wait_messages "recovered" 1 30)"
text=$(tg_messages | jq -r --arg c "$check" '[.[] | select(.text | contains($c) and contains("recovered"))][0].text // ""')
expect_contains "the recovery names the environment and the check" "$text" "✅ $ENVN $check recovered"

# A restart and a deploy of the image app change its containers: neither raises a down alert, and its own check stays ok.
before=$(down_messages "")
out=$(adm restart web 2>&1); code=$?
expect_code "restart the image app" 0 "$code" "$out"
out=$(adm deploy web --version v2 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v2 over the running app" 0 "$code" "$out"
# The check reads unknown for the two minutes after the deploy ends, so it reads ok only after that.
if wait_state "down:web/main" ok 300; then pass "the image app's down check reads ok after the restart and the deploy"; else fail "the image app's down check reads ok after the restart and the deploy" "$(adm checks 2>&1)"; fi
sleep 10
expect_eq "a restart and a deploy send no down alert" "$before" "$(down_messages "")"

# The finished one-shot is never judged down: its check reads ok and no message names it.
if wait_state "down:$proj/job" ok 300; then pass "the one-shot's down check reads ok, as nothing is wanted of it"; else fail "the one-shot's down check reads ok" "$(adm checks 2>&1)"; fi
expect_eq "the finished one-shot sends no down alert" "0" "$(down_messages "/job")"

finish
