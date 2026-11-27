#!/bin/bash
# 56-compose-up-diagnostics checks that a failed Compose start leaves Docker's error in the operation diagnostics.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

missing="/missing-$PROJECT-$ENVN-56"
write_manifest <<YML
project: $PROJECT
network: $NETWORK
apps:
  web:
    kind: compose
    file: compose.yml
YML
cat > "$MAN/compose.yml" <<YML
services:
  web:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["$missing"]
YML

out=$(adm deploy web --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy with a missing entrypoint fails" 1 "$code" "$out"
expect_contains "deploy reports the Compose up failure" "$out" "compose up failed"
expect_not_contains "deploy omits the missing entrypoint" "$out" "$missing"
if ! id=$(printf '%s\n' "$out" | grep -oE '\b[0-9a-f]{32}\b' | head -n 1); then
  fail "deploy prints the operation ID" "$out"
  finish
fi

out=$(adm diagnostics "$id" 2>&1); code=$?
expect_code "read text diagnostics for the failed operation" 0 "$code" "$out"
if [ "$code" -eq 0 ]; then
  expect_contains "text diagnostics name the Compose up entry" "$out" "[1] compose up"
  expect_contains "text diagnostics show the missing entrypoint" "$out" "$missing"
fi

out=$(adm diagnostics "$id" --json 2>&1); code=$?
expect_code "read JSON diagnostics for the failed operation" 0 "$code" "$out"
if [ "$code" -eq 0 ]; then
  if jq -e --arg missing "$missing" '
    .diagnostics | length == 1 and
    (.[0] | .kind == "compose" and .name == "up" and (.logs | contains($missing)))
  ' <<<"$out" >/dev/null; then
    pass "JSON diagnostics show the missing entrypoint in the Compose up logs"
  else
    fail "JSON diagnostics show the missing entrypoint in the Compose up logs" "$out"
  fi
fi

finish
