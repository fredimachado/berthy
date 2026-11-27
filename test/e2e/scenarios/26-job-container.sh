#!/bin/bash
# 26-job-container proves that a job of an image app runs in a container of its own, on the real Swarm. The job's container is
# labelled with the job, has the app's environment, the role's named volume and the shared network, and is gone when the job
# ends. A job past its timeout is stopped, so no container or process of it is left. A job that fails keeps its last output
# in the state, with the secret values redacted, and `job logs` shows it; the job's check names that command. A job runs
# while its role is scaled to 0, and a deploy of the app during a long job does not end the job.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

DATA_VOLUME="data"
slot_epoch=$(( ($(date -u +%s) / 60) * 60 - 4 * 3600 ))
slot_minute=$(date -u -d "@$slot_epoch" +%-M)
slot_hour=$(date -u -d "@$slot_epoch" +%-H)
slot_text=$(date -u -d "@$slot_epoch" +%Y-%m-%dT%H:%M:%SZ)

manifest() {
  cat <<YML
project: $PROJECT
network: $NETWORK
apps:
  web:
    kind: image
    image: $REG/e2e/web
    env:
      secret:
        - SECRET_VAR: JOBSECRET
    roles:
      main:
        volumes:
          - "$DATA_VOLUME:/data"
        health:
          test: ["CMD", "true"]
          interval: 2s
          timeout: 2s
          start_period: 1s
          retries: 3
jobs:
  missed:
    app: web
    schedule: "$slot_minute $slot_hour * * *"
    command: ["true"]
  slow:
    app: web
    command: ["sh", "-c", "echo working; sleep 60"]
    timeout: 5s
  failing:
    app: web
    schedule: "0 3 * * *"
    command: ["sh", "-c", "echo boom \$SECRET_VAR; exit 3"]
  volume:
    app: web
    command: ["sh", "-c", "test \"\$(cat /data/marker)\" = hello && echo \$BERTHY_ROLE"]
  long:
    app: web
    command: ["sh", "-c", "sleep 30; echo finished"]
YML
}

# job_containers JOB: the ids of the containers of JOB, running or not.
job_containers() { d ps -aq --filter label=berthy.environment=$ENVN --filter label=berthy.oneoff=job --filter label=berthy.job="$1"; }

manifest | write_manifest
printf 'hunter-2-secret' | adm secret set JOBSECRET >/dev/null 2>&1
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1" 0 "$code" "$out"

# 1. The web replica writes a file into the role's volume. A job of the app reads it from its own container, which mounts the
# same volume, and sees the role and the app's environment.
out=$(adm exec web -- sh -c 'echo hello > /data/marker' 2>&1); code=$?
expect_code "the replica writes into its volume" 0 "$code" "$out"
out=$(adm job run volume 2>&1); code=$?
expect_code "a job reads the file the replica wrote" 0 "$code" "$out"
expect_contains "the run succeeded" "$out" "status: succeeded"
expect_eq "the container of a finished job is removed" "" "$(job_containers volume)"

# 2. A job past its timeout is stopped: nothing of it is left, and its last output is kept.
start=$SECONDS
out=$(adm job run slow 2>&1); code=$?
elapsed=$((SECONDS - start))
expect_code "a job past its timeout fails" 1 "$code" "$out"
expect_contains "the failure says it timed out" "$out" "timed out after 5s"
if [ "$elapsed" -lt 40 ]; then pass "the timed-out job ended in ${elapsed}s"; else fail "the timed-out job ended in time" "took ${elapsed}s"; fi
expect_eq "no container of the timed-out job is left" "" "$(job_containers slow)"
out=$(adm job logs slow 2>&1); code=$?
expect_code "job logs shows the timed-out run" 0 "$code" "$out"
expect_contains "the output kept is what the job printed" "$out" "working"
expect_contains "the run is named" "$out" "timed out after 5s"

