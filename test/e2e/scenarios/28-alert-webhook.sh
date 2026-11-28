#!/bin/bash
# 28-alert-webhook proves that alerts reach an HTTP webhook as well as Telegram, with the same rules, and that the URL, which is
# a secret, never shows.
#
# The manifest has alerts.telegram (the fake Bot API) and alerts.webhook (the fake webhook, its URL and one header in secrets).
# `alerts test` reports each channel and sends to both. A probe that fails is one POST and one Telegram message. A deploy that
# fails is one POST with kind deploy and one message; `alerts.deploys: all` also sends the success. A webhook that is down
# costs a deploy nothing and Telegram its message nothing; the agent logs the failure without the URL.
#
# The heartbeat (alerts.heartbeat, every 10 s) is a GET to the fake webhook about every interval, and `checks` and `doctor` show
# when the last one was accepted. While the endpoint is down the check is unknown and nobody is alerted. Stopping the agent
# stops the pings.
#
# The URLs, the header value and the bot token appear in no log line, no check and no `doctor` answer.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

hook_path=/hooks/s3cr3t-e2e-token
hook_header='Bearer e2e-hook-header-value'
beat_path=/beat/s3cr3t-e2e-beat-token

# alerts_manifest DEPLOYS HEALTH_CMD: the web app with a probe that always fails, and alerts to both channels. DEPLOYS is the
# value of alerts.deploys; HEALTH_CMD is the role's health command ("true" passes, "false" never does).
alerts_manifest() {
  cat <<YML
project: $PROJECT
network: $NETWORK
alerts:
  deploys: $1
  telegram:
    bot_token_secret: TELEGRAM_BOT_TOKEN
    chat_id: "1"
  webhook:
    url_secret: ALERT_WEBHOOK_URL
    headers_secret: ALERT_WEBHOOK_HEADERS
  heartbeat:
    url_secret: HEARTBEAT_URL
    interval: 10s
probes:
  broken:
    tcp: 127.0.0.1:1
    interval: 5s
    timeout: 2s
    failures: 1
apps:
  web:
    kind: image
    image: $REG/e2e/web
    deploy:
      deploy_timeout: 20s
    roles:
      main:
        health:
          test: ["CMD", "$2"]
          interval: 2s
          timeout: 2s
          start_period: 1s
          retries: 2
YML
}

# posts KIND [NAME]: the JSON bodies of matching webhook POSTs, as a JSON array.
posts() { hook_requests | jq -c --arg k "$1" --arg n "${2:-}" '[.[] | select(.method == "POST") | .body | fromjson | select(.kind == $k and ($n == "" or .name == $n))]'; }

# beats: how many GETs of the heartbeat the fake webhook has recorded.
beats() { hook_requests | jq "[.[] | select(.method == \"GET\" and .path == \"$beat_path\")] | length"; }

# beat_state FIELD: a field of the heartbeat check in `checks`, as the agent stores it.
beat_state() { adm checks --json | jq -r --arg f "$1" '.[] | select(.name == "heartbeat") | .[$f]'; }

# wait_beat_state STATE SECONDS: waits up to SECONDS for the heartbeat check to be in STATE.
wait_beat_state() {
  local i
  for i in $(seq 1 $(($2 * 2))); do
    [ "$(beat_state state)" = "$1" ] && return 0
    sleep 0.5
  done
  return 1
}

# wait_posts KIND N SECONDS: waits up to SECONDS for N posts of that kind, then 3 s more so a surplus one would show, and prints
# the count.
wait_posts() {
  local i
  for i in $(seq 1 $(($3 * 4))); do
    [ "$(posts "$1" "${4:-}" | jq 'length')" -ge "$2" ] && break
    sleep 0.25
  done
  sleep 3
  posts "$1" "${4:-}" | jq 'length'
}

# Count only the alert under test, but show all received texts when it fails.
tg_count() { tg_messages | jq --arg text "$1" '[.[] | select(.text | test($text))] | length'; }
probe_message_received() { [ "$(tg_count "probe:broken")" -ge 1 ]; }
expect_tg_count() {
  local messages count
  messages=$(tg_messages)
  count=$(printf '%s' "$messages" | jq --arg text "$3" '[.[] | select(.text | test($text))] | length')
  if [ "$count" = "$2" ]; then
    pass "$1"
  else
    fail "$1" "want '$2', got '$count'; received texts:"
    printf '%s\n' "$messages" | jq -r '.[].text'
  fi
}

