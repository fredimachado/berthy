#!/bin/bash
# Serial replica startup and old-task drain each get time to finish.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

nonce=$(openssl rand -hex 6)
repo="$REG/e2e/web"
declare -A attempted=()

cleanup() {
  local code=$? service owned
  for service in "${!attempted[@]}"; do
    owned=$(d service inspect "$service" 2>/dev/null | jq -r --arg project "$PROJECT" --arg env "$ENVN" --arg app "${attempted[$service]}" --arg repo "$repo@" \
      '.[0] | select(.Spec.Labels["berthy.project"] == $project and .Spec.Labels["berthy.environment"] == $env and .Spec.Labels["berthy.app"] == $app and (.Spec.TaskTemplate.ContainerSpec.Image | startswith($repo))) | .ID')
    if [ -n "$owned" ]; then d service rm "$owned" >/dev/null || code=1; fi
  done
  exit "$code"
}
trap cleanup EXIT

begin_app() {
  local inventory role service
  if ! inventory=$(d service ls --format '{{.Name}}'); then
    fail 'list existing fixture services'
    finish
  fi
  for role in "$@"; do
    service="$PROJECT-$ENVN-$app-$role"
    if grep -Fxq "$service" <<<"$inventory"; then
      fail 'fixture service name is available' "$service"
      finish
    fi
  done
  for role in "$@"; do
    attempted["$PROJECT-$ENVN-$app-$role"]=$app
  done
}

manifest() {
  local timeout=${2:-75s}
  cat <<YML
project: $PROJECT
network: $NETWORK
apps:
  $app:
    kind: image
    image: $repo
    deploy:
      deploy_timeout: $timeout
      start_attempts: 1
    roles:
      main:
        replicas: 4
YML
  if [ "$1" = slow ]; then
    cat <<'YML'
        entrypoint: ["sh", "-ec", "date +%s >/tmp/rollout-start; exec nginx -g 'daemon off;'"]
        health:
          test: ["CMD-SHELL", "[ $(( $(date +%s) - $(cat /tmp/rollout-start) )) -ge 35 ]"]
          interval: 2s
          timeout: 2s
          retries: 30
          start_period: 0s
YML
  else
    cat <<'YML'
        health:
          test: ["CMD", "true"]
          interval: 2s
          timeout: 2s
YML
  fi
  if [ "$#" -ge 3 ]; then
    cat <<YML
      sentinel:
        replicas: 1
        health:
          test: ["CMD", "$3"]
          interval: 1s
          timeout: 2s
          retries: 1
YML
  fi
}

applied() {
  python3 - "$RUN/state/state.db" "$ENVN" "$app" <<'PY'
import sqlite3, sys
with sqlite3.connect('file:' + sys.argv[1] + '?mode=ro', uri=True) as db:
    row = db.execute('SELECT releases.digest FROM applied JOIN releases ON releases.id=applied.release_id WHERE applied.environment=? AND applied.app=?', sys.argv[2:]).fetchone()
    print(row[0] if row else '')
PY
}

