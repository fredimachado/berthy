#!/bin/bash
# 33-cancel proves `berthy cancel` on the real Swarm. A deploy with a long pre-deploy command is cancelled: the command's
# container is stopped, the old version still serves and the lock is free. A deploy whose new version never becomes healthy is
# cancelled during the health wait: it ends rolled back and the previous version runs. A deploy that has switched traffic (its
# post-deploy command runs) refuses a cancel with the reason and carries on to succeed; the dry run says so first. A cancel of an
# operation that has ended is refused. A run is cancelled and its container is gone. A job run is cancelled with `job cancel`
# and keeps its last output. In the terminal UI a person approves an agent's held deploy, cancels it while it runs and sees it
# end as cancelled.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

svc="$PROJECT-$ENVN-web-main"

# manifest HEALTH START_PERIOD PRE POST [protected]: the web app. HEALTH is the command of the health check ("true" passes,
# "false" fails), START_PERIOD the time failures do not count, PRE and POST the argv (as a YAML list) of a pre-deploy and a
# post-deploy command, or empty for none. The jobs a scenario cancels are always there.
manifest() {
  local health=$1 start=$2 pre=$3 post=$4
  [ "${5:-}" = protected ] && echo "protected: true"
  cat <<YML
project: $PROJECT
network: $NETWORK
apps:
  web:
    kind: image
    image: $REG/e2e/web
    deploy:
      deploy_timeout: 300s
YML
  if [ -n "$pre$post" ]; then
    echo "    hooks:"
    [ -n "$pre" ] && printf '      pre_deploy:\n        - run: %s\n' "$pre"
    [ -n "$post" ] && printf '      post_deploy:\n        - run: %s\n' "$post"
  fi
  cat <<YML
    roles:
      main:
        health:
          test: ["CMD", "$health"]
          interval: 2s
          timeout: 2s
          start_period: $start
          retries: 3
jobs:
  long:
    app: web
    command: ["sh", "-c", "echo working; sleep 60"]
    timeout: 5m
YML
}

lock_json() { adm lock status --json 2>/dev/null; }
# lock_op: the operation that holds the deploy lock, or nothing.
lock_op() { lock_json | sed -n 's/^ *"operation_id": "\(.*\)".*/\1/p'; }
lock_free() { lock_json | grep -q '"locked": false'; }
# wait_until WHAT COMMAND...: runs COMMAND twice a second for a minute, until it succeeds.
wait_until() {
  local what=$1 i
  shift
  for i in $(seq 1 120); do
    "$@" && return 0
    sleep 0.5
  done
  fail "$what" "did not happen in time"
  return 1
}
have_op() { [ -n "$(lock_op)" ]; }
svc_image() { d service inspect --format '{{.Spec.TaskTemplate.ContainerSpec.Image}}' "$svc" 2>/dev/null; }
runs_image() { case $(svc_image) in *"${1#sha256:}"*) return 0 ;; esac; return 1; }
oneoff_running() { [ -n "$(d ps -q --filter label=berthy.environment=$ENVN --filter label=berthy.oneoff="$1" --filter status=running)" ]; }
run_containers() { d ps -aq --filter label=berthy.environment=$ENVN --filter label=berthy.oneoff=run; }
job_containers() { d ps -aq --filter label=berthy.environment=$ENVN --filter label=berthy.oneoff=job --filter label=berthy.job=long; }
healthy_count() { d ps -q --filter label=com.docker.swarm.service.name="$svc" --filter health=healthy | wc -l; }

manifest true 1s "" "" | write_manifest
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1" 0 "$code" "$out"

