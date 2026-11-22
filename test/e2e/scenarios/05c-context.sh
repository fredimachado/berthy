#!/bin/bash
# 05c-context proves berthy context add, list and remove against a copy of the harness's CLI configuration. The three
# commands edit the file on this machine and never contact the agent. After a remove, ps cannot find the environment;
# after an add, it can. add creates a missing file (mode 600) through BERTHY_CONFIG, and refuses https with exit 2.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"

cp "$RUN/admin.json" "$RUN/ctx.json" || exit 1

out=$("$BIN" context list --config "$RUN/ctx.json" 2>&1); code=$?
expect_code "context list of the harness admin file" 0 "$code" "$out"
expect_contains "list names the project" "$out" "$PROJECT"
expect_contains "list names the environment" "$out" "$ENVN"
expect_contains "list names the agent URL" "$out" "http://$AGENT_LISTEN"
expect_contains "list names the token variable" "$out" "BERTHY_ADMIN_TOKEN"
expect_contains "list says the variable is set" "$out" "yes"
expect_not_contains "list does not print the token" "$out" "$BERTHY_ADMIN_TOKEN"

out=$("$BIN" context remove --project "$PROJECT" -e "$ENVN" --config "$RUN/ctx.json" 2>&1); code=$?
expect_code "context remove" 0 "$code" "$out"
expect_contains "remove names the entry" "$out" "removed $PROJECT/$ENVN from $RUN/ctx.json"

out=$("$BIN" ps -e "$ENVN" --config "$RUN/ctx.json" 2>&1); code=$?
expect_code "ps after remove fails" 1 "$code" "$out"
expect_contains "ps after remove: unknown environment" "$out" "unknown environment"

out=$("$BIN" context add --project "$PROJECT" -e "$ENVN" --url "http://$AGENT_LISTEN" --token-env BERTHY_ADMIN_TOKEN --config "$RUN/ctx.json" 2>&1); code=$?
expect_code "context add restores the entry" 0 "$code" "$out"

out=$("$BIN" ps -e "$ENVN" --config "$RUN/ctx.json" 2>&1); code=$?
expect_code "ps after add answers" 0 "$code" "$out"

fresh="$RUN/fresh/config.json"
out=$(BERTHY_CONFIG="$fresh" "$BIN" context add --project "$PROJECT" -e "$ENVN" --url "http://$AGENT_LISTEN" --token-env BERTHY_ADMIN_TOKEN 2>&1); code=$?
expect_code "add through BERTHY_CONFIG creates a missing file" 0 "$code" "$out"
expect_eq "the new file is private" "600" "$(stat -c '%a' "$fresh")"
out=$(BERTHY_CONFIG="$fresh" "$BIN" agent-info -e "$ENVN" 2>&1); code=$?
expect_code "agent-info through the new file" 0 "$code" "$out"

sum=$(sha256sum "$RUN/ctx.json" | awk '{print $1}')
out=$("$BIN" context add --project "$PROJECT" -e "$ENVN" --url https://example.com --token-env BERTHY_ADMIN_TOKEN --config "$RUN/ctx.json" 2>&1); code=$?
expect_code "add with https is refused with exit 2" 2 "$code" "$out"
expect_eq "https add leaves the file unchanged" "$sum" "$(sha256sum "$RUN/ctx.json" | awk '{print $1}')"

# berthy agent token prints a context add line that another machine can run.
token_out=$("$BIN" agent token --project "$PROJECT" -e "$ENVN" --config "$RUN/agent.json" 2>&1); code=$?
expect_code "agent token with the fixture agent.json" 0 "$code" "$token_out"
add_line=$(printf '%s\n' "$token_out" | sed -n 's/^on the machine that will use it: //p')
expect_contains "agent token prints a context add line" "$add_line" "berthy context add --project $PROJECT -e $ENVN --url \"http://$AGENT_LISTEN\""
token_value=$(printf '%s\n' "$token_out" | sed -n 's/^token: //p')
expect_not_contains "the context add line does not hold the token" "$add_line" "$token_value"
# The printed line starts with "berthy " and quotes the URL for a paste into a shell. Eval it as a shell
# would, with the fixture binary in place of berthy, so the quotes are not literal characters.
out=$(eval "$(printf '%q' "$BIN") ${add_line#berthy } --config $(printf '%q' "$RUN/ctx2.json")" 2>&1); code=$?
expect_code "running the printed context add line" 0 "$code" "$out"
out=$("$BIN" context list --config "$RUN/ctx2.json" 2>&1); code=$?
expect_code "context list of the file the printed line wrote" 0 "$code" "$out"
expect_contains "list shows the project from the printed line" "$out" "$PROJECT"
expect_contains "list shows the environment from the printed line" "$out" "$ENVN"
expect_contains "list shows the agent URL from the printed line" "$out" "http://$AGENT_LISTEN"

finish
