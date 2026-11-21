#!/bin/bash
# 07-min-version proves the manifest's min_version with release-stamped binaries of this tree. The CLI is stamped v0.9.0 and
# the agent v0.8.0. The agent of 6acd0ce, stamped v0.8.0, stands in for a release before projects: it serves only
# /v1/environments/<environment>/. The checks, in order:
#   a. config validate refuses a min_version above the CLI's release, and needs no server;
#   b. the CLI refuses to send a deploy to an agent older than min_version, and records nothing;
#   c. the agent refuses the same request sent straight to it, so a request from another client is refused too;
#   e. a development CLI sends the request and the agent refuses it, and a deploy with no min_version still works there;
#   d. the agent of 6acd0ce, which serves only /v1/environments/, answers 404 to every request of this CLI. The CLI asks the
#      older path, finds the agent answers it, and says the agent runs an older release, with and without min_version. With
#      min_version the lookup of the agent's release fails the same way, so the CLI no longer refuses before sending: the
#      request goes out and the 404 carries the hint.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

CLI090=$E2E_ROOT/bin/berthy-v090
AGENT080=$E2E_ROOT/bin/berthy-agent-v080
OLD080=$E2E_ROOT/bin/berthy-old-v080
OLD_SRC=$E2E_ROOT/src-6acd0ce
# The old agent's configuration, in its release's layout, with a state directory and a key of its own (see old_agent_state).
OLD_CFG=$RUN/old/agent.json

# stamped OUT VERSION TREE: builds TREE's cmd/berthy stamped with VERSION, or fails and keeps no partial file.
stamped() {
  local out=$1 version=$2 tree=$3
  if (cd "$tree" && "$GO" build -buildvcs=false -ldflags "-X berthy/internal/version.Version=$version" -o "$out.new" ./cmd/berthy); then
    mv -f "$out.new" "$out"
  else
    rm -f "$out.new"
    echo "build of $version from $tree failed" >&2
    return 1
  fi
}

# start_with BINARY [CONFIG]: starts BINARY as the agent with CONFIG (the fixture's when none is given) and waits until /healthz
# answers, as start_agent does for the fixture's own binary.
start_with() {
  local bin=$1 cfg=${2:-$RUN/agent.json} i
  spawn "$RUN/agent.pid" "$LOG/agent.log" "$bin" agent --config "$cfg"
  for i in $(seq 1 120); do
    if curl_local -f -o /dev/null "http://$AGENT_LISTEN/healthz" 2>/dev/null; then
      return 0
    fi
    if [ -s "$RUN/agent.pid" ] && ! owned "$RUN/agent.pid" "$bin"; then
      break
    fi
    sleep 0.25
  done
  echo "agent $bin did not answer (logs in $LOG/agent.log)" >&2
  return 1
}

# restart_with BINARY [CONFIG]: stops the running agent and starts BINARY in its place.
restart_with() {
  stop_agent
  start_with "$1" "${2:-}"
}

# old_agent_state: gives the old agent a state directory and a key of its own, a configuration in the layout of its release (one
# environment, no projects). The agent of this tree has migrated the fixture's state
# to schema 14, which the agent of 6acd0ce (schema 13) refuses to open, so the old agent must never see that state.
old_agent_state() {
  mkdir -p "$RUN/old/state" && chmod 700 "$RUN/old/state" || return 1
  head -c 32 /dev/urandom > "$RUN/old/state.key" && chmod 600 "$RUN/old/state.key" || return 1
  printf '{"environment":"%s","listen":"%s","docker_socket":"%s","state_dir":"%s","key_file":"%s","telegram_api_url":"http://%s","compose_command":["%s"],"tokens":[%s]}\n' \
    "$ENVN" "$AGENT_LISTEN" "$SOCK" "$RUN/old/state" "$RUN/old/state.key" "$TG_LISTEN" "$COMPOSE_BIN" "$(cat "$RUN/admin-entry.json")" > "$OLD_CFG" \
    && chmod 600 "$OLD_CFG"
}

# manifest_with_min VERSION: the base manifest with min_version VERSION, or with none when VERSION is empty.
manifest_with_min() {
  if [ -n "$1" ]; then
    echo "min_version: $1"
  fi
  base_manifest
}

# The binaries: this tree at v0.9.0 and at v0.8.0, and the agent of 6acd0ce at v0.8.0 (the export is the committed tree).
stamped "$CLI090" v0.9.0 "$SRC" || exit 1
stamped "$AGENT080" v0.8.0 "$SRC" || exit 1
rm -rf "$OLD_SRC" && mkdir -p "$OLD_SRC" || exit 1
git -c safe.directory="$REPO" -C "$REPO" archive 6acd0ce | tar -x -C "$OLD_SRC" || { echo "cannot export 6acd0ce" >&2; exit 1; }
find "$OLD_SRC" -type f \( -name '*.go' -o -name go.mod -o -name go.sum \) -exec sed -i 's/\r$//' {} + || exit 1
stamped "$OLD080" v0.8.0 "$OLD_SRC" || exit 1