# 1. A pre-deploy command that runs for a minute. A cancel stops its container and the deploy, and leaves v1 serving.
manifest true 1s '["sleep","60"]' "" | write_manifest
adm deploy web --version v2 --manifest-dir "$MAN" > "$LOG/cancel-pre.out" 2>&1 &
pid=$!
wait_until "the deploy to hold the lock" have_op
id_pre=$(lock_op)
wait_until "the pre-deploy command to run" oneoff_running pre_hook
out=$(adm cancel "$id_pre" --dry-run 2>&1); code=$?
expect_code "a dry run of the cancel is accepted" 0 "$code" "$out"
expect_contains "the dry run says it would be accepted" "$out" "would be accepted"
expect_eq "the dry run did not stop the deploy" "$id_pre" "$(lock_op)"
start=$SECONDS
out=$(adm cancel "$id_pre" 2>&1); code=$?
expect_code "cancel during the pre-deploy command" 0 "$code" "$out"
expect_contains "the cancel is accepted" "$out" "cancel accepted"
expect_contains "the operation ended as failed" "$out" "ended: failed"
expect_contains "the reason says who cancelled" "$out" "cancelled by"
wait "$pid"; code=$?
expect_code "the cancelled deploy fails" 1 "$code" "$(cat "$LOG/cancel-pre.out")"
expect_contains "the deploy reports the cancel" "$(cat "$LOG/cancel-pre.out")" "cancelled by"
if [ $((SECONDS - start)) -lt 30 ]; then pass "the deploy stopped at once"; else fail "the deploy stopped at once" "took $((SECONDS - start))s"; fi
if oneoff_running pre_hook; then fail "the command's container is stopped" "still running"; else pass "the command's container is stopped"; fi
if runs_image "$WEB_V1_DIGEST"; then pass "the old version still serves"; else fail "the old version still serves" "$(svc_image)"; fi
if lock_free; then pass "the lock is free"; else fail "the lock is free" "$(lock_json)"; fi
out=$(adm cancel "$id_pre" 2>&1); code=$?
expect_code "a cancel of an operation that ended is refused" 1 "$code" "$out"
expect_contains "the refusal says it ended" "$out" "already ended: failed"

# 2. A new version that never becomes healthy (failures do not count for five minutes). A cancel during the wait puts v1 back.
manifest false 300s "" "" | write_manifest
adm deploy web --version v2 --manifest-dir "$MAN" > "$LOG/cancel-health.out" 2>&1 &
pid=$!
wait_until "the deploy to hold the lock" have_op
id_health=$(lock_op)
wait_until "the service to run v2" runs_image "$WEB_V2_DIGEST"
sleep 3
out=$(adm cancel "$id_health" 2>&1); code=$?
expect_code "cancel during the health wait" 0 "$code" "$out"
expect_contains "the deploy ended rolled back" "$out" "ended: rolled_back"
expect_contains "the reason says who cancelled" "$out" "cancelled by"
wait "$pid"; code=$?
expect_code "the cancelled deploy fails" 1 "$code" "$(cat "$LOG/cancel-health.out")"
expect_contains "the deploy reports the rollback" "$(cat "$LOG/cancel-health.out")" "rolled_back"
if runs_image "$WEB_V1_DIGEST"; then pass "the previous version is back"; else fail "the previous version is back" "$(svc_image)"; fi
wait_until "the previous version to be healthy" test "$(healthy_count)" -ge 1
if lock_free; then pass "the lock is free after the rollback"; else fail "the lock is free after the rollback" "$(lock_json)"; fi

# 3. A deploy that switched traffic: its post-deploy command runs. The cancel is refused with the reason and the deploy succeeds.
manifest true 1s "" '["sleep","8"]' | write_manifest
adm deploy web --version v2 --manifest-dir "$MAN" > "$LOG/cancel-post.out" 2>&1 &
pid=$!
wait_until "the deploy to hold the lock" have_op
id_post=$(lock_op)
wait_until "the post-deploy command to run" oneoff_running post_hook
out=$(adm cancel "$id_post" --dry-run 2>&1); code=$?
expect_code "the dry run of a late cancel exits 1" 1 "$code" "$out"
expect_contains "the dry run says it would be refused, and why" "$out" "would be refused: the deploy already switched traffic"
out=$(adm cancel "$id_post" 2>&1); code=$?
expect_code "a cancel after the switch is refused" 1 "$code" "$out"
expect_contains "the refusal gives the reason" "$out" "the deploy already switched traffic"
wait "$pid"; code=$?
expect_code "the deploy that refused the cancel succeeds" 0 "$code" "$(cat "$LOG/cancel-post.out")"
if runs_image "$WEB_V2_DIGEST"; then pass "v2 runs"; else fail "v2 runs" "$(svc_image)"; fi
out=$(adm cancel "$id_post" 2>&1); code=$?
expect_code "a cancel of a deploy that succeeded is refused" 1 "$code" "$out"
expect_contains "the refusal says it ended" "$out" "already ended: succeeded"
audit=$(adm audit 2>&1)
expect_contains "the accepted cancel is audited" "$audit" "cancel:$id_pre"
expect_contains "the refused cancel is audited" "$audit" "cancel:$id_post"

