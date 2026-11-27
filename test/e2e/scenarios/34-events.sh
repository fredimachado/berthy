#!/bin/bash
# 34-events proves that Docker's events wake the monitor early. The monitor reads the inventory every three minutes while the
# agent follows Docker's event feed, and every minute otherwise, and the drift check runs every five minutes, so each of the
# changes below shows within seconds only because an event asked for an early read:
#   - a service changed by hand with docker service update: the drift check warns (service update event);
#   - a deploy over it: it ends promptly and clears the warning (the health wait reacts to the container's events);
#   - a container of a Compose service killed: its down check names the exit code at once, still unconfirmed (die event);
#   - a Compose service that keeps dying and that Docker keeps starting again: its restart check warns, and the chat hears of
#     it (die events; each one gives the monitor the restart count it needs to see growth);
#   - an agent restart while that loop continues: the restart check turns unknown while it warms up, and the chat gets no
#     false recovery.
# The checks decide as before; the events only make them run early. The out-of-memory kill (oom event) is covered by a unit
# test: the WSL kernel of the harness gives a container swap, so a memory limit makes it swap instead of dying.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

proj="$PROJECT-$ENVN-db"
svc="$PROJECT-$ENVN-web-main"
# What a wake may take, in seconds. Without events the shortest wait is the next minute's rebuild, and the drift check's is five.
WAKE=20

now_ms() { echo $(($(date +%s%N) / 1000000)); }

# check_field NAME FIELD: a field of the stored check NAME, or nothing while the monitor does not list it.
check_field() { adm checks --json 2>/dev/null | jq -r --arg n "$1" --arg f "$2" '.[] | select(.name == $n) | .[$f]' | head -n 1; }

# wait_check NAME FIELD PATTERN SECONDS: waits up to SECONDS for the field of the check to contain PATTERN. It prints the
# seconds it took, to one decimal, and returns 0 when the field matched.
wait_check() {
  local start end
  start=$(now_ms)
  end=$((start + $4 * 1000))
  while [ "$(now_ms)" -lt "$end" ]; do
    if [[ "$(check_field "$1" "$2")" == *"$3"* ]]; then
      awk -v ms=$(($(now_ms) - start)) 'BEGIN { printf "%.1f\n", ms / 1000 }'
      return 0
    fi
    sleep 0.5
  done
  echo "$4+"
  return 1
}

# container_of SERVICE: the ID of the running container of a service of the Compose project.
container_of() { d ps -q --filter label=com.docker.compose.project="$proj" --filter label=com.docker.compose.service="$1" | head -n 1; }

adm secret set TELEGRAM_BOT_TOKEN < "$RUN/tg-token" >/dev/null

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
# crash dies three seconds after it starts, and Docker starts it again each time. It is a service of its own file, started in
# the project by hand after the deploy: Berthy's deploy runs compose up --wait, which refuses a restarting container.
cat > "$MAN/crash.yml" <<YML
services:
  crash:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sh", "-c", "sleep 3; exit 3"]
    restart: always
YML

