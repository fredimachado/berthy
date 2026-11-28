#!/bin/bash
# A shell-free worker must survive its uptime threshold. Clean and nonzero early exits spend a start attempt.
set -euo pipefail
. "$(dirname "$0")/../lib.sh"

nonce=$(openssl rand -hex 6)
owner="$FIXTURE_ID.$nonce"
app="uptime-$nonce"
service="$PROJECT-$ENVN-$app-worker"
sibling="$PROJECT-$ENVN-$app-z-fail"
adopted_app="$app-adopted"
adopted_service="$PROJECT-$ENVN-$adopted_app-worker"
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
  local code=$? current ref label name
  stop_events
  if [ -n "$service_attempted" ]; then
    for name in "$service" "$sibling" "$adopted_service"; do
      current=$(d service inspect "$name" 2>/dev/null | jq -r --arg project "$PROJECT" --arg env "$ENVN" --arg app "$app" --arg adopted "$adopted_app" --arg owner "$owner" --arg repo "$repo@" \
        '.[0] | select((.Spec.TaskTemplate.ContainerSpec.Image | startswith($repo)) and
          (.Spec.Labels["berthy.e2e.uptime"] == $owner or
           (.Spec.Labels["berthy.project"] == $project and .Spec.Labels["berthy.environment"] == $env and
            (.Spec.Labels["berthy.app"] == $app or .Spec.Labels["berthy.app"] == $adopted)))) | .ID' || true)
      if [ -n "$current" ]; then
        d service rm "$current" >/dev/null || code=1
      fi
    done
    wait_for "uptime fixture containers removed" owned_tasks_gone || code=1
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
if grep -Fxq "$service" <<<"$services" || grep -Fxq "$sibling" <<<"$services" || grep -Fxq "$adopted_service" <<<"$services" || grep -Fxq "$repo" <<<"$images"; then
  die 'fixture names are available' "$service, $sibling, $adopted_service, or $repo already exists"
fi

