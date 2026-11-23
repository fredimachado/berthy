#!/bin/bash
# 39-env-default: omitted -e reads BERTHY_ENV; a given -e wins; an empty or invalid value is refused.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"

out=$(BERTHY_ENV=$ENVN "$BIN" ps --config "$RUN/admin.json" 2>&1); code=$?
expect_code "ps from BERTHY_ENV" 0 "$code" "$out"

out=$(BERTHY_ENV=nope "$BIN" ps -e "$ENVN" --config "$RUN/admin.json" 2>&1); code=$?
expect_code "given -e wins over BERTHY_ENV" 0 "$code" "$out"

out=$(BERTHY_ENV=$ENVN "$BIN" ps -e "" --config "$RUN/admin.json" 2>&1); code=$?
expect_code "explicit empty -e is refused" 2 "$code" "$out"

out=$(BERTHY_ENV='Bad!' "$BIN" ps --config "$RUN/admin.json" 2>&1); code=$?
expect_code "invalid BERTHY_ENV is refused" 2 "$code" "$out"
expect_contains "invalid BERTHY_ENV names the variable" "$out" "BERTHY_ENV is not a valid environment"

out=$(env -u BERTHY_ENV "$BIN" ps --config "$RUN/admin.json" 2>&1); code=$?
expect_code "unset BERTHY_ENV still needs -e" 2 "$code" "$out"
expect_contains "unset keeps the old message" "$out" "remote commands require -e"

out=$(BERTHY_ENV=$ENVN "$BIN" lock acquire -m e2e --config "$RUN/admin.json" 2>&1); code=$?
expect_code "lock acquire from BERTHY_ENV" 0 "$code" "$out"
out=$(BERTHY_ENV=$ENVN "$BIN" lock release --config "$RUN/admin.json" 2>&1); code=$?
expect_code "lock release from BERTHY_ENV" 0 "$code" "$out"

finish
