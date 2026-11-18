#!/bin/bash
# 11-deploy-reports proves that people hear about deploys they did not run, and that the telemetry backend sees every operation.
#
# Telegram: with alerts.deploys unset (failed), a successful deploy sends nothing and a forced failed deploy sends
# exactly one message with the environment, the app, the version, who asked, the public reason and the diagnostics command.
# `deploys: all` also sends one for a success, and `deploys: none` sends none even for a failure. The manifest of the request
# decides, not the one applied before.
#
# Telemetry: each operation, of every kind, shows up in the OTLP backend as two log records (a start and an end) that carry the
# environment, app, kind, version, digest, state and duration. The fakes stand in for Telegram and for the OTLP backend.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

# reports_manifest DEPLOYS HEALTH_CMD: the base manifest with alerts and telemetry, and a role whose health check runs
# HEALTH_CMD ("true" passes, "false" never does). DEPLOYS is the value of alerts.deploys, or empty to leave it out.
reports_manifest() {
  cat <<YML
project: $PROJECT
network: $NETWORK
alerts:
  telegram:
    bot_token_secret: TELEGRAM_BOT_TOKEN
    chat_id: "1"
YML
  [ -n "$1" ] && printf '  deploys: %s\n' "$1"
  cat <<YML
telemetry:
  otlp_endpoint: http://$OTLP_LISTEN
  headers_secret: OTLP_HEADERS
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

# message_count: how many messages the fake Telegram holds.
message_count() { tg_messages | jq 'length'; }

# wait_messages N: waits up to 20 s for N messages, then 3 s more so a surplus one would show, and prints the count.
wait_messages() {
  local i
  for i in $(seq 1 80); do
    [ "$(message_count)" -ge "$1" ] && break
    sleep 0.25
  done
  sleep 3
  message_count
}

# operation_id OUTPUT: the id of the operation a deploy or restart printed.
operation_id() { printf '%s\n' "$1" | grep -oE '\b[0-9a-f]{32}\b' | head -n 1; }

# log_records ID: the records of the operation in the OTLP summary, one compact JSON object per line.
log_records() { jq -c --arg id "$1" '.logs[] | select(.attributes["berthy.operation.id"] == $id)' "$RUN/otlp-summary.json"; }

# wait_records ID N: waits up to 20 s for N records of the operation.
wait_records() {
  local i
  for i in $(seq 1 80); do
    [ "$(log_records "$1" | wc -l)" -ge "$2" ] && break
    sleep 0.25
  done
}

adm secret set TELEGRAM_BOT_TOKEN < "$RUN/tg-token" >/dev/null
printf 'Authorization=Bearer e2e-otlp' | adm secret set OTLP_HEADERS >/dev/null

# deploys unset means failed: a success is silent.
reports_manifest "" true | write_manifest
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1" 0 "$code" "$out"
out=$(adm deploy web --version v2 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v2" 0 "$code" "$out"
ok_id=$(operation_id "$out")
expect_eq "a successful deploy sends no message by default" 0 "$(wait_messages 1)"

# A forced failure: the role's health check never passes.
tg_reset
reports_manifest "" false | write_manifest
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "the deploy with a failing health check fails" 1 "$code" "$out"
fail_id=$(operation_id "$out")
expect_eq "a failed deploy sends exactly one message" 1 "$(wait_messages 1)"
text=$(tg_messages | jq -r '.[0].text // ""')
echo "  message: $(printf '%s' "$text" | tr '\n' '|')"
expect_contains "the message names the environment and the app" "$text" "$ENVN: web"
expect_contains "the message names the version" "$text" "v1"
expect_contains "the message names who asked" "$text" "by admin"
expect_contains "the message says the deploy did not succeed" "$text" "🔴"
expect_contains "the message gives the diagnostics command" "$text" "berthy diagnostics $fail_id -e $ENVN"
expect_not_contains "the message holds no application output" "$text" "Traceback"
expect_eq "the message goes to the configured chat" 1 "$(tg_messages | jq -r '.[0].chat_id')"

# deploys: all also reports a success.
tg_reset
reports_manifest all true | write_manifest
out=$(adm deploy web --version v2 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v2 with deploys: all" 0 "$code" "$out"
all_id=$(operation_id "$out")
expect_eq "deploys: all sends one message for a success" 1 "$(wait_messages 1)"
text=$(tg_messages | jq -r '.[0].text // ""')
echo "  message: $(printf '%s' "$text" | tr '\n' '|')"
expect_contains "the success message" "$text" "✅ $ENVN: web v2 deployed by admin in "

# deploys: none sends nothing, even for a failure, and the request's manifest decides over the one applied before.
tg_reset
reports_manifest none false | write_manifest
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "a failing deploy with deploys: none fails" 1 "$code" "$out"
expect_eq "deploys: none sends no message" 0 "$(wait_messages 1)"

# Telemetry: two records for each operation, of every kind.
wait_records "$ok_id" 2
records=$(log_records "$ok_id")
expect_eq "the successful deploy has a start record and an end record" 2 "$(printf '%s\n' "$records" | wc -l)"
start=$(printf '%s\n' "$records" | jq -c 'select(.body == "deploy started")')
end=$(printf '%s\n' "$records" | jq -c 'select(.body == "deploy succeeded")')
echo "  start: $start"
echo "  end:   $end"
expect_eq "the start record says running" running "$(printf '%s' "$start" | jq -r '.attributes["berthy.operation.state"]')"
expect_eq "the end record says succeeded" succeeded "$(printf '%s' "$end" | jq -r '.attributes["berthy.operation.state"]')"
for rec in "$start" "$end"; do
  expect_eq "a record names the environment" "$ENVN" "$(printf '%s' "$rec" | jq -r '.attributes["berthy.environment"]')"
  expect_eq "a record names the app" web "$(printf '%s' "$rec" | jq -r '.attributes["berthy.app"]')"
  expect_eq "a record names the kind" deploy "$(printf '%s' "$rec" | jq -r '.attributes["berthy.operation.kind"]')"
  expect_eq "a record names the version" v2 "$(printf '%s' "$rec" | jq -r '.attributes["berthy.version"]')"
done
expect_eq "the end record carries the image digest" "$WEB_V2_DIGEST" "$(printf '%s' "$end" | jq -r '.attributes["berthy.image.digest"]')"
case $(printf '%s' "$end" | jq -r '.attributes["berthy.operation.duration_ms"]') in
  '' | null | *[!0-9]*) fail "the end record carries the duration in milliseconds" "got $(printf '%s' "$end" | jq -r '.attributes["berthy.operation.duration_ms"]')" ;;
  *) pass "the end record carries the duration in milliseconds" ;;
esac
expect_eq "the failed deploy has two records too" 2 "$(wait_records "$fail_id" 2; log_records "$fail_id" | wc -l)"
echo "  failed records: $(log_records "$fail_id" | paste -sd' ' -)"
expect_eq "a failed end is a warning" WARN "$(log_records "$fail_id" | jq -r 'select(.body != "deploy started") | .severity')"

out=$(adm restart web 2>&1); code=$?
expect_code "restart" 0 "$code" "$out"
restart_id=$(operation_id "$out")
wait_records "$restart_id" 2
expect_eq "a restart has two records as well" "restart succeeded,restart started" "$(log_records "$restart_id" | jq -r '.body' | sort -r | paste -sd, -)"
sleep 1
expect_eq "the restart sent no message" 0 "$(message_count)"

finish
