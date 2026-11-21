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
