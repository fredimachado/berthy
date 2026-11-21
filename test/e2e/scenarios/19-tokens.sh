#!/bin/bash
# 19-tokens proves token management through the API on the real agent. An admin makes a deploy token with no shell on the
# server and no restart; the token deploys the web app, and the list shows it with a last-used time. Revoking it makes its
# next command fail with 401. A name in agent.json cannot be revoked, and neither can the last admin token. An agent
# token cannot make a token. A token that has expired is refused. The audit names the admin for the create and the revoke.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

# as_token TOKEN CMD [ARGS]: berthy as the token, through a CLI configuration that names the token by environment variable.
as_token() {
  local token=$1 c=$2
  shift 2
  printf '{"projects":{"%s":{"environments":{"%s":{"url":"http://%s","token_env":"BERTHY_SCENARIO_TOKEN"}}}}}\n' "$PROJECT" "$ENVN" "$AGENT_LISTEN" > "$RUN/scenario.json"
  BERTHY_SCENARIO_TOKEN=$token "$BIN" "$c" -e "$ENVN" --config "$RUN/scenario.json" "$@"
}

# 1. An admin makes a deploy token. The value is shown once, in the output of the create.
out=$(adm token create deployer --scope deploy 2>&1); code=$?
expect_code "create a deploy token" 0 "$code" "$out"
expect_contains "the create names the token and says it is shown once" "$out" "token deployer (deploy), shown once, store it now: bty.$ENVN."
value=$(printf '%s\n' "$out" | sed -n 's/.*store it now: //p')
second=$(adm token create second --scope deploy --json | sed -n 's/^ *"token": "\(bty\.[^"]*\)".*/\1/p')
case $second in bty.$ENVN.*) pass "the --json create returns the value" ;; *) fail "the --json create returns the value" "no token in the output" ;; esac

# 2. The token deploys web, at once and with no restart of the agent.
out=$(as_token "$value" deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "the new token deploys web" 0 "$code" "$out"

# 3. The list shows it, with its origin and a last-used time. The admin token of agent.json is listed too, from the file.
out=$(adm token list 2>&1); code=$?
expect_code "token list answers" 0 "$code" "$out"
line=$(printf '%s\n' "$out" | awk '$1 == "deployer"')
expect_contains "the list shows the new token as an API token" "$line" "api"
expect_contains "the list shows its scope" "$line" "deploy"
expect_eq "the list shows when it was last used, as an age" "ago" "$(printf '%s\n' "$line" | awk '{print $NF}')"
expect_eq "a token that was never used shows never" "never" "$(printf '%s\n' "$out" | awk '$1 == "second" {print $NF}')"
admin_line=$(printf '%s\n' "$out" | awk '$1 == "admin"')
expect_contains "the admin token of agent.json is listed from the config" "$admin_line" "config"
expect_eq "the ci token is listed as an agent token from the API" "ci yes api" "$(printf '%s\n' "$out" | awk '$1 == "ci" {print $1, $3, $4}')"

# 4. A name is unique, and a name in agent.json cannot be revoked through the API.
out=$(adm token create deployer --scope read 2>&1); code=$?
expect_code "a second token with the same name is refused" 1 "$code" "$out"
expect_contains "the refusal says the name exists" "$out" "exists already"
out=$(adm token revoke admin 2>&1); code=$?
expect_code "the token of agent.json cannot be revoked" 1 "$code" "$out"
expect_contains "the refusal says where it is defined" "$out" "is defined in /etc/berthy/agent.json; remove it there"

# 5. An admin token of the API can be revoked while another admin remains. The admin token of agent.json counts and cannot be
# revoked, so the guard against revoking the last admin never fires here; the agent's unit tests cover it.
out=$(adm token create second-admin --scope admin 2>&1); code=$?
expect_code "create a second admin token" 0 "$code" "$out"
out=$(adm token revoke second-admin 2>&1); code=$?
expect_code "an admin token can be revoked while another admin remains" 0 "$code" "$out"

# 6. An agent token cannot make a token, and a deploy token cannot either.
out=$(ci token create x --scope read 2>&1); code=$?
expect_code "an agent token cannot make a token" 1 "$code" "$out"
expect_contains "the refusal says an agent token cannot manage tokens" "$out" "an agent token cannot manage tokens"
out=$(as_token "$value" token create x --scope read 2>&1); code=$?
expect_code "a deploy token cannot make a token" 1 "$code" "$out"

# 7. Revoke the deploy token: its next command fails with 401.
out=$(adm token revoke deployer 2>&1); code=$?
expect_code "revoke the deploy token" 0 "$code" "$out"
expect_contains "the revoke says so" "$out" "revoked token deployer"
out=$(as_token "$value" history web 2>&1); code=$?
expect_code "the revoked token's next command fails" 1 "$code" "$out"
expect_contains "it fails with 401" "$out" "HTTP 401"
out=$(adm token revoke deployer 2>&1); code=$?
expect_code "revoking it again is refused" 1 "$code" "$out"

# 8. A token that has expired is refused at its next request. The shortest expiry is a minute, so the scenario waits for it.
out=$(adm token create brief --scope read --expires 1m --json 2>&1); code=$?
expect_code "create a token that expires in a minute" 0 "$code" "$out"
brief=$(printf '%s\n' "$out" | sed -n 's/^ *"token": "\(bty\.[^"]*\)".*/\1/p')
out=$(as_token "$brief" history web 2>&1); code=$?
expect_code "the token works before it expires" 0 "$code" "$out"
out=$(adm token list 2>&1)
case $(printf '%s\n' "$out" | awk '$1 == "brief" {print $6}') in
  [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]) pass "the list shows the expiry date" ;;
  *) fail "the list shows the expiry date" "$out" ;;
esac
sleep 62
out=$(as_token "$brief" history web 2>&1); code=$?
expect_code "the token is refused after it expires" 1 "$code" "$out"
expect_contains "it fails with 401" "$out" "HTTP 401"
out=$(adm token list 2>&1)
expect_contains "the list marks it expired" "$(printf '%s\n' "$out" | awk '$1 == "brief"')" "(expired)"

# 9. A dry run changes nothing.
out=$(adm token create ghost --scope read --dry-run 2>&1); code=$?
expect_code "a dry-run create succeeds" 0 "$code" "$out"
expect_contains "it says what would be made" "$out" "would create token ghost (read)"
expect_not_contains "and makes nothing" "$(adm token list 2>&1)" "ghost"

# 10. The audit names the admin for the create and the revoke, and holds no token value.
out=$(adm audit 2>&1); code=$?
expect_code "audit answers" 0 "$code" "$out"
# audit_rows ACTION ACTOR: how many audit records have exactly this action and this actor name.
audit_rows() {
  printf '%s\n' "$out" | awk -v action="action: $1" -v actor="actor_name: $2" '
    BEGIN { RS = "" }
    { a = 0; b = 0; n = split($0, l, "\n")
      for (i = 1; i <= n; i++) { if (l[i] == action) a = 1; if (l[i] == actor) b = 1 }
      if (a && b) c++ }
    END { print c + 0 }'
}
expect_eq "the audit has the create of deployer by admin" "1" "$(audit_rows token_create:deployer admin)"
expect_eq "the audit has the revoke of deployer by admin" "1" "$(audit_rows token_revoke:deployer admin)"
expect_not_contains "the audit holds no token value" "$out" "$value"
if grep -rqF "$value" "$LOG"; then fail "the agent log holds no token value" "found in $LOG"; else pass "the agent log holds no token value"; fi

finish
