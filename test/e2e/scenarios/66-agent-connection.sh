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

for command in token backup; do
  adm "$command" list --project "$PROJECT" --json >"$RUN/$command.out" 2>"$RUN/$command.err"; code=$?
  expect_code "$command list after stop" 1 "$code"
  expect_eq "$command stderr is empty" "" "$(cat "$RUN/$command.err")"
  if jq -se --arg reason "$want" '. == [{code:"agent_connection_failed",reason:$reason}]' "$RUN/$command.out" >/dev/null; then pass "$command explains refusal"; else fail "$command explains refusal" "$(cat "$RUN/$command.out")"; fi
done

mkdir -p "$RUN/tag-manifest"
cat >"$RUN/tag-manifest/berthy.yml" <<EOF
project: $PROJECT
network: $PROJECT
apps:
  api:
    kind: image
    image: alpine
    roles:
      web: {}
EOF
stamped "$RUN/berthy-release" v0.9.0 "$SRC" || exit 1
for form in single batch; do
  args=(deploy)
  if [ "$form" = single ]; then args+=(api --version stable); else args+=(--version api=stable); fi
  "$RUN/berthy-release" "${args[@]}" -e "$ENVN" --project "$PROJECT" --config "$RUN/admin.json" --manifest-dir "$RUN/tag-manifest" --dry-run --json >"$RUN/$form.out" 2>"$RUN/$form.err"; code=$?
  expect_code "$form tagged deploy after stop" 1 "$code"
  expect_eq "$form stderr is empty" "" "$(cat "$RUN/$form.err")"
  if jq -se --arg reason "$want" '. == [{code:"agent_connection_failed",reason:$reason}]' "$RUN/$form.out" >/dev/null; then pass "$form explains refusal"; else fail "$form explains refusal" "$(cat "$RUN/$form.out")"; fi
done

printf '%s\n' '[{"tool":"list_resources"}]' >"$RUN/mcp-calls.json"
"$MCP_BIN" --command "$BIN mcp -e $ENVN --project $PROJECT --config $RUN/admin.json --manifest-dir $RUN/tag-manifest" --script "$RUN/mcp-calls.json" --out "$RUN/mcp-results.json" --timeout 60s >"$RUN/mcp.out" 2>"$RUN/mcp.err"; code=$?
expect_code "MCP call completes" 0 "$code" "$(cat "$RUN/mcp.err")"
if jq -e --arg reason "$want" '.calls[0].is_error == true and (.calls[0].text | contains($reason))' "$RUN/mcp-results.json" >/dev/null; then pass "MCP explains refusal"; else fail "MCP explains refusal" "$(cat "$RUN/mcp-results.json")"; fi
finish