out=$(adm secret set TELEGRAM_BOT_TOKEN < "$RUN/tg-token" 2>&1); code=$?
expect_code "secret TELEGRAM_BOT_TOKEN is set" 0 "$code" "$out"
out=$(printf '%s' "http://$HOOK_LISTEN$hook_path" | adm secret set ALERT_WEBHOOK_URL 2>&1); code=$?
expect_code "secret ALERT_WEBHOOK_URL is set" 0 "$code" "$out"
out=$(printf 'Authorization=%s' "$hook_header" | adm secret set ALERT_WEBHOOK_HEADERS 2>&1); code=$?
expect_code "secret ALERT_WEBHOOK_HEADERS is set" 0 "$code" "$out"
out=$(printf '%s' "http://$HOOK_LISTEN$beat_path" | adm secret set HEARTBEAT_URL 2>&1); code=$?
expect_code "secret HEARTBEAT_URL is set" 0 "$code" "$out"

tg_reset
hook_reset
alerts_manifest failed true | write_manifest
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1" 0 "$code" "$out"

# 1. A failing probe is one POST and one Telegram message. The monitor reads the manifest every minute, so this takes a while.
expect_eq "a failing probe is one POST" 1 "$(wait_posts check 1 120 probe:broken)"
body=$(posts check probe:broken | jq -c '.[0]')
echo "  probe alert: $body"
expect_eq "the POST names the environment" "$ENVN" "$(printf '%s' "$body" | jq -r '.environment')"
expect_eq "the POST names the check" probe:broken "$(printf '%s' "$body" | jq -r '.name')"
expect_eq "the POST gives the state" critical "$(printf '%s' "$body" | jq -r '.state')"
expect_contains "the POST gives the detail" "$(printf '%s' "$body" | jq -r '.detail')" "refused"
case $(printf '%s' "$body" | jq -r '.since') in
  20??-??-??T*Z) pass "the POST gives the time the state began" ;;
  *) fail "the POST gives the time the state began" "got $(printf '%s' "$body" | jq -r '.since')" ;;
esac
wait_for "probe Telegram alert" probe_message_received || fail "the probe Telegram alert arrives" "timed out"
expect_tg_count "the probe is one Telegram message too" 1 "probe:broken"
expect_eq "the webhook text is the Telegram text" "$(tg_messages | jq -r '[.[] | select(.text | contains("probe:broken"))][0].text')" "$(printf '%s' "$body" | jq -r '.text')"
expect_contains "the text says which check failed" "$(printf '%s' "$body" | jq -r '.text')" "probe:broken"

# 2. alerts test sends to every channel and reports each.
tg_reset
hook_reset
out=$(adm alerts test 2>&1); code=$?
expect_code "alerts test succeeds when both channels do" 0 "$code" "$out"
expect_contains "alerts test reports Telegram" "$out" "telegram: ok"
expect_contains "alerts test reports the webhook" "$out" "webhook: ok"
req=$(hook_requests | jq -c '.[0]')
expect_eq "the test alert is one POST" POST "$(printf '%s' "$req" | jq -r '.method')"
expect_eq "the POST goes to the path of the URL secret" "$hook_path" "$(printf '%s' "$req" | jq -r '.path')"
expect_eq "the POST carries the header of the headers secret" "$hook_header" "$(printf '%s' "$req" | jq -r '.headers.Authorization[0]')"
expect_eq "the POST is JSON" application/json "$(printf '%s' "$req" | jq -r '.headers["Content-Type"][0]')"
expect_eq "the test alert says kind test" test "$(printf '%s' "$req" | jq -r '.body | fromjson | .kind')"
expect_eq "the test alert gives the same text as Telegram" "$(tg_messages | jq -r '[.[] | select(.text | contains("test alert from berthy"))][0].text')" "$(printf '%s' "$req" | jq -r '.body | fromjson | .text')"
expect_tg_count "Telegram got one test alert" 1 "test alert from berthy"