out=$(adm deploy db --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy the Compose app db" 0 "$code" "$out"
start=$(now_ms)
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy the image app web at v1" 0 "$code" "$out"
echo "  deploy web v1 took $(awk -v ms=$(($(now_ms) - start)) 'BEGIN { printf "%.1f", ms / 1000 }') s"

# 1. A service changed by hand: the drift check runs on the service's update event, not at its five-minute interval.
if wait_check drift:web detail "matches release" 60 >/dev/null; then pass "the drift check reads ok after the deploy"; else fail "the drift check reads ok after the deploy" "$(adm checks 2>&1)"; fi
d service update --detach --env-add I35_BY_HAND=1 "$svc" >/dev/null 2>&1; code=$?
expect_code "change the service by hand" 0 "$code"
took=$(wait_check drift:web state warning $WAKE); code=$?
if [ "$code" -eq 0 ]; then pass "the drift check warned $took s after the hand update"; else fail "the drift check warned within ${WAKE}s of the hand update" "$(adm checks 2>&1)"; fi

# 2. A deploy over it ends without waiting for a deadline, and clears the warning.
start=$(now_ms)
out=$(adm deploy web --version v2 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v2 over the hand-changed service" 0 "$code" "$out"
took=$(awk -v ms=$(($(now_ms) - start)) 'BEGIN { printf "%.1f", ms / 1000 }')
echo "  deploy web v2 took $took s"
if [ "${took%.*}" -lt 60 ]; then pass "the deploy did not wait for a deadline"; else fail "the deploy did not wait for a deadline" "took $took s"; fi
if wait_check drift:web detail "matches release" $WAKE >/dev/null; then pass "the deploy cleared the drift warning"; else fail "the deploy cleared the drift warning" "$(adm checks 2>&1)"; fi

# 3. A container killed: the down check names the exit code in seconds. It stays unconfirmed for two minutes, as before.
# Operations on the project keep its checks quiet for two minutes, so the check reads ok only after that.
if wait_check "down:$proj/side" state ok 300 >/dev/null; then pass "the service's down check reads ok while its container runs"; else fail "the service's down check reads ok" "$(adm checks 2>&1)"; fi
cid=$(container_of side)
expect_eq "the service has one running container" "1" "$(printf '%s\n' "$cid" | grep -c .)"
d kill "$cid" >/dev/null 2>&1
took=$(wait_check "down:$proj/side" detail "exited with code 137, not yet confirmed" $WAKE); code=$?
if [ "$code" -eq 0 ]; then pass "the down check named the exit code $took s after the kill"; else fail "the down check named the exit code within ${WAKE}s of the kill" "$(adm checks 2>&1)"; fi
expect_eq "the down check is still unconfirmed" "unknown" "$(check_field "down:$proj/side" state)"
d start "$cid" >/dev/null 2>&1

# 4. A service that keeps dying and that Docker keeps starting again: each death wakes the monitor, which reads the restart count
# each time, so the restart check sees the growth within seconds of the third restart, and the chat hears of it. Without the
# events, the service would not even have a check until the next minute's rebuild, and growth needs reads that are apart.
loop="restarts:$proj/crash"
tg_reset
out=$(DOCKER_HOST="unix://$SOCK" DOCKER_CONFIG="$E2E_ROOT/docker-config" "$COMPOSE_BIN" -p "$proj" -f "$MAN/compose.yml" -f "$MAN/crash.yml" up -d crash 2>&1); code=$?
expect_code "start the crashing service in the project" 0 "$code" "$out"
took=$(wait_check "$loop" detail "restarted " 45); code=$?
if [ "$code" -eq 0 ]; then pass "the restart check saw the loop $took s after the service started"; else fail "the restart check saw the loop within 45s" "$(adm checks 2>&1)"; fi
for _ in $(seq 1 $WAKE); do
  [ "$(tg_messages | jq -r '[.[] | select(.text | contains("restarted "))] | length')" -ge 1 ] && break
  sleep 1
done
text=$(tg_messages | jq -r '[.[] | select(.text | contains("restarted "))][0].text // ""')
echo "  message: $(printf '%s' "$text" | tr '\n' '|')"
expect_contains "the chat heard of the restart loop" "$text" "$ENVN $loop: a replica restarted "
expect_contains "the alert gives the window" "$text" " times in 10 minutes"

# 5. An agent restart while the loop continues must not send a false recovery: the watch answers unknown until it has
# watched for one restart window again.
tg_reset
stop_agent
start_agent >/dev/null 2>&1; code=$?
expect_code "the agent restarts while the crash loop continues" 0 "$code"
took=$(wait_check "$loop" detail "watching restarts since agent start" 45); code=$?
if [ "$code" -eq 0 ]; then pass "the restart check warmed up as unknown $took s after the agent restart"; else fail "the restart check warmed up as unknown within 45s of the agent restart" "$(adm checks 2>&1)"; fi
expect_eq "the restart check is unknown during warm-up" "unknown" "$(check_field "$loop" state)"
# Restart-loop conditions show in berthy checks (doctor does not list them).
out=$(adm checks 2>&1)
expect_contains "checks shows the warm-up detail" "$out" "watching restarts since agent start"
for _ in $(seq 1 10); do
  [ "$(tg_messages | jq -r '[.[] | select(.text | contains("recovered"))] | length')" -ge 1 ] && break
  sleep 1
done
expect_eq "no recovery reaches the chat during warm-up" 0 "$(tg_messages | jq -r '[.[] | select(.text | contains("recovered"))] | length')"

finish