cat >"$work/worker.go" <<'GO'
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	behavior := os.Getenv("WORKER_EXIT")
	if len(os.Args) == 2 {
		behavior = os.Args[1]
	}
	switch behavior {
	case "exec":
		fmt.Println("uptime exec ok")
		return
	case "check-health":
		return
	case "fail-health":
		os.Exit(1)
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
  local version=$1 behavior=$2 ref="$repo:$1" pinned check
  refs+=("$ref")
  if [[ "$version" = final || "$version" = adopted ]]; then
    check=check-health
    if [ "$version" = final ]; then check=fail-health; fi
    DOCKER_BUILDKIT=0 d build --network none -t "$ref" -f - "$work" >"$LOG/uptime-build-$version.log" <<DOCKERFILE
FROM scratch
COPY worker /worker
LABEL berthy.e2e.uptime=$owner berthy.e2e.version=$version
ENTRYPOINT ["/worker"]
HEALTHCHECK --interval=1s --retries=1 CMD ["/worker","$check"]
DOCKERFILE
  else
    d import --change 'ENTRYPOINT ["/worker"]' --change "ENV WORKER_EXIT=$behavior" \
      --change "LABEL berthy.e2e.uptime=$owner berthy.e2e.version=$version" "$work/worker.tar" "$ref" >/dev/null
  fi
  d push "$ref" >"$LOG/uptime-push-$version.log"
  pinned=$(d image inspect "$ref" | jq -er --arg prefix "$repo@" '.[0].RepoDigests[] | select(startswith($prefix))')
  [[ $pinned == "$repo@sha256:"* ]]
  refs=("$pinned" "${refs[@]}")
  digest=${pinned#*@}
}

manifest() {
  local health=$1 budget=${2:-90s} target_app=${3:-$app} bound=${4:-}
  cat <<YML
project: $PROJECT
network: $NETWORK
apps:
  $target_app:
    kind: image
    image: $repo
    deploy:
      start_attempts: 2
      deploy_timeout: $budget
    roles:
      worker:
        replicas: 1
YML
  if [ -n "$bound" ]; then printf '        service: %s
' "$bound"; fi
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
manifest '{uptime: 20s, start_interval: 2s}' 30s | write_manifest
code=0
out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$MAN" 2>&1) || code=$?
printf '%s\n' "$out" >"$LOG/uptime-incompatible-validate.txt"
expect_code 'validation rejects a command option with uptime' 1 "$code" "$out"
expect_contains 'validation names health.start_interval' "$out" 'health.start_interval'
code=0
out=$(adm deploy "$app" --version "$good" --manifest-dir "$MAN" 2>&1) || code=$?
printf '%s\n' "$out" >"$LOG/uptime-incompatible.txt"
expect_code 'command options with uptime are refused' 1 "$code" "$out"
expect_contains 'deploy explains that startup interval requires a command' "$out" 'start_interval is only allowed with health.test'
services=$(d service ls --format '{{.Name}}') || die 'list services after incompatible plan'
if grep -Fxq "$service" <<<"$services"; then die 'invalid health options leave no service'; fi
pass 'invalid health options leave no service'
if [ "$E2E_FAILS" -ne 0 ]; then finish; fi

watch_worker() {
  local step=$1 name=$2 since
  since=$(date -u +'%Y-%m-%dT%H:%M:%S.%NZ')
  DOCKER_CONFIG="$E2E_ROOT/docker-config" "$DOCKER_BIN" --host "unix://$SOCK" events --since "$since" \
    --filter type=container --filter "label=com.docker.swarm.service.name=$name" --format '{{json .}}' \
    >"$LOG/uptime-$step-events.jsonl" &
  events_pid=$!
}

deploy_worker() {
  local step=$1 wanted=$2 target_app=${3:-$app} name=${4:-$service}
  watch_worker "$step" "$name"
  code=0
  out=$(adm deploy "$target_app" --version "$wanted" --manifest-dir "$MAN" 2>&1) || code=$?
  terminal=$(date +%s%N)
  stop_events
  printf '%s\n' "$out" >"$LOG/uptime-$step-deploy.txt"
  operation=$(awk '/^id: / {print $2}' <<<"$out")
}

running_container() {
  local image=$1 generation=${2:-} name=${3:-$service} ids id inspect
  ids=$(d ps -q --no-trunc --filter "label=com.docker.swarm.service.name=$name") || return 1
  for id in $ids; do
    inspect=$(d inspect "$id" 2>/dev/null) || continue
    if jq -e --arg img "$image" --arg gen "$generation" '.[0] |
      .Config.Image == $img and .State.Running and
      ($gen == "" or .Config.Labels["berthy.operation"] == $gen)' <<<"$inspect" >/dev/null; then
      printf '%s\n' "$id"
      return 0
    fi
  done
  return 1
}

check_healthy() {
  local step=$1 wanted=$2 name=${3:-$service} id
  expect_code "$step deploy succeeds" 0 "$code" "$out"
  expect_contains "$step has terminal success" "$out" 'state: succeeded'
  if [ "$E2E_FAILS" -ne 0 ] || [ -z "$operation" ]; then die "$step records a successful operation"; fi
  id=$(running_container "$repo@$wanted" "$operation" "$name") || die "$step has a running task for the new generation"
  d service inspect "$name" >"$LOG/uptime-$step-service.json"
  d inspect "$id" >"$LOG/uptime-$step-task.json"
  jq -e '.[0].Spec.TaskTemplate.ContainerSpec |
    (.Mounts // []) == [] and .Healthcheck == {Test:["NONE"]}' "$LOG/uptime-$step-service.json" >/dev/null ||
    die "$step service disables Docker health and has no mounts"
  jq -e '.[0] | .Config.Healthcheck.Test == ["NONE"] and (.State | has("Health") | not) and
    (.Mounts // []) == []' "$LOG/uptime-$step-task.json" >/dev/null ||
    die "$step running container has no Docker health or mounts" "$LOG/uptime-$step-task.json"
  if ! python3 - "$LOG/uptime-$step-events.jsonl" "$id" "$repo@$wanted" "$RUN/state/state.db" "$operation" "$terminal" "$step" <<'PYCODE'; then
import json, sqlite3, sys

path, container, image, db, operation, terminal, step = sys.argv[1:]
with open(path) as stream:
    events = [json.loads(line) for line in stream]
matching = [e for e in events if e["Actor"]["ID"] == container and e["Actor"]["Attributes"].get("image") == image]
started = next(e["timeNano"] for e in matching if e["Action"] == "start")
with sqlite3.connect("file:" + db + "?mode=ro", uri=True) as conn:
    rows = conn.execute("SELECT kind, data, created FROM events WHERE operation_id=? ORDER BY id", (operation,)).fetchall()
ready = next(created for kind, data, created in rows if kind == "rollout" and json.loads(data) == {"role": "worker", "status": "ready"})
succeeded = next(created for kind, _, created in rows if kind == "succeeded")
assert ready - started // 1_000_000 >= 20_000 and ready <= succeeded <= int(terminal) // 1_000_000, (started, ready, succeeded, terminal)
print(f"{step}: start to ready {(ready*1_000_000-started)/1e9:.3f}s, start to success {(succeeded*1_000_000-started)/1e9:.3f}s")
PYCODE
    die "$step readiness follows the threshold and precedes terminal success"
  fi
  pass "$step task runs without Docker health and Berthy recorded readiness"
}

manifest "$default" | write_manifest
deploy_worker default "$good"
check_healthy default "$good"

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
  local step=$1 wanted=$2 exit_code=$3 task_state=$4 live id task status started finished
  manifest "$default" | write_manifest
  deploy_worker "$step" "$wanted"
  expect_code "$step deploy exits 1" 1 "$code" "$out"
  expect_contains "$step deploy rolls back" "$out" 'state: rolled_back'
  expect_contains "$step deploy exhausts two starts" "$out" 'gave up after 2 failed starts'
  d service ps --no-trunc --format '{{json .}}' "$service" >"$LOG/uptime-$step-tasks.jsonl"
  if ! python3 - "$LOG/uptime-$step-events.jsonl" "$LOG/uptime-$step-tasks.jsonl" \
    "$repo@$wanted" "$exit_code" "$task_state" "$terminal" <<'PY'; then
import json, sys

events_path, tasks_path, image, exit_code, state, terminal = sys.argv[1:]
with open(events_path) as stream:
    events = [json.loads(line) for line in stream]
with open(tasks_path) as stream:
    tasks = [json.loads(line) for line in stream]
candidate = [e for e in events if e["Actor"]["Attributes"].get("image") == image]
exits = [e for e in candidate if e["Action"] == "die" and e["Actor"]["Attributes"].get("exitCode") == exit_code]
exit_ids = {e["Actor"]["Attributes"]["com.docker.swarm.task.id"] for e in exits}
observed = {t["ID"] for t in tasks if t["Image"] == image and t["CurrentState"].split()[0] == state}
assert len(observed & exit_ids) >= 2, (observed, exit_ids)
starts = {e["Actor"]["ID"]: e["timeNano"] for e in candidate if e["Action"] == "start"}
first, second = sorted(exits, key=lambda e: e["timeNano"])[:2]
elapsed = [(e["timeNano"] - starts[e["Actor"]["ID"]]) / 1e9 for e in (first, second)]
print(f"exit {exit_code}: first two candidates exited after {elapsed[0]:.3f}s and {elapsed[1]:.3f}s, start to terminal rollback {(int(terminal)-min(starts.values()))/1e9:.3f}s")
assert all(0 <= age < 20 for age in elapsed), elapsed
print(f"exit {exit_code}: {len(observed & exit_ids)} retained {state} tasks match exit events")
PY
    die "$step candidates spent two distinct start attempts" "$LOG/uptime-$step-events.jsonl"
  fi
  expect_eq "$step previous digest remains applied" "$repo@$good" "$(applied)"
  live=$(d service inspect -f '{{.Spec.TaskTemplate.ContainerSpec.Image}}' "$service")
  expect_eq "$step previous live digest restored" "$repo@$good" "$live"
  id=$(running_container "$repo@$good") || die "$step previous worker is running"
  task=$(d inspect -f '{{index .Config.Labels "com.docker.swarm.task.id"}}' "$id")
  status=$(d inspect --type task "$task" | jq -er '.[0] | select(.DesiredState == "running" and .Status.State == "running") | .Status.Timestamp')
  started=$(date -u -d "$status" +%s%N)
  finished=$(python3 - "$RUN/state/state.db" "$operation" <<'PYCODE'
import sqlite3, sys

db, operation = sys.argv[1:]
with sqlite3.connect("file:" + db + "?mode=ro", uri=True) as conn:
    row = conn.execute("SELECT created FROM events WHERE operation_id=? AND kind='rolled_back'", (operation,)).fetchone()
    print(row[0])
PYCODE
  )
  if (( finished - started / 1000000 < 20000 )); then
    die "$step predecessor met its uptime threshold before rollback completed" "$status / $finished"
  fi
  pass "$step previous worker met its threshold before rollback completed"
  expect_contains "$step deploy lock is free" "$(adm lock status --json 2>&1)" '"locked": false'
  if [ "$E2E_FAILS" -ne 0 ]; then finish; fi
}

check_early_exit nonzero "$nonzero" 1 Failed
check_early_exit clean "$clean" 0 Complete

manifest "$default" | write_manifest
deploy_worker final "$final"
d image inspect "$repo:final" | jq -e '.[0].Config.Healthcheck.Test == ["CMD","/worker","fail-health"]' >/dev/null ||
  die 'final image declares a failing Docker check'
check_healthy final "$final"
expect_eq 'the final surviving digest is applied' "$repo@$final" "$(applied)"

check_runtime_slots() {
  local step=$1 count=$2 generation ids task_ids specs stamp started
  local -a containers tasks
  expect_contains "$step succeeds" "$out" 'state: succeeded'
  generation=$(awk '/^id: / {print $2}' <<<"$out")
  [ -n "$generation" ] || die "$step records an operation"
  ids=$(d ps -q --no-trunc --filter "label=com.docker.swarm.service.name=$service" --filter "label=berthy.operation=$generation")
  [ -n "$ids" ] || die "$step has replacement containers"
  mapfile -t containers <<<"$ids"
  task_ids=$(d inspect "${containers[@]}" | jq -er '.[] | select(.State.Running and .Config.Healthcheck.Test == ["NONE"] and (.State | has("Health") | not)) | .Config.Labels["com.docker.swarm.task.id"]')
  mapfile -t tasks <<<"$task_ids"
  specs=$(d inspect --type task "${tasks[@]}")
  jq -e --argjson count "$count" '(map(.Slot) | sort) == [range(1; $count+1)] and
    all(.[]; .DesiredState == "running" and .Status.State == "running")' <<<"$specs" >/dev/null ||
    die "$step has $count running replacement slots" "$specs"
  while IFS= read -r stamp; do
    started=$(date -u -d "$stamp" +%s%N)
    if (( terminal - started < 20000000000 )); then
      die "$step waits for each replacement to meet the uptime threshold" "$stamp / $terminal"
    fi
  done < <(jq -r '.[].Status.Timestamp' <<<"$specs")
  pass "$step completed with $count replacement slot(s) past the uptime threshold"
}

out=$(adm restart "$app" 2>&1) || die 'restart of uptime worker succeeds' "$out"
terminal=$(date +%s%N)
check_runtime_slots restart 1
out=$(adm scale "$app" --replicas 2 2>&1) || die 'scale of uptime worker succeeds' "$out"
terminal=$(date +%s%N)
check_runtime_slots scale 2
out=$(adm wait "$app" --timeout 30s 2>&1) || die 'wait of uptime workers succeeds' "$out"
pass 'wait verifies both uptime workers'
out=$(adm exec "$app" -- /worker exec 2>&1) || die 'exec in an uptime worker succeeds' "$out"
expect_contains 'shell-free exec prints its response' "$out" 'uptime exec ok'

{
  manifest "$default"
  cat <<YML
      z-fail:
        health: $default
        command: ["1"]
YML
} | write_manifest
deploy_worker sibling "$good"
expect_code 'failing sibling deploy exits 1' 1 "$code" "$out"
expect_contains 'failing sibling rolls back' "$out" 'state: rolled_back'
expect_contains 'printed reason identifies the exited sibling' "$out" 'reason: role z-fail: task exited with code 1'
if grep -q '^reason: .*role worker:' <<<"$out"; then
  die 'printed reason omits the ready worker' "$out"
fi
[ -n "$operation" ] || die 'failing sibling operation has an ID'
adm diagnostics "$operation" --json >"$LOG/uptime-sibling-diagnostics.json" ||
  die 'read diagnostics for failing sibling'
jq -e '.diagnostics | length > 0 and all(.[]; .role == "z-fail") and
  any(.[]; .kind == "task" and .exit_code == 1)' "$LOG/uptime-sibling-diagnostics.json" >/dev/null ||
  die 'stored diagnostics identify only the exited sibling' "$LOG/uptime-sibling-diagnostics.json"
pass 'printed reason and stored diagnostics name only the failing sibling'

build_image adopted stay
adopted_digest=$digest
out=$(d service create --detach --no-resolve-image --name "$adopted_service" --network "$NETWORK" \
  --label "berthy.e2e.uptime=$owner" "$repo@$adopted_digest" 2>&1) ||
  die 'create command-checked worker to adopt' "$out"
adoptable_worker_healthy() {
  local ids
  ids=$(d ps -q --filter "label=com.docker.swarm.service.name=$adopted_service" --filter health=healthy) &&
    [ -n "$ids" ]
}
wait_for 'worker for adoption becomes Docker healthy' adoptable_worker_healthy ||
  die 'worker for adoption becomes Docker healthy'
before_spec=$(d service inspect -f '{{json .Spec}}' "$adopted_service")

manifest "$default" 90s "$adopted_app" "$adopted_service" | write_manifest
out=$(adm deploy "$adopted_app" --adopt --version "$adopted_digest" --manifest-dir "$MAN" 2>&1) ||
  die 'adopt command-checked worker with an uptime manifest' "$out"
expect_contains 'adoption succeeds' "$out" 'state: succeeded'
expect_eq 'adoption preserves the live spec' "$before_spec" "$(d service inspect -f '{{json .Spec}}' "$adopted_service")"

watch_worker adopted-restart "$adopted_service"
out=$(adm restart "$adopted_app" 2>&1) || die 'restart of adopted worker succeeds' "$out"
terminal=$(date +%s%N)
stop_events
expect_contains 'adopted restart succeeds' "$out" 'state: succeeded'
operation=$(awk '/^id: / {print $2}' <<<"$out")
[ -n "$operation" ] || die 'adopted restart records an operation'
id=$(running_container "$repo@$adopted_digest" "$operation" "$adopted_service") ||
  die 'adopted restart creates a replacement'
d inspect "$id" | jq -e '.[0] | .Config.Healthcheck.Test == ["CMD","/worker","check-health"] and
  .State.Health.Status == "healthy"' >/dev/null || die 'adopted replacement keeps the Docker check'
if ! python3 - "$LOG/uptime-adopted-restart-events.jsonl" "$id" "$terminal" <<'PY'; then
import json, sys

path, container, terminal = sys.argv[1:]
with open(path) as stream:
    events = [json.loads(line) for line in stream]
healthy = next(e["timeNano"] for e in events if e["Actor"]["ID"] == container and e["Action"] == "health_status: healthy")
assert healthy < int(terminal), (healthy, terminal)
PY
  die 'adopted restart waits for Docker health before returning'
fi
pass 'adopted restart kept the Docker command check and waited for health'

deploy_worker adopted-first-deploy "$final" "$adopted_app" "$adopted_service"
check_healthy adopted-first-deploy "$final" "$adopted_service"

finish
