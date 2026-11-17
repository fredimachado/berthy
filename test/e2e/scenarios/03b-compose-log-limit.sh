#!/bin/bash
# 03b-compose-log-limit proves the doctor's warning about Compose services without a log size limit. Berthy does not rewrite
# a Compose file, so a service that sets no max-size logs without a limit. The doctor names that service and not the one
# that caps its logs, and the warning leaves the doctor's exit code at 0. The services run the e2e web image by its digest,
# with a command that only waits, because the fixture has no internet access to pull other images.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

# write_compose: the Compose file next to the manifest. "bare" sets no logging; "capped" sets json-file with max-size 5m.
write_compose() {
  cat > "$MAN/compose.yml" <<YML
services:
  bare:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sleep", "3600"]
  capped:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sleep", "3600"]
    logging:
      driver: json-file
      options:
        max-size: 5m
YML
}

write_compose
write_manifest <<YML
project: $PROJECT
network: $NETWORK
apps:
  shop:
    kind: compose
    file: compose.yml
YML

out=$(adm deploy shop --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy the Compose app" 0 "$code" "$out"
running=$(d ps --filter status=running --filter label=com.docker.compose.project=e2e-shop --format '{{.Label "com.docker.compose.service"}}')
expect_contains "the bare service runs" "$running" "bare"
expect_contains "the capped service runs" "$running" "capped"

out=$(adm doctor 2>&1); code=$?
expect_code "doctor exits 0 with the warning" 0 "$code" "$out"
expect_contains "doctor warns about services without a log limit" "$out" "compose_log_limit"
expect_contains "the warning names the service without a limit" "$out" "e2e-shop/bare"
expect_not_contains "the warning leaves out the capped service" "$out" "e2e-shop/capped"

# The JSON answer: the check is not ok and not critical, so it cannot fail the doctor. Spaces are dropped for the match.
json=$(adm doctor --json 2>&1); code=$?
compact=${json//[[:space:]]/}
expect_code "doctor --json exits 0 with the warning" 0 "$code" "$json"
expect_contains "the JSON check is a warning, not critical" "$compact" '"name":"compose_log_limit","ok":false,"critical":false'
expect_contains "the JSON warning names the service without a limit" "$compact" 'e2e-shop/bare'
expect_not_contains "the JSON warning leaves out the capped service" "$compact" 'e2e-shop/capped'

finish
