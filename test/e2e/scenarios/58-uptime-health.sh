#!/bin/bash
# A shell-free worker must survive its uptime threshold. Clean and nonzero early exits spend a start attempt.
set -euo pipefail
. "$(dirname "$0")/../lib.sh"

nonce=$(openssl rand -hex 6)
owner="$FIXTURE_ID.$nonce"
app="uptime-$nonce"
service="$PROJECT-$ENVN-$app-worker"
repo="$REG/$PROJECT-$ENVN-$app"
work=$(mktemp -d "$RUN/uptime.XXXXXX")
events_pid=
service_attempted=
refs=()

die() { fail "$1" "${2:-}"; finish; }

stop_events() {
  if [ -n "$events_pid" ]; then
    kill "$events_pid" 2>/dev/null || true
    wait "$events_pid" 2>/dev/null || true
    events_pid=
  fi
}

owned_tasks_gone() {
  local tasks
  tasks=$(d ps -aq --filter "label=berthy.e2e.uptime=$owner") && [ -z "$tasks" ]
}

cleanup() {
  local code=$? current ref label
  stop_events
  if [ -n "$service_attempted" ]; then
    current=$(d service inspect "$service" 2>/dev/null | jq -r --arg project "$PROJECT" --arg env "$ENVN" --arg app "$app" --arg repo "$repo@" \
      '.[0] | select(.Spec.Labels["berthy.project"] == $project and .Spec.Labels["berthy.environment"] == $env and .Spec.Labels["berthy.app"] == $app and (.Spec.TaskTemplate.ContainerSpec.Image | startswith($repo))) | .ID' || true)
    if [ -n "$current" ]; then
      d service rm "$current" >/dev/null || code=1
      wait_for "uptime fixture containers removed" owned_tasks_gone || code=1
    fi
  fi
  for ref in "${refs[@]}"; do
    label=$(d image inspect -f '{{index .Config.Labels "berthy.e2e.uptime"}}' "$ref" 2>/dev/null || true)
    if [ "$label" = "$owner" ]; then
      d image rm "$ref" >/dev/null || code=1
    fi
  done
  rm -f "$work/worker" "$work/worker.go" "$work/worker.tar"
  rmdir "$work"
  exit "$code"
}
trap cleanup EXIT

services=$(d service ls --format '{{.Name}}') || die 'list existing services'
images=$(d images --no-trunc --digests --format '{{.Repository}}') || die 'list existing images'
if grep -Fxq "$service" <<<"$services" || grep -Fxq "$repo" <<<"$images"; then
  die 'fixture names are available' "$service or $repo already exists"
fi

cat >"$work/worker.go" <<'GO'
package main

import (
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	switch os.Getenv("WORKER_EXIT") {
	case "0":
		<-time.After(5 * time.Second)
		os.Exit(0)
	case "1":
		<-time.After(5 * time.Second)
		os.Exit(1)
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM)
	<-stop
}
GO
CGO_ENABLED=0 "$GO" build -o "$work/worker" "$work/worker.go"
tar -C "$work" -cf "$work/worker.tar" worker

