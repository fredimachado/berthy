#!/bin/bash
# 31-hand-update proves a deploy over a service that someone just scaled and changed by hand with docker service update. The
# hand update makes Swarm start a second replica and replace the first, and the deploy, which asks for one replica, then
# keeps the replica that started last: it runs in slot 2 of 1. The deploy counts replicas, not slot numbers, so it ends
# healthy without waiting for its deadline, and it does so each time.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

svc="$PROJECT-$ENVN-web-main"
base_manifest | write_manifest

out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy web v1" 0 "$code" "$out"

for round in 1 2 3; do
  d service update --detach --replicas 2 --env-add "DRIFT_X=hand$round" "$svc" >/dev/null 2>&1; code=$?
  expect_code "round $round: change the service by hand" 0 "$code"
  start=$SECONDS
  out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
  expect_code "round $round: deploy right after the hand update" 0 "$code" "$out"
  expect_contains "round $round: the deploy succeeded" "$out" "state: succeeded"
  # The health deadline is minutes; a deploy that counted slots would have waited all of it.
  if [ $((SECONDS - start)) -lt 60 ]; then pass "round $round: the deploy did not wait for a deadline"; else fail "round $round: the deploy took $((SECONDS - start)) s"; fi
  replicas=$(d service inspect "$svc" --format '{{.Spec.Mode.Replicated.Replicas}}')
  expect_eq "round $round: the deploy restored one replica" "1" "$replicas"
done

finish