# a. config validate: the CLI at v0.9.0 refuses a min_version above its own release, and it needs no server.
manifest_with_min 1.0.0 | write_manifest || exit 1
out=$("$CLI090" config validate -e "$ENVN" --manifest-dir "$MAN" 2>&1); code=$?
expect_eq "config validate refuses a min_version above the CLI's release: exit" 1 "$code"
expect_contains "config validate names both releases" "$out" "this manifest needs Berthy 1.0.0 or newer; this CLI is v0.9.0"
manifest_with_min 0.9.0 | write_manifest || exit 1
out=$("$CLI090" config validate -e "$ENVN" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "config validate accepts a min_version the CLI meets" 0 "$code" "$out"

# b. The CLI refuses to send the deploy to the agent at v0.8.0, and nothing is recorded.
restart_with "$AGENT080" || exit 1
manifest_with_min 0.9.0 | write_manifest || exit 1
out=$("$CLI090" deploy web --version v1 -e "$ENVN" --config "$RUN/admin.json" --manifest-dir "$MAN" 2>&1); code=$?
expect_eq "CLI refuses the deploy to an older agent: exit" 1 "$code"
expect_contains "CLI names the agent's release and what to do" "$out" "this manifest needs Berthy 0.9.0 or newer; the agent of $ENVN runs v0.8.0: upgrade it with berthy setup"
out=$(adm history web 2>&1)
expect_not_contains "history records no release" "$out" "$WEB_V1_DIGEST"
services=$(d service ls -q --filter label=berthy.environment=$ENVN)
expect_eq "no service was created" "" "$services"

# c. The agent's own check: the same deploy sent straight to the agent, with the merged manifest the CLI would send.
manifest_json=$("$BIN" config show -e "$ENVN" --manifest-dir "$MAN" --json 2>&1); code=$?
expect_code "config show prints the merged manifest" 0 "$code" "$manifest_json"
body="{\"app\":\"web\",\"version\":\"v1\",\"request_id\":\"e2e-07-agent-check\",\"manifest\":$manifest_json}"
resp=$(curl_local -w '\n%{http_code}' -X POST -H "Authorization: Bearer $BERTHY_ADMIN_TOKEN" -H 'Content-Type: application/json' --data-binary "$body" "http://$AGENT_LISTEN/v1/projects/$PROJECT/environments/$ENVN/deploys")
status=${resp##*$'\n'}
reply=${resp%$'\n'*}
expect_eq "agent refuses a request from another client: HTTP" 400 "$status"
expect_contains "agent names its own release" "$reply" "this manifest needs Berthy 0.9.0 or newer; this agent runs v0.8.0: upgrade it with berthy setup"

# e. A development CLI skips its own checks, sends the request, and the agent refuses it. With no min_version the same agent
#    deploys, so the refusals are the checks and not a broken agent.
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_eq "dev CLI: the deploy is refused by the agent: exit" 1 "$code"
expect_contains "dev CLI: the agent names its release" "$out" "agent returned HTTP 400: this manifest needs Berthy 0.9.0 or newer; this agent runs v0.8.0: upgrade it with berthy setup"
base_manifest | write_manifest || exit 1
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "a manifest without min_version deploys to the v0.8.0 agent" 0 "$code" "$out"
spec=$(service_spec "$PROJECT-web-main")
expect_contains "the service runs the pinned v1 digest" "$spec" "$WEB_V1_DIGEST"

# d. The agent of 6acd0ce serves only /v1/environments/<environment>/, so it answers 404 to the CLI's project URL. The CLI says
#    the agent runs an older release. It runs on a state directory of its own, never the fixture's. With min_version the CLI
#    cannot read the agent's release, so it sends the request, and the same hint comes back. An agent that serves project URLs
#    and refuses a request as "invalid request", which also gets the older-agent hint, is covered by the unit tests
#    (TestOlderAgentHintNamesAnOlderReleaseOnly, TestInvalidRequestIsTheAgentsDecodeRefusal): no agent of 6acd0ce reaches it.
old_agent_state || { echo "cannot give the old agent its own state" >&2; exit 1; }
restart_with "$OLD080" "$OLD_CFG" || exit 1
manifest_with_min "" | write_manifest || exit 1
out=$("$CLI090" deploy web --version v1 -e "$ENVN" --config "$RUN/admin.json" --manifest-dir "$MAN" 2>&1); code=$?
expect_eq "old agent: the deploy is refused: exit" 1 "$code"
expect_contains "old agent: it answers 404" "$out" "agent returned HTTP 404"
expect_contains "old agent: the CLI names the older release" "$out" "the agent runs an older release than this CLI; upgrade it with berthy setup"
manifest_with_min 0.9.0 | write_manifest || exit 1
out=$("$CLI090" deploy web --version v1 -e "$ENVN" --config "$RUN/admin.json" --manifest-dir "$MAN" 2>&1); code=$?
expect_eq "old agent with min_version: the deploy is refused: exit" 1 "$code"
expect_contains "old agent with min_version: the CLI names the older release" "$out" "the agent runs an older release than this CLI; upgrade it with berthy setup"
out=$("$CLI090" ps -e "$ENVN" --config "$RUN/admin.json" 2>&1); code=$?
expect_eq "old agent: ps is refused: exit" 1 "$code"
expect_contains "old agent: ps names the older release" "$out" "the agent runs an older release than this CLI; upgrade it with berthy setup"

finish