build_image() {
  local version=$1 behavior=$2 ref="$repo:$1" pinned
  refs+=("$ref")
  d import --change 'ENTRYPOINT ["/worker"]' --change "ENV WORKER_EXIT=$behavior" \
    --change "LABEL berthy.e2e.uptime=$owner berthy.e2e.version=$version" "$work/worker.tar" "$ref" >/dev/null
  d push "$ref" >"$LOG/uptime-push-$version.log"
  pinned=$(d image inspect "$ref" | jq -er --arg prefix "$repo@" '.[0].RepoDigests[] | select(startswith($prefix))')
  [[ $pinned == "$repo@sha256:"* ]]
  refs=("$pinned" "${refs[@]}")
  digest=${pinned#*@}
}

manifest() {
  local health=$1 budget=${2:-90s}
  cat <<YML
project: $PROJECT
network: $NETWORK
apps:
  $app:
    kind: image
    image: $repo
    deploy:
      start_attempts: 2
      deploy_timeout: $budget
    roles:
      worker:
        replicas: 1
YML
  if [ -n "$health" ]; then printf '        health: %s\n' "$health"; fi
}

build_image initial stay
good=$digest
printf 'Docker Engine %s\n' "$(d version --format '{{.Server.Version}}')"
manifest '' | write_manifest
service_attempted=1
code=0
out=$(adm deploy "$app" --version "$good" --manifest-dir "$MAN" 2>&1) || code=$?
printf '%s\n' "$out" >"$LOG/uptime-no-health.txt"
expect_code 'imported shell-free image without a check cannot deploy' 1 "$code" "$out"
expect_contains 'missing check fails role preflight' "$out" 'role preflight failed'

manifest '{uptime: 20s}' | write_manifest
code=0
out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$MAN" 2>&1) || code=$?
printf '%s\n' "$out" >"$LOG/uptime-validate.txt"
expect_code 'uptime worker manifest validates' 0 "$code" "$out"
if [ "$E2E_FAILS" -ne 0 ]; then finish; fi

build_image nonzero 1
nonzero=$digest
build_image clean 0
clean=$digest
build_image final stay
final=$digest
if [ "$(printf '%s\n' "$good" "$nonzero" "$clean" "$final" | sort -u | wc -l)" -ne 4 ]; then
  die 'worker versions have distinct digests'
fi

default='{uptime: 20s}'
portable='{uptime: 20s, interval: 3s, start_period: 30s}'
manifest '{uptime: 20s, interval: 1h, start_interval: 2s}' 30s | write_manifest
code=0
out=$(adm deploy "$app" --version "$good" --manifest-dir "$MAN" 2>&1) || code=$?
printf '%s\n' "$out" >"$LOG/uptime-incompatible.txt"
expect_code 'unequal startup interval is refused' 1 "$code" "$out"
expect_contains 'refusal names health.start_interval' "$out" 'health.start_interval'
services=$(d service ls --format '{{.Name}}') || die 'list services after incompatible plan'
if grep -Fxq "$service" <<<"$services"; then die 'incompatible timing leaves no service'; fi
pass 'incompatible timing leaves no service'
if [ "$E2E_FAILS" -ne 0 ]; then finish; fi

deploy_worker() {
  local step=$1 wanted=$2 since
  since=$(date -u +'%Y-%m-%dT%H:%M:%S.%NZ')
  DOCKER_CONFIG="$E2E_ROOT/docker-config" "$DOCKER_BIN" --host "unix://$SOCK" events --since "$since" \
    --filter type=container --filter "label=com.docker.swarm.service.name=$service" --format '{{json .}}' \
    >"$LOG/uptime-$step-events.jsonl" &
  events_pid=$!
  code=0
  out=$(adm deploy "$app" --version "$wanted" --manifest-dir "$MAN" 2>&1) || code=$?
  terminal=$(date +%s%N)
  stop_events
  printf '%s\n' "$out" >"$LOG/uptime-$step-deploy.txt"
  operation=$(awk '/^id: / {print $2}' <<<"$out")
}

healthy_container() {
  local image=$1 generation=${2:-} ids id inspect
  ids=$(d ps -q --no-trunc --filter "label=com.docker.swarm.service.name=$service") || return 1
  for id in $ids; do
    inspect=$(d inspect "$id" 2>/dev/null) || continue
    if jq -e --arg img "$image" --arg gen "$generation" '.[0] |
      .Config.Image == $img and .State.Running and .State.Health.Status == "healthy" and
      ($gen == "" or .Config.Labels["berthy.operation"] == $gen)' <<<"$inspect" >/dev/null; then
      printf '%s\n' "$id"
      return 0
    fi
  done
  return 1
}

check_service() {
  local step=$1 interval=$2 grace=$3
  d service inspect "$service" >"$LOG/uptime-$step-service.json"
  jq -e --arg src "$BIN" --argjson interval "$interval" --argjson grace "$grace" '.[0].Spec.TaskTemplate.ContainerSpec |
    .Mounts == [{Type:"bind",Source:$src,Target:"/.berthy/berthy",ReadOnly:true}] and
    .Healthcheck.Test == ["CMD","/.berthy/berthy","probe","uptime","20s"] and
    .Healthcheck.Interval == $interval and (.Healthcheck.StartPeriod // 0) == $grace and
    (.Healthcheck | has("StartInterval") | not)' "$LOG/uptime-$step-service.json" >/dev/null
}

check_healthy() {
  local step=$1 wanted=$2 interval=$3 grace=$4 id
  expect_code "$step deploy succeeds" 0 "$code" "$out"
  expect_contains "$step has terminal success" "$out" 'state: succeeded'
  if [ "$E2E_FAILS" -ne 0 ] || [ -z "$operation" ]; then die "$step records a successful operation"; fi
  check_service "$step" "$interval" "$grace" || die "$step service has the generated check and bind"
  id=$(healthy_container "$repo@$wanted" "$operation") || die "$step has a healthy task for the new generation"
  d inspect "$id" >"$LOG/uptime-$step-task.json"
  if ! jq -e --arg src "$BIN" --argjson interval "$interval" --argjson grace "$grace" '.[0] |
    .Config.Healthcheck.Test == ["CMD","/.berthy/berthy","probe","uptime","20s"] and
    .Config.Healthcheck.Interval == $interval and (.Config.Healthcheck.StartPeriod // 0) == $grace and
    (.Config.Healthcheck.StartInterval // 0) == 0 and
    any(.Mounts[]; .Type == "bind" and .Source == $src and .Destination == "/.berthy/berthy" and .RW == false) and
    any(.State.Health.Log[]; .ExitCode == 0 and .Output == "")' "$LOG/uptime-$step-task.json" >/dev/null; then
    die "$step task uses the expected check, timing, and read-only bind" "$LOG/uptime-$step-task.json"
  fi
  if ! python3 - "$LOG/uptime-$step-events.jsonl" "$id" "$repo@$wanted" "$terminal" "$step" <<'PY'; then
import json, sys

path, container, image, terminal, step = sys.argv[1:]
with open(path) as stream:
    events = [json.loads(line) for line in stream]
matching = [e for e in events if e["Actor"]["ID"] == container and e["Actor"]["Attributes"].get("image") == image]
started = next(e["timeNano"] for e in matching if e["Action"] == "start")
healthy = next(e["timeNano"] for e in matching if e["Action"] == "health_status: healthy")
terminal = int(terminal)
assert healthy - started >= 20_000_000_000 and terminal > healthy, (started, healthy, terminal)
print(f"{step}: start to first healthy {(healthy-started)/1e9:.3f}s, start to terminal success {(terminal-started)/1e9:.3f}s")
PY
    die "$step first health follows the threshold and precedes terminal success"
  fi
  pass "$step task configuration and first healthy event match"
}

manifest "$default" | write_manifest
deploy_worker default "$good"
check_healthy default "$good" 20100000000 0

manifest "$portable" | write_manifest
deploy_worker portable "$good"
check_healthy portable "$good" 3000000000 30000000000

applied() {
  python3 - "$RUN/state/state.db" "$ENVN" "$app" <<'PY'
import sqlite3, sys

db, env, app = sys.argv[1:]
with sqlite3.connect("file:" + db + "?mode=ro", uri=True) as conn:
    row = conn.execute("SELECT releases.digest FROM applied JOIN releases ON releases.id=applied.release_id WHERE applied.environment=? AND applied.app=?", (env, app)).fetchone()
    print(row[0] if row else "")
PY
}

check_early_exit() {
  local step=$1 wanted=$2 exit_code=$3 task_state=$4 previous live
  previous=$(healthy_container "$repo@$good") || die "$step starts with a healthy predecessor"
  manifest "$default" | write_manifest
  deploy_worker "$step" "$wanted"
  expect_code "$step deploy exits 1" 1 "$code" "$out"
  expect_contains "$step deploy rolls back" "$out" 'state: rolled_back'
  expect_contains "$step deploy exhausts two starts" "$out" 'gave up after 2 failed starts'
  d service ps --no-trunc --format '{{json .}}' "$service" >"$LOG/uptime-$step-tasks.jsonl"
  if ! python3 - "$LOG/uptime-$step-events.jsonl" "$LOG/uptime-$step-tasks.jsonl" \
    "$repo@$wanted" "$previous" "$exit_code" "$task_state" "$terminal" <<'PY'; then
import json, sys

events_path, tasks_path, image, previous, exit_code, state, terminal = sys.argv[1:]
with open(events_path) as stream:
    events = [json.loads(line) for line in stream]
with open(tasks_path) as stream:
    tasks = [json.loads(line) for line in stream]
candidate = [e for e in events if e["Actor"]["Attributes"].get("image") == image]
exits = [e for e in candidate if e["Action"] == "die" and e["Actor"]["Attributes"].get("exitCode") == exit_code]
exit_ids = {e["Actor"]["Attributes"]["com.docker.swarm.task.id"] for e in exits}
observed = {t["ID"] for t in tasks if t["Image"] == image and t["CurrentState"].split()[0] == state}
assert len(observed & exit_ids) >= 2, (observed, exit_ids)
assert not any(e["Action"] == "health_status: healthy" for e in candidate)
starts = {e["Actor"]["ID"]: e["timeNano"] for e in candidate if e["Action"] == "start"}
first, second = sorted(exits, key=lambda e: e["timeNano"])[:2]
assert not any(e["Actor"]["ID"] == previous and e["Action"] in ("die", "stop", "destroy") and
               e["timeNano"] <= first["timeNano"] for e in events), "predecessor disappeared during the first start-first attempt"
elapsed = [(e["timeNano"] - starts[e["Actor"]["ID"]]) / 1e9 for e in (first, second)]
print(f"exit {exit_code}: first two candidates exited after {elapsed[0]:.3f}s and {elapsed[1]:.3f}s, start to terminal rollback {(int(terminal)-min(starts.values()))/1e9:.3f}s")
print(f"exit {exit_code}: {len(observed & exit_ids)} retained {state} tasks match exit events, {len(exit_ids)} exit events total, no candidate became healthy")
PY
    die "$step candidates spent attempts while the predecessor was available" "$LOG/uptime-$step-events.jsonl"
  fi
  expect_eq "$step previous digest remains applied" "$repo@$good" "$(applied)"
  live=$(d service inspect -f '{{.Spec.TaskTemplate.ContainerSpec.Image}}' "$service")
  expect_eq "$step previous live digest restored" "$repo@$good" "$live"
  healthy_container "$repo@$good" >/dev/null || die "$step previous worker is running and healthy"
  pass "$step previous worker is running and healthy"
  expect_contains "$step deploy lock is free" "$(adm lock status --json 2>&1)" '"locked": false'
  if [ "$E2E_FAILS" -ne 0 ]; then finish; fi
}

check_early_exit nonzero "$nonzero" 1 Failed
check_early_exit clean "$clean" 0 Complete

manifest "$default" | write_manifest
deploy_worker final "$final"
check_healthy final "$final" 20100000000 0
expect_eq 'the final surviving digest is applied' "$repo@$final" "$(applied)"

finish
