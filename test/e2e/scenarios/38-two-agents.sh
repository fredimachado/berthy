#!/bin/bash
# One project with an environment on each of two agents that share the Docker daemon (Systock's
# shape: dev and prod on different machines). Each agent lists the other's service as an unmanaged
# workload, because claims are per agent; this scenario does not assert on that.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

EC=${ENVN}c

# Drive berthy from the shared manifest directory with no --project: the manifest picks the project
# and -e picks which agent's context in admin.json.
from_man() {
  local e=$1 c=$2
  shift 2
  (cd "$MAN" && { [ "$c" != deploy ] || record_deploy "$PROJECT" "$e" "$@"; } && "$BIN" "$c" -e "$e" --config "$RUN/admin.json" "$@")
}

start_agent2 || exit 1

# 1. agent-info of each environment answers with its own environment.
out=$(from_man "$ENVN" agent-info --json 2>&1); code=$?
expect_code "agent-info of $ENVN" 0 "$code" "$out"
expect_eq "agent-info of $ENVN names it" "$ENVN" "$(printf '%s' "$out" | jq -r .environment)"

out=$(from_man "$EC" agent-info --json 2>&1); code=$?
expect_code "agent-info of $EC" 0 "$code" "$out"
expect_eq "agent-info of $EC names it" "$EC" "$(printf '%s' "$out" | jq -r .environment)"

# 2. Deploy web v1 to each; Docker shows both services.
out=$(from_man "$ENVN" deploy web --version v1 2>&1); code=$?
expect_code "deploy $ENVN web v1" 0 "$code" "$out"
out=$(from_man "$EC" deploy web --version v1 2>&1); code=$?
expect_code "deploy $EC web v1" 0 "$code" "$out"
services=$(d service ls --format '{{.Name}}')
expect_contains "service of $ENVN" "$services" "$PROJECT-$ENVN-web-main"
expect_contains "service of $EC" "$services" "$PROJECT-$EC-web-main"

# 3. Each history has one release; a later deploy to one does not add a release to the other.
count_releases() { from_man "$1" history web --json 2>/dev/null | jq 'length'; }
expect_eq "history of $ENVN has one release" 1 "$(count_releases "$ENVN")"
expect_eq "history of $EC has one release" 1 "$(count_releases "$EC")"
out=$(from_man "$ENVN" deploy web --version v2 2>&1); code=$?
expect_code "deploy $ENVN web v2" 0 "$code" "$out"
expect_eq "history of $ENVN has two releases after v2" 2 "$(count_releases "$ENVN")"
expect_eq "history of $EC still has one release" 1 "$(count_releases "$EC")"

# 4. A secret of $ENVN is absent from $EC.
out=$(printf 'only-on-first' | from_man "$ENVN" secret set TWO_AGENTS_ONLY 2>&1); code=$?
expect_code "secret set on $ENVN" 0 "$code" "$out"
out=$(from_man "$ENVN" secret list 2>&1); code=$?
expect_code "secret list on $ENVN" 0 "$code" "$out"
expect_contains "secret list on $ENVN names it" "$out" TWO_AGENTS_ONLY
out=$(from_man "$EC" secret list 2>&1); code=$?
expect_code "secret list on $EC" 0 "$code" "$out"
expect_not_contains "secret of $ENVN is absent from $EC" "$out" TWO_AGENTS_ONLY

# 5. Stopping the second agent leaves ps of the first environment working.
stop_agent2
out=$(from_man "$ENVN" ps 2>&1); code=$?
expect_code "ps of $ENVN after stop_agent2" 0 "$code" "$out"
expect_contains "ps of $ENVN still lists web" "$out" "web"

finish