# 3. A job that fails keeps its output with the secret redacted, and its alert names the command.
out=$(adm job run failing 2>&1); code=$?
expect_code "a failing job exits non-zero" 1 "$code" "$out"
expect_contains "the failure has the exit status" "$out" "exit status 3"
expect_contains "the run says where the output is" "$out" "berthy job logs failing"
out=$(adm job logs failing 2>&1); code=$?
expect_code "job logs shows the failed run" 0 "$code" "$out"
expect_contains "the output has the redacted secret" "$out" "boom [redacted]"
expect_not_contains "the output has no secret value" "$out" "hunter-2-secret"
out=$(adm job logs failing --json 2>&1); code=$?
expect_code "job logs --json answers" 0 "$code" "$out"
expect_contains "the JSON names the run's status" "$out" '"status": "failed"'
expect_not_contains "the JSON has no secret value" "$out" "hunter-2-secret"
out=$(adm job logs volume 2>&1); code=$?
expect_code "a job that never failed has no output kept" 1 "$code" "$out"
expect_contains "the refusal says so" "$out" "no output kept for volume"

# The job's check turns critical and its detail names the command.
checked=""
for _ in $(seq 1 25); do
  out=$(adm checks 2>&1)
  if grep -qF "berthy job logs failing shows its output" <<<"$out"; then checked=yes; break; fi
  sleep 3
done
if [ -n "$checked" ]; then pass "the job's check names the command that shows the output"; else fail "the job's check names the command that shows the output" "not seen in checks"; detail_block "$out"; fi

# 4. A job needs no running replica: it runs while its role is scaled to 0.
out=$(adm scale web --replicas 0 2>&1); code=$?
expect_code "scale to 0" 0 "$code" "$out"
expect_eq "no replica runs" "0" "$(d ps -q --filter label=com.docker.swarm.service.name="$PROJECT-$ENVN-web-main" | wc -l)"
out=$(adm job run volume 2>&1); code=$?
expect_code "a job runs while its role is scaled to 0" 0 "$code" "$out"
expect_contains "it succeeded" "$out" "status: succeeded"

# Seed yesterday's successful occurrence and leave today's slot unhandled while the agent is stopped.
out=$(adm job run missed 2>&1); code=$?
expect_code "the missed fixture job succeeds" 0 "$code" "$out"
run_id=$(adm jobs --json | jq -r '.[] | select(.name == "missed") | .last.id')
if ! [[ "$run_id" =~ ^[1-9][0-9]*$ ]]; then fail "capture the missed fixture run" "$run_id"; finish; fi
stop_agent
if python3 - "$RUN/state/state.db" "$ENVN" "$run_id" "$slot_epoch" <<'PY'
import datetime, json, sqlite3, sys
path, env, run_id, slot = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
today = datetime.datetime.fromtimestamp(slot, datetime.timezone.utc)
yesterday = today - datetime.timedelta(days=1)
with sqlite3.connect('file:' + path + '?mode=rw', uri=True) as db:
    release = db.execute("SELECT r.id, r.manifest, r.operation_id FROM applied a JOIN releases r ON r.id=a.release_id WHERE a.environment=? AND a.app='web'", (env,)).fetchone()
    settings = db.execute('SELECT manifest, operation_id FROM environment_settings WHERE environment=?', (env,)).fetchone()
    assert release and settings and release[2] == settings[1], 'release and settings must come from the same deployment'
    expression = json.loads(release[1])['jobs']['missed']['schedule']
    timezone = json.loads(settings[0]).get('timezone') or 'UTC'
    assert timezone == 'UTC'
    assert expression == f'{today.minute} {today.hour} * * *'
    assert (yesterday.minute, yesterday.hour) == (today.minute, today.hour)
    handled = int(yesterday.timestamp()) * 1000
    before = handled - 60000
    assert db.execute('UPDATE releases SET created=? WHERE id=? AND environment=?', (before, release[0], env)).rowcount == 1
    assert db.execute('UPDATE environment_settings SET updated=? WHERE environment=?', (before, env)).rowcount == 1
    assert db.execute("UPDATE job_runs SET trigger='schedule', scheduled=?, started=?, finished=? WHERE environment=? AND id=? AND job='missed' AND trigger='manual' AND status='succeeded'", (handled, handled, handled + 1000, env, run_id)).rowcount == 1
    assert db.execute('SELECT created FROM releases WHERE id=?', (release[0],)).fetchone()[0] < handled
    assert db.execute('SELECT updated FROM environment_settings WHERE environment=?', (env,)).fetchone()[0] < handled
    assert db.execute("SELECT COUNT(*) FROM job_runs WHERE environment=? AND job='missed' AND scheduled=?", (env, slot * 1000)).fetchone()[0] == 0