# 4. A run is cancelled and its container is gone.
adm run web -- sleep 60 > "$LOG/cancel-run.out" 2>&1 &
pid=$!
wait_until "the run to hold the lock" have_op
id_run=$(lock_op)
wait_until "the run's container to run" oneoff_running run
out=$(adm cancel "$id_run" 2>&1); code=$?
expect_code "cancel a run" 0 "$code" "$out"
expect_contains "the run ended as failed" "$out" "ended: failed"
wait "$pid"; code=$?
expect_code "the cancelled run fails" 1 "$code" "$(cat "$LOG/cancel-run.out")"
expect_eq "the run's container is gone" "" "$(run_containers)"
if lock_free; then pass "the lock is free after the run"; else fail "the lock is free after the run" "$(lock_json)"; fi

# 5. A job run is not an operation: `job cancel` names the job. Its container is stopped and removed, and the last lines it wrote
# are kept.
adm job run long > "$LOG/cancel-job.out" 2>&1 &
pid=$!
wait_until "the job's container to run" oneoff_running job
out=$(adm job cancel long 2>&1); code=$?
expect_code "cancel a job run" 0 "$code" "$out"
wait "$pid"; code=$?
expect_code "the cancelled job run fails" 1 "$code" "$(cat "$LOG/cancel-job.out")"
expect_contains "the run says who cancelled it" "$(cat "$LOG/cancel-job.out")" "cancelled by"
expect_eq "the job's container is gone" "" "$(job_containers)"
out=$(adm job logs long 2>&1); code=$?
expect_code "job logs of the cancelled run" 0 "$code" "$out"
expect_contains "the output it wrote is kept" "$out" "working"
out=$(adm job cancel long 2>&1); code=$?
expect_code "a job that is not running refuses a cancel" 1 "$code" "$out"
expect_contains "the refusal says why" "$out" "job is not running"

# 6. The terminal UI. The environment is protected, so the deploy of an agent token is held. A person approves it in the UI,
# presses c while it runs, confirms, and sees it end as cancelled.
make_ciadmin || fail "make an agent admin token" "token create failed"
manifest true 1s "" "" protected | write_manifest
out=$(adm deploy web --version "$WEB_V2_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "protect the environment with a digest deploy" 0 "$code" "$out"
manifest true 1s '["sleep","60"]' "" protected | write_manifest
out=$(cia deploy web --version "$WEB_V1_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "an agent's deploy is held" 3 "$code" "$out"

dir=$LOG/tui-cancel
rm -rf "$dir"
mkdir -p "$dir"
cat > "$dir/script.json" <<JSON
[
  {"sleep": "2s"},
  {"keys": ["4"]},
  {"wait_text": "deploy web", "timeout": "30s"},
  {"keys": ["a"]},
  {"wait_text": "Approving carries out the deploy", "timeout": "30s"},
  {"keys": ["y"]},
  {"wait_text": "c cancel the operation", "timeout": "60s"},
  {"snapshot": "following"},
  {"keys": ["c"]},
  {"wait_text": "Cancel operation", "timeout": "20s"},
  {"snapshot": "asking"},
  {"keys": ["y"]},
  {"wait_text": "was cancelled", "timeout": "90s"},
  {"snapshot": "cancelled"},
  {"keys": ["enter"]},
  {"sleep": "500ms"},
  {"keys": ["q"]},
  {"expect_exit": 0}
]
JSON
out=$("$TUI_BIN" --command "$BIN ui -e $ENVN --config $RUN/admin.json" --size 120x40 --script "$dir/script.json" --out "$dir/out" --timeout 180s --redact-env BERTHY_ADMIN_TOKEN 2>&1); code=$?
expect_code "the terminal UI approves the held deploy, cancels it and shows it cancelled" 0 "$code" "$out"
if lock_free; then pass "the lock is free after the cancel in the UI"; else fail "the lock is free after the cancel in the UI" "$(lock_json)"; fi
if runs_image "$WEB_V2_DIGEST"; then pass "the UI's cancel left v2 serving"; else fail "the UI's cancel left v2 serving" "$(svc_image)"; fi
if oneoff_running pre_hook; then fail "the UI's cancel stopped the command" "still running"; else pass "the UI's cancel stopped the command"; fi

finish
