#!/bin/bash
set -uo pipefail
. "$(dirname "$0")/../lib.sh"

adm ps --project "$PROJECT" >"$RUN/ps-before.out" 2>"$RUN/ps-before.err"; code=$?
expect_code "ps reaches the agent" 0 "$code" "$(cat "$RUN/ps-before.err")"
stop_agent
want="agent connection failed at $AGENT_LISTEN: connection refused"
adm ps --project "$PROJECT" >"$RUN/ps-human.out" 2>"$RUN/ps-human.err"; code=$?
expect_code "ps after stop" 1 "$code"
expect_eq "human stderr explains refusal" "$want" "$(cat "$RUN/ps-human.err")"
expect_eq "human stdout is empty" "" "$(cat "$RUN/ps-human.out")"
adm ps --project "$PROJECT" --json >"$RUN/ps-json.out" 2>"$RUN/ps-json.err"; code=$?
expect_code "ps --json after stop" 1 "$code"
expect_eq "JSON stderr is empty" "" "$(cat "$RUN/ps-json.err")"
if jq -se --arg reason "$want" '. == [{code:"agent_connection_failed", reason:$reason}]' "$RUN/ps-json.out" >/dev/null; then pass "JSON explains refusal"; else fail "JSON explains refusal" "$(cat "$RUN/ps-json.out")"; fi
finish