run_progress() {
  local service out code running slots=
  app="progress-$nonce"
  service="$PROJECT-$ENVN-$app-main"
  begin_app main
  manifest fast | write_manifest
  out=$(adm deploy "$app" --version "$WEB_V1_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
  expect_code 'deploy four fast v1 replicas' 0 "$code" "$out"
  if [ "$code" -ne 0 ]; then finish; fi
  expect_eq 'v1 applied digest' "$repo@$WEB_V1_DIGEST" "$(applied)"

  manifest slow | write_manifest
  out=$(adm deploy "$app" --version "$WEB_V2_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
  printf '%s\n' "$out" >"$LOG/progress-deploy.out"
  expect_code 'serial deploy of four slow v2 replicas' 0 "$code" "$out"
  expect_eq 'v2 applied digest' "$repo@$WEB_V2_DIGEST" "$(applied)"
  expect_eq 'serial Swarm update' 1 "$(d service inspect --format '{{.Spec.UpdateConfig.Parallelism}}' "$service")"
  d service ps --no-trunc "$service" >"$LOG/progress-tasks.txt"
  running=$(d ps -q --filter "label=com.docker.swarm.service.name=$service")
  if [ -n "$running" ]; then
    slots=$(d inspect $running | jq -r --arg image "$repo@$WEB_V2_DIGEST" \
      '.[] | select(.Config.Image == $image and .State.Running and .State.Health.Status == "healthy") | .Config.Labels["com.docker.swarm.task.name"] | split(".")[1]' | sort -u)
  fi
  expect_eq 'four running healthy v2 slots' 4 "$(grep -cE '^[1-9][0-9]*$' <<<"$slots")"
  expect_contains 'deploy lock is free' "$(adm lock status --json 2>&1)" '"locked": false'
}

drain_manifest() {
  local role
  cat <<YML
project: $PROJECT
network: $NETWORK
apps:
  $app:
    kind: image
    image: $repo
    deploy:
      deploy_timeout: 20s
      drain_timeout: 30s
    roles:
YML
  for role in first second; do
    cat <<YML
      $role:
        replicas: 1
        health:
          test: ["CMD", "true"]
          interval: 1s
          timeout: 2s
YML
    if [ "$1" = slow-stop ]; then
      # The base image may use SIGQUIT as its stop signal.
      cat <<'YML'
        entrypoint: ["sh", "-ec", "trap '' TERM QUIT; while :; do sleep 60 & wait $!; done"]
YML
    fi
  done
}

run_drain() {
  local role service out code running live
  app="drain-$nonce"
  begin_app first second
  drain_manifest slow-stop | write_manifest
  out=$(adm deploy "$app" --version "$WEB_V1_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
  expect_code 'deploy two slow-stopping v1 roles' 0 "$code" "$out"
  if [ "$code" -ne 0 ]; then finish; fi
  expect_eq 'v1 applied digest before drain' "$repo@$WEB_V1_DIGEST" "$(applied)"

  drain_manifest fast-stop | write_manifest
  out=$(adm deploy "$app" --version "$WEB_V2_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
  printf '%s\n' "$out" >"$LOG/drain-deploy.out"
  expect_code 'update both slow-stopping roles' 0 "$code" "$out"
  expect_eq 'v2 applied digest after drain' "$repo@$WEB_V2_DIGEST" "$(applied)"
  for role in first second; do
    service="$PROJECT-$ENVN-$app-$role"
    d service ps --no-trunc "$service" >"$LOG/drain-$role-tasks.txt"
    running=$(d ps -q --filter "label=com.docker.swarm.service.name=$service")
    live=
    if [ -n "$running" ]; then
      live=$(d inspect $running | jq -r '.[] | [.Config.Image, .State.Health.Status] | @tsv')
    fi
    expect_eq "$role has one healthy v2 task and no running v1 task" "$repo@$WEB_V2_DIGEST"$'\t'healthy "$live"
  done
  expect_contains 'deploy lock is free after drain' "$(adm lock status --json 2>&1)" '"locked": false'
}

live_generation() {
  d service inspect --format '{{index .Spec.TaskTemplate.ContainerSpec.Labels "berthy.operation"}}' "$1"
}

assert_image_slots() {
  local name=$1 digest=$2 count=$3 service running slots= generation
  service="$PROJECT-$ENVN-$app-$name"
  generation=$(live_generation "$service")
  expect_eq "$name desired replicas" "$count" "$(d service inspect --format '{{.Spec.Mode.Replicated.Replicas}}' "$service")"
  running=$(d ps -q --filter "label=com.docker.swarm.service.name=$service")
  if [ -n "$running" ] && [ -n "$generation" ]; then
    slots=$(d inspect $running | jq -r --arg image "$repo@$digest" --arg gen "$generation" \
      '.[] | select(.Config.Image == $image and .Config.Labels["berthy.operation"] == $gen and .State.Running and .State.Health.Status == "healthy") | .Config.Labels["com.docker.swarm.task.name"] | split(".")[1]' | sort -u)
  fi
  expect_eq "$name running healthy slots of the applied generation" "$count" "$(grep -cE '^[1-9][0-9]*$' <<<"$slots")"
}

seed_slow() {
  local out code
  manifest slow 180s "$@" | write_manifest
  out=$(adm deploy "$app" --version "$WEB_V1_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
  expect_code 'seed four slow v1 replicas' 0 "$code" "$out"
  if [ "$code" -ne 0 ]; then finish; fi
  expect_eq 'slow predecessor digest' "$repo@$WEB_V1_DIGEST" "$(applied)"
}

run_runtime() {
  local kind count out code service before after
  local -a args
  app="runtime-$nonce"
  service="$PROJECT-$ENVN-$app-main"
  begin_app main
  seed_slow
  for kind in "$@"; do
    before=$(live_generation "$service")
    args=("$kind" "$app")
    count=4
    if [ "$kind" = scale ]; then
      count=5
      args+=(--replicas "$count")
    fi
    out=$(adm "${args[@]}" 2>&1); code=$?
    printf '%s\n' "$out" >"$LOG/$kind-operation.out"
    expect_code "$kind waits for the replacement batches" 0 "$code" "$out"
    d service ps --no-trunc "$service" >"$LOG/$kind-tasks.txt"
    adm lock status --json >"$LOG/$kind-lock.json" 2>&1
    expect_contains "$kind releases the lock" "$(cat "$LOG/$kind-lock.json")" '"locked": false'
    if [ "$code" -ne 0 ]; then finish; fi
    after=$(live_generation "$service")
    if [ -z "$after" ] || [ "$before" = "$after" ]; then fail "$kind starts a new task generation" "$before -> $after"; fi
    assert_image_slots main "$WEB_V1_DIGEST" "$count"
  done
}

run_compensation() {
  local out code service sentinel previous
  app="compensation-$nonce"
  begin_app main sentinel
  seed_slow true
  service="$PROJECT-$ENVN-$app-main"
  sentinel="$PROJECT-$ENVN-$app-sentinel"
  previous=$(live_generation "$service")

  manifest fast 75s false | write_manifest
  out=$(adm deploy "$app" --version "$WEB_V2_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
  printf '%s\n' "$out" >"$LOG/compensation-operation.out"
  expect_code 'unhealthy candidate exits 1' 1 "$code" "$out"
  expect_contains 'later role exhausted its start attempts' "$out" 'gave up after 1 failed starts'
  expect_contains 'candidate rolled back' "$out" 'state: rolled_back'
  expect_eq 'predecessor digest remains applied' "$repo@$WEB_V1_DIGEST" "$(applied)"
  d service ps --no-trunc "$service" >"$LOG/compensation-main-tasks.txt"
  d service ps --no-trunc "$sentinel" >"$LOG/compensation-sentinel-tasks.txt"
  adm lock status --json >"$LOG/compensation-lock.json" 2>&1
  expect_contains 'compensation releases the lock' "$(cat "$LOG/compensation-lock.json")" '"locked": false'
  expect_eq 'prior main generation was restored' "$previous" "$(live_generation "$service")"
  assert_image_slots main "$WEB_V1_DIGEST" 4
  assert_image_slots sentinel "$WEB_V1_DIGEST" 1
}

case ${BERTHY_E2E_ROLLOUT_CASE:-all} in
  all) run_progress; run_drain; run_runtime restart scale; run_compensation ;;
  progress) run_progress ;;
  drain) run_drain ;;
  restart) run_runtime restart ;;
  scale) run_runtime scale ;;
  compensation) run_compensation ;;
  *) fail 'choose rollout case' "$BERTHY_E2E_ROLLOUT_CASE" ;;
esac
finish
