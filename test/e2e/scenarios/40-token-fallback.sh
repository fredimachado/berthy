#!/bin/bash
set -uo pipefail
. "$(dirname "$0")/../lib.sh"

out=$(unset BERTHY_ADMIN_TOKEN; "$BIN" ps -e "$ENVN" --config "$RUN/admin.json" 2>&1); code=$?
expect_code "ps without a token" 1 "$code" "$out"
expect_contains "ps explains the missing store" "$out" "no token for $PROJECT/$ENVN: its token variable is empty and this machine has no credential store"
out=$("$BIN" ps -e "$ENVN" --config "$RUN/admin.json" 2>&1); code=$?
expect_code "ps with the variable set" 0 "$code" "$out"
out=$("$BIN" context list --config "$RUN/admin.json" 2>&1); code=$?
expect_code "context list" 0 "$code" "$out"
expect_contains "list reports unavailable store" "$out" "unknown"
out=$(BERTHY_TOKEN_STORE=nonee "$BIN" ps -e "$ENVN" --config "$RUN/admin.json" 2>&1); code=$?
expect_code "invalid store setting" 2 "$code" "$out"
expect_contains "invalid store setting is explained" "$out" 'BERTHY_TOKEN_STORE must be "none" or unset'
finish
