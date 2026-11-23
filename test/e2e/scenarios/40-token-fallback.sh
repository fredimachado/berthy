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

# With BERTHY_TOKEN_STORE=none the harness has no store: --save-token keeps the entry and says so.
save_cfg="$RUN/save-token.json"
printf '%s\n' "$BERTHY_ADMIN_TOKEN" | "$BIN" context add --project "$PROJECT" -e "$ENVN" --url "http://$AGENT_LISTEN" --token-env BERTHY_SAVE_TOKEN --config "$save_cfg" --save-token >"$RUN/save-token.out" 2>"$RUN/save-token.err"
code=$?
out=$(cat "$RUN/save-token.out" "$RUN/save-token.err")
expect_code "context add --save-token without a store" 1 "$code" "$out"
expect_contains "save-token keeps the entry" "$out" "added $PROJECT/$ENVN"
expect_contains "save-token says not saved" "$out" "not saved: no credential store on this machine; set \$BERTHY_SAVE_TOKEN instead"
expect_not_contains "save-token never prints the token" "$out" "$BERTHY_ADMIN_TOKEN"
finish
