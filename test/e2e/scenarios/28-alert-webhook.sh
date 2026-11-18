#!/bin/bash
# 28-alert-webhook proves that alerts reach an HTTP webhook as well as Telegram, with the same rules, and that the URL, which is
# a secret, never shows.
#
# The manifest has alerts.telegram (the fake Bot API) and alerts.webhook (the fake webhook, its URL and one header in secrets).
# `alerts test` reports each channel and sends to both. A probe that fails is one POST and one Telegram message. A deploy that
# fails is one POST with kind deploy and one message; `alerts.deploys: all` also sends the success. A webhook that is down
# costs a deploy nothing and Telegram its message nothing; the agent logs the failure without the URL. The URL, the header
# value and the bot token appear in no log line, no check and no `doctor` answer.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

hook_path=/hooks/s3cr3t-e2e-token
hook_header='Bearer e2e-hook-header-value'

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

# posts KIND: the JSON bodies of the webhook's POSTs of that kind, as a JSON array.
posts() { hook_requests | jq -c --arg k "$1" '[.[] | select(.method == "POST") | .body | fromjson | select(.kind == $k)]'; }

# wait_posts KIND N SECONDS: waits up to SECONDS for N posts of that kind, then 3 s more so a surplus one would show, and prints
# the count.
wait_posts() {
  local i
  for i in $(seq 1 $(($3 * 4))); do
    [ "$(posts "$1" | jq 'length')" -ge "$2" ] && break
    sleep 0.25
  done
  sleep 3
  posts "$1" | jq 'length'
}

adm secret set TELEGRAM_BOT_TOKEN < "$RUN/tg-token" >/dev/null
printf '%s' "http://$HOOK_LISTEN$hook_path" | adm secret set ALERT_WEBHOOK_URL >/dev/null
printf 'Authorization=%s' "$hook_header" | adm secret set ALERT_WEBHOOK_HEADERS >/dev/null

alerts_manifest failed true | write_manifest
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1" 0 "$code" "$out"

# 1. alerts test sends to every channel and reports each.
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
expect_eq "the test alert gives the same text as Telegram" "$(tg_messages | jq -r '.[0].text')" "$(printf '%s' "$req" | jq -r '.body | fromjson | .text')"
expect_eq "Telegram got one message" 1 "$(tg_messages | jq 'length')"

# 2. A failing probe is one POST and one Telegram message. The monitor reads the manifest every minute, so this takes a while.
tg_reset
hook_reset
expect_eq "a failing probe is one POST" 1 "$(wait_posts check 1 120)"
body=$(posts check | jq -c '.[0]')
echo "  probe alert: $body"
expect_eq "the POST names the environment" "$ENVN" "$(printf '%s' "$body" | jq -r '.environment')"
expect_eq "the POST names the check" probe:broken "$(printf '%s' "$body" | jq -r '.name')"
expect_eq "the POST gives the state" critical "$(printf '%s' "$body" | jq -r '.state')"
expect_contains "the POST gives the detail" "$(printf '%s' "$body" | jq -r '.detail')" "refused"
case $(printf '%s' "$body" | jq -r '.since') in
  20??-??-??T*Z) pass "the POST gives the time the state began" ;;
  *) fail "the POST gives the time the state began" "got $(printf '%s' "$body" | jq -r '.since')" ;;
esac
expect_eq "the probe is one Telegram message too" 1 "$(tg_messages | jq 'length')"
expect_eq "the webhook text is the Telegram text" "$(tg_messages | jq -r '.[0].text')" "$(printf '%s' "$body" | jq -r '.text')"
expect_contains "the text says which check failed" "$(printf '%s' "$body" | jq -r '.text')" "probe:broken"

# 3. A deploy that fails is one POST of kind deploy and one Telegram message.
tg_reset
hook_reset
alerts_manifest failed false | write_manifest
out=$(adm deploy web --version v2 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "the deploy with a failing health check fails" 1 "$code" "$out"
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
expect_eq "the failed deploy is one Telegram message" 1 "$(tg_messages | jq '[.[] | select(.text | contains("berthy diagnostics"))] | length')"
expect_eq "the webhook text is the Telegram text of the deploy" "$(tg_messages | jq -r '[.[] | select(.text | contains("berthy diagnostics"))][0].text')" "$(printf '%s' "$body" | jq -r '.text')"

# 4. alerts.deploys: all sends the success to both channels too.
tg_reset
hook_reset
alerts_manifest all true | write_manifest
out=$(adm deploy web --version v2 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v2 with deploys: all" 0 "$code" "$out"
expect_eq "deploys: all sends the success as one POST" 1 "$(wait_posts deploy 1 20)"
expect_eq "the success POST says succeeded" succeeded "$(posts deploy | jq -r '.[0].state')"
expect_eq "the success POST gives the version" v2 "$(posts deploy | jq -r '.[0].detail')"

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
  [ "$(tg_messages | jq 'length')" -ge 1 ] && grep -q 'deploy alert not sent' "$LOG/agent.log" && break
  sleep 0.25
done
expect_eq "Telegram still gets the message" 1 "$(tg_messages | jq 'length')"
line=$(grep 'deploy alert not sent' "$LOG/agent.log" | tail -n 1)
echo "  log line: $line"
expect_contains "the agent logs the webhook failure" "$line" "webhook: webhook unavailable"
start_hook || fail "the fake webhook restarts" "it did not answer"

# 6. The doctor checks that the secrets of every channel are stored.
out=$(adm doctor --json 2>&1)
expect_eq "doctor shows the webhook check as ok" true "$(printf '%s' "$out" | jq -r '.checks[] | select(.name == "webhook") | .ok')"
expect_eq "doctor shows the Telegram check as ok" true "$(printf '%s' "$out" | jq -r '.checks[] | select(.name == "telegram") | .ok')"
adm secret rm ALERT_WEBHOOK_HEADERS >/dev/null 2>&1
out2=$(adm doctor --json 2>&1)
expect_eq "doctor shows the webhook check failing when a secret is gone" false "$(printf '%s' "$out2" | jq -r '.checks[] | select(.name == "webhook") | .ok')"
expect_contains "doctor says which secret" "$out2" "webhook headers secret missing"
out3=$(adm alerts test 2>&1); code=$?
expect_code "alerts test fails when a channel cannot send" 1 "$code" "$out3"
expect_contains "alerts test still sends to Telegram" "$out3" "telegram: ok"
expect_contains "alerts test says why the webhook cannot send" "$out3" "webhook: failed: webhook headers secret missing"

# 7. The URL, the header value and the bot token appear nowhere.
checks=$(adm checks 2>&1)
for secret in "s3cr3t-e2e-token" "e2e-hook-header-value" "$(cat "$RUN/tg-token")"; do
  expect_not_contains "the logs hold no secret" "$(cat "$LOG"/agent.log "$LOG"/webhook.log)" "$secret"
  expect_not_contains "checks holds no secret" "$checks" "$secret"
  expect_not_contains "doctor holds no secret" "$out$out2$out3" "$secret"
done

finish