# 3. A deploy that fails is one POST of kind deploy and one Telegram message.
tg_reset
hook_reset
alerts_manifest failed false | write_manifest
out=$(adm deploy web --version v2 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "the deploy with a failing health check fails" 1 "$code" "$out"
op_state=$(printf '%s' "$out" | sed -n 's/^state: //p' | tail -n 1)
op_error=$(printf '%s' "$out" | sed -n 's/^error: //p' | tail -n 1)
lock=$(adm lock status --json 2>&1)
if { [ "$op_state" = failed ] || [ "$op_state" = rolled_back ]; } && [ "$(printf '%s' "$lock" | jq -r '.locked')" = false ]; then
  pass "the failed deploy ended in a state that releases the lock"
else
  fail "the failed deploy ended in a state that releases the lock" "state $op_state error $op_error lock $lock"
fi
expect_eq "a failed deploy is one POST of kind deploy" 1 "$(wait_posts deploy 1 20)"
body=$(posts deploy | jq -c '.[0]')
echo "  deploy alert: $body"
expect_eq "the deploy POST names the app" web "$(printf '%s' "$body" | jq -r '.name')"
expect_eq "the deploy POST names the environment" "$ENVN" "$(printf '%s' "$body" | jq -r '.environment')"
case $(printf '%s' "$body" | jq -r '.state') in
  failed | rolled_back) pass "the deploy POST says the deploy did not succeed" ;;
  *) fail "the deploy POST says the deploy did not succeed" "got $(printf '%s' "$body" | jq -r '.state')" ;;
esac
expect_contains "the deploy POST gives the reason" "$(printf '%s' "$body" | jq -r '.detail')" "healthy"
expect_contains "the deploy text names the version" "$(printf '%s' "$body" | jq -r '.text')" "v2"
expect_tg_count "the failed deploy is one Telegram message" 1 "web v2 (failed|rolled back):"
expect_eq "the webhook text is the Telegram text of the deploy" "$(tg_messages | jq -r '[.[] | select(.text | test("web v2 (failed|rolled back):"))][0].text')" "$(printf '%s' "$body" | jq -r '.text')"

