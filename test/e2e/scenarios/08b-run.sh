#!/bin/bash
# 08b-run proves `berthy run` on the real Swarm. The command runs in a new container of the applied release with the app's
# environment, the BERTHY_* variables of its role and the role's memory limit, its output and exit code come back through
# the CLI, and the container is gone afterwards. A failing command fails the run, a run past --timeout is stopped and removed
# and leaves the deploy lock free, a dry run shows the image and the role and runs nothing, and on a protected environment an
# agent token's run is held until a person approves it.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

# run_manifest [protected]: the base manifest with an env value, a memory limit on the role, and optionally protected: true.
run_manifest() {
  [ "${1:-}" = protected ] && echo "protected: true"
  cat <<YML
project: $PROJECT
network: $NETWORK
apps:
  web:
    kind: image
    image: $REG/e2e/web
    env:
      plain:
        GREETING: hi
    roles:
      main:
        resources:
          memory: 64M
        health:
          test: ["CMD", "true"]
          interval: 2s
          timeout: 2s
          start_period: 1s
          retries: 3
YML
}

# run_containers: the ids of the run containers left on the daemon, one per line.
run_containers() { d ps -aq --filter label=berthy.environment=$ENVN --filter label=berthy.oneoff=run; }

run_manifest | write_manifest
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1" 0 "$code" "$out"

out=$(adm run web -- sh -c 'echo $GREETING $BERTHY_ROLE $BERTHY_VERSION; cat /sys/fs/cgroup/memory.max' 2>&1); code=$?
expect_code "run exits 0" 0 "$code" "$out"
expect_contains "the command sees the app's env, its role and the tag" "$out" "hi main v1"
expect_contains "the container has the role's memory limit" "$out" "67108864"
expect_eq "the run container is gone afterwards" "" "$(run_containers)"

out=$(adm run web -- sh -c 'echo before failing; exit 3' 2>&1); code=$?
expect_code "a failing command exits non-zero" 1 "$code" "$out"
expect_contains "its output is shown" "$out" "before failing"
expect_contains "the record says it failed" "$out" "failed"
expect_contains "the failure says the run returned nonzero" "$out" "run returned nonzero"
expect_eq "the container of a failed run is gone too" "" "$(run_containers)"

start=$SECONDS
out=$(adm run web --timeout 2s -- sleep 30 2>&1); code=$?
elapsed=$((SECONDS - start))
expect_code "a run past its timeout fails" 1 "$code" "$out"
expect_contains "the failure says it timed out" "$out" "timed out"
if [ "$elapsed" -lt 20 ]; then pass "the timed-out run ended in ${elapsed}s"; else fail "the timed-out run ended in time" "took ${elapsed}s"; fi
expect_eq "the timed-out container is stopped and removed" "" "$(run_containers)"

out=$(adm deploy web --version v2 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "a deploy right after succeeds: the lock was released" 0 "$code" "$out"

before=$(adm audit 2>&1)
out=$(adm run web --dry-run -- echo x 2>&1); code=$?
expect_code "a dry run plans" 0 "$code" "$out"
expect_contains "the plan names the v2 digest" "$out" "${WEB_V2_DIGEST#sha256:}"
expect_contains "the plan names the role" "$out" "main"
expect_contains "the plan names the command" "$out" "echo"
expect_eq "the dry run records nothing" "$before" "$(adm audit 2>&1)"
expect_eq "the dry run created no container" "" "$(run_containers)"

out=$(adm run web --timeout 25h -- true 2>&1); code=$?
expect_code "a timeout above 24h is refused" 1 "$code" "$out"
out=$(adm run web 2>&1); code=$?
expect_code "run without a command is refused" 2 "$code" "$out"

# Approval. An agent token needs scope admin to run, so the scenario makes one through the API.
make_ciadmin || fail "make an agent admin token" "token create failed"

run_manifest protected | write_manifest
out=$(adm deploy web --version "$WEB_V2_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "protect the environment with a digest deploy" 0 "$code" "$out"

out=$(cia run web -- echo approved 2>&1); code=$?
expect_code "an agent's run is held for approval" 3 "$code" "$out"
expect_contains "the held plan names the command" "$out" "echo"
expect_contains "the held plan names the image" "$out" "${WEB_V2_DIGEST#sha256:}"
expect_eq "nothing ran while it was held" "" "$(run_containers)"
id=$(printf '%s\n' "$out" | sed -n 's/^held for approval: \([^ ]*\) .*/\1/p')

out=$(adm approvals 2>&1); code=$?
expect_code "approvals lists" 0 "$code" "$out"
expect_contains "the pending run is listed" "$out" "run web"

out=$(adm approve "$id" 2>&1); code=$?
expect_code "approve runs it" 0 "$code" "$out"
expect_contains "the approved run's output is shown" "$out" "approved"
expect_eq "the approved run's container is gone" "" "$(run_containers)"

finish