PY
then pass "seed yesterday's scheduled success"; else fail "seed yesterday's scheduled success" "fixture transaction failed"; start_agent; finish; fi
start_agent || { fail "restart the agent" "the agent did not answer"; finish; }
expected_detail="missed scheduled run at $slot_text"
check_json=""
for _ in $(seq 1 45); do
  check_json=$(adm checks --json 2>&1)
  if [ "$(printf '%s' "$check_json" | jq -r '.[] | select(.name == "job:missed") | .state' 2>/dev/null)" = warning ]; then break; fi
  sleep 1
done
check_row=$(printf '%s' "$check_json" | jq -c '.[] | select(.name == "job:missed")' 2>/dev/null)
printf 'missed slot: %s; seeded run: %s; check: %s\n' "$slot_text" "$run_id" "$check_row"
expect_eq "the missed slot warns" "warning" "$(printf '%s' "$check_row" | jq -r '.state' 2>/dev/null)"
expect_eq "the warning names the missed slot" "$expected_detail" "$(printf '%s' "$check_row" | jq -r '.detail' 2>/dev/null)"
out=$(adm checks 2>&1); code=$?
expect_code "human-readable checks answers" 0 "$code" "$out"
printf '%s\n' "$out" | grep -F 'job:missed' || true
expect_contains "human-readable checks names the job" "$out" "job:missed"
expect_contains "human-readable checks names the missed slot" "$out" "$expected_detail"
last_id=$(adm jobs --json | jq -r '.[] | select(.name == "missed") | .last.id')
expect_eq "the seeded run remains the last run" "$run_id" "$last_id"
remaining=$(python3 - "$RUN/state/state.db" "$ENVN" "$slot_epoch" <<'PY'
import sqlite3, sys
with sqlite3.connect('file:' + sys.argv[1] + '?mode=ro', uri=True) as db:
    print(db.execute("SELECT COUNT(*) FROM job_runs WHERE environment=? AND job='missed' AND scheduled=?", (sys.argv[2], int(sys.argv[3]) * 1000)).fetchone()[0])
PY
)
expect_eq "today's slot remains unexecuted" "0" "$remaining"

# 5. A deploy of the app during a long job does not end it: the job finishes on the release it started with.
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy brings the role back" 0 "$code" "$out"
adm job run long >"$RUN/long.out" 2>&1 &
longpid=$!
seen=""
for _ in $(seq 1 30); do
  if [ -n "$(job_containers long)" ]; then seen=yes; break; fi
  sleep 1
done
if [ -n "$seen" ]; then pass "the long job runs in a container of its own"; else fail "the long job runs in a container of its own" "no container"; fi
label=$(d inspect --format '{{index .Config.Labels "berthy.job"}}' "$(job_containers long | head -n 1)" 2>/dev/null)
expect_eq "the container carries the job's label" "long" "$label"
out=$(adm deploy web --version v2 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v2 during the job" 0 "$code" "$out"
if [ -n "$(job_containers long)" ]; then pass "the deploy left the job's container running"; else fail "the deploy left the job's container running" "the container is gone"; fi
wait "$longpid"; code=$?
expect_code "the long job finished successfully" 0 "$code" "$(cat "$RUN/long.out")"
expect_contains "the long job succeeded" "$(cat "$RUN/long.out")" "status: succeeded"
expect_eq "the long job's container is removed" "" "$(job_containers long)"

finish