# 4. alerts.deploys: all sends the success to both channels too.
tg_reset
hook_reset
alerts_manifest all true | write_manifest
out=$(adm deploy web --version v2 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v2 with deploys: all" 0 "$code" "$out"
expect_eq "deploys: all sends the success as one POST" 1 "$(wait_posts deploy 1 20)"
expect_eq "the success POST says succeeded" succeeded "$(posts deploy | jq -r '.[0].state')"
expect_eq "the success POST gives the version" v2 "$(posts deploy | jq -r '.[0].detail')"

# 4b. The heartbeat is a GET about every 10 seconds, and checks and doctor show when the last one was accepted. The monitor reads
# the manifest every minute, so the first ping can take a while.
if wait_beat_state ok 120; then pass "the heartbeat check turns ok once the agent has read the manifest"; else fail "the heartbeat check turns ok once the agent has read the manifest" "state $(beat_state state): $(beat_state detail)"; fi
hook_reset
sleep 28
count=$(beats)
echo "  heartbeat GETs in 28 s: $count"
if [ "$count" -ge 2 ] && [ "$count" -le 4 ]; then pass "the heartbeat pings about every 10 seconds"; else fail "the heartbeat pings about every 10 seconds" "got $count GETs in 28 s"; fi
first=$(hook_requests | jq -c '[.[] | select(.method == "GET")][0]')
expect_eq "the ping is a GET with no body" "" "$(printf '%s' "$first" | jq -r '.body')"
expect_eq "the ping goes to the path of the URL secret" "$beat_path" "$(printf '%s' "$first" | jq -r '.path')"
expect_eq "the heartbeat check is ok" ok "$(beat_state state)"
detail=$(beat_state detail)
echo "  heartbeat detail: $detail"
expect_contains "checks shows when the last ping was accepted" "$detail" "last ping accepted at 20"
doc=$(HOME="$RUN/doctor-no-copy" adm doctor --json 2>"$RUN/doctor-keycopy-stderr")
expect_contains "doctor sends the key copy warning to stderr" "$(cat "$RUN/doctor-keycopy-stderr")" "warning: this machine has no saved copy of the current state key"
expect_eq "doctor shows the heartbeat as ok" true "$(printf '%s' "$doc" | jq -r '.checks[] | select(.name == "heartbeat") | .ok')"
expect_contains "doctor shows when the last ping was accepted" "$(printf '%s' "$doc" | jq -r '.checks[] | select(.name == "heartbeat") | .detail')" "last ping accepted at 20"

# 4c. While the endpoint is down the heartbeat check is unknown, with the time of the last accepted ping, and nobody is alerted.
tg_reset
stop_hook
if wait_beat_state unknown 60; then pass "the heartbeat check turns unknown when the URL cannot be reached"; else fail "the heartbeat check turns unknown when the URL cannot be reached" "state $(beat_state state)"; fi
detail=$(beat_state detail)
echo "  heartbeat detail: $detail"
expect_contains "the detail says the ping failed" "$detail" "ping failed: unreachable"
expect_contains "the detail keeps the time of the last accepted ping" "$detail" "last ping accepted at 20"
sleep 30
expect_tg_count "an unknown heartbeat never alerts" 0 "heartbeat"
expect_eq "the heartbeat check is still unknown, not worse" unknown "$(beat_state state)"
start_hook || fail "the fake webhook restarts" "it did not answer"
if wait_beat_state ok 60; then pass "the heartbeat check recovers when the URL answers again"; else fail "the heartbeat check recovers when the URL answers again" "state $(beat_state state)"; fi
expect_tg_count "recovery sends no message either" 0 "heartbeat"

# 4d. Stopping the agent stops the pings.
stop_agent
sleep 3
hook_reset
sleep 25
expect_eq "a stopped agent sends no ping" 0 "$(beats)"
start_agent || fail "the agent restarts" "it did not answer"

# 5. A webhook that is down costs a deploy nothing and Telegram its message nothing, and the failure is logged without the URL.
stop_hook
tg_reset
alerts_manifest failed false | write_manifest
start=$SECONDS
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
elapsed=$((SECONDS - start))
expect_code "a failing deploy with the webhook down still ends the same way" 1 "$code" "$out"
if [ "$elapsed" -lt 60 ]; then pass "the deploy did not wait for the webhook (${elapsed}s)"; else fail "the deploy did not wait for the webhook" "took ${elapsed}s"; fi
for i in $(seq 1 40); do
  [ "$(tg_count "web v1 (failed|rolled back):")" -ge 1 ] && grep -q 'deploy alert not sent' "$LOG/agent.log" && break
  sleep 0.25
done
expect_tg_count "Telegram still gets the message" 1 "web v1 (failed|rolled back):"
line=$(grep 'deploy alert not sent' "$LOG/agent.log" | tail -n 1)
echo "  log line: $line"
expect_contains "the agent logs the webhook failure" "$line" "webhook: webhook unavailable"
start_hook || fail "the fake webhook restarts" "it did not answer"

# 6. The doctor checks that the secrets of every channel are stored.
out=$(HOME="$RUN/doctor-no-copy" adm doctor --json 2>"$RUN/doctor-keycopy-stderr")
expect_eq "doctor shows the webhook check as ok" true "$(printf '%s' "$out" | jq -r '.checks[] | select(.name == "webhook") | .ok')"
expect_eq "doctor shows the Telegram check as ok" true "$(printf '%s' "$out" | jq -r '.checks[] | select(.name == "telegram") | .ok')"
rm_out=$(adm secret rm ALERT_WEBHOOK_HEADERS 2>&1); code=$?
expect_code "secret ALERT_WEBHOOK_HEADERS is removed" 0 "$code" "$rm_out"
out2=$(HOME="$RUN/doctor-no-copy" adm doctor --json 2>"$RUN/doctor-keycopy-stderr")
expect_eq "doctor shows the webhook check failing when a secret is gone" false "$(printf '%s' "$out2" | jq -r '.checks[] | select(.name == "webhook") | .ok')"
expect_contains "doctor says which secret" "$out2" "webhook headers secret missing"
out3=$(adm alerts test 2>&1); code=$?
expect_code "alerts test fails when a channel cannot send" 1 "$code" "$out3"
expect_contains "alerts test still sends to Telegram" "$out3" "telegram: ok"
expect_contains "alerts test says why the webhook cannot send" "$out3" "webhook: failed: webhook headers secret missing"

# 7. The URL, the header value and the bot token appear nowhere.
checks=$(adm checks 2>&1)
for secret in "s3cr3t-e2e-token" "s3cr3t-e2e-beat-token" "e2e-hook-header-value" "$(cat "$RUN/tg-token")"; do
  expect_not_contains "the logs hold no secret" "$(cat "$LOG"/agent.log "$LOG"/webhook.log)" "$secret"
  expect_not_contains "checks holds no secret" "$checks" "$secret"
  expect_not_contains "doctor holds no secret" "$out$out2$out3$doc" "$secret"
done

finish
