#!/bin/bash
# A serial update takes longer than one startup window while each replica makes progress.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

app="progress-$(openssl rand -hex 6)"
service="$PROJECT-$ENVN-$app-main"
repo="$REG/e2e/web"
attempted=

cleanup() {
  local code=$? owned
  if [ -n "$attempted" ]; then
    owned=$(d service inspect "$service" 2>/dev/null | jq -r --arg project "$PROJECT" --arg env "$ENVN" --arg app "$app" --arg repo "$repo@" \
      '.[0] | select(.Spec.Labels["berthy.project"] == $project and .Spec.Labels["berthy.environment"] == $env and .Spec.Labels["berthy.app"] == $app and (.Spec.TaskTemplate.ContainerSpec.Image | startswith($repo))) | .ID')
    if [ -n "$owned" ]; then d service rm "$owned" >/dev/null || code=1; fi
  fi
  exit "$code"
}
trap cleanup EXIT

if ! services=$(d service ls --format '{{.Name}}') || grep -Fxq "$service" <<<"$services"; then
  fail 'fixture service name is available' "$service"
  finish
fi

manifest() {
  cat <<YML
project: $PROJECT
network: $NETWORK
apps:
  $app:
    kind: image
    image: $repo
    deploy:
      deploy_timeout: 75s
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
}

applied() {
  python3 - "$RUN/state/state.db" "$ENVN" "$app" <<'PY'
import sqlite3, sys
with sqlite3.connect('file:' + sys.argv[1] + '?mode=ro', uri=True) as db:
    row = db.execute('SELECT releases.digest FROM applied JOIN releases ON releases.id=applied.release_id WHERE applied.environment=? AND applied.app=?', sys.argv[2:]).fetchone()
    print(row[0] if row else '')
PY
}

manifest fast | write_manifest
attempted=1
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
slots=
if [ -n "$running" ]; then
  slots=$(d inspect $running | jq -r --arg image "$repo@$WEB_V2_DIGEST" \
    '.[] | select(.Config.Image == $image and .State.Running and .State.Health.Status == "healthy") | .Config.Labels["com.docker.swarm.task.name"] | split(".")[1]' | sort -u)
fi
expect_eq 'four running healthy v2 slots' 4 "$(grep -cE '^[1-9][0-9]*$' <<<"$slots")"
expect_contains 'deploy lock is free' "$(adm lock status --json 2>&1)" '"locked": false'
finish
