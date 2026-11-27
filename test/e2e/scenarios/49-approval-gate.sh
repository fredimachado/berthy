#!/bin/bash
# 49-approval-gate reproduces two approval-gate findings on a real agent, and checks that the released v0.8.0 CLI still works.
# Finding 1: planning a change rewrites the body with dry_run; an empty or null body is not rewritten, so the route carries
# the change out (lock release, secret delete, cancel). Finding 2: the body decoder matches keys case-insensitively and lets
# the last duplicate win, so a request planned as one command can run another. The scenario asserts the fixed answers; on
# 9ecb7aa the checks the plan marks red fail. Part 4 is a pin that must pass throughout.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

URL="http://$AGENT_LISTEN/v1/projects/$PROJECT/environments/$ENVN"
INVALID='{"code":"bad_request","message":"invalid request"}'

# call TOKEN METHOD PATH [BODY]: one request, without a body when BODY is not given. Prints the HTTP status; the answer is
# in $RUN/gate.body.
call() {
  local token=$1 method=$2 path=$3
  if [ $# -ge 4 ]; then
    curl_local -o "$RUN/gate.body" -w '%{http_code}' -X "$method" -H "Authorization: Bearer $token" \
      -H 'Content-Type: application/json' --data-binary "$4" "$URL/$path"
  else
    curl_local -o "$RUN/gate.body" -w '%{http_code}' -X "$method" -H "Authorization: Bearer $token" "$URL/$path"
  fi
}
answer() { tr -d '\n' < "$RUN/gate.body"; }
admin_get() { curl_local -H "Authorization: Bearer $BERTHY_ADMIN_TOKEN" "$URL/$1"; }

# pending_ids: the ids of the pending approvals, one per line, sorted.
pending_ids() { admin_get 'approvals?status=pending' | grep -o '"id":"[^"]*"' | sort; }
# clear_pending: rejects every pending approval, so the next part starts with none.
clear_pending() {
  local id
  for id in $(pending_ids | sed 's/"id":"\(.*\)"/\1/'); do
    adm reject "$id" -m "scenario cleanup" >/dev/null 2>&1 || true
  done
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

# Part 1. Null bodies must not carry out a change, and must answer 400 invalid request.
make_ciadmin || { fail "make an agent admin token" "token create failed"; finish; }

{ echo "protected: true"; base_manifest; } | write_manifest
out=$(adm deploy web --version "$WEB_V2_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "protect the environment with a digest deploy" 0 "$code" "$out"
[ "$code" = 0 ] || finish

# 1.1 POST lock/release null while a person holds the lock.
out=$(adm lock acquire -m "held by a person" 2>&1); code=$?
expect_code "a person acquires the lock" 0 "$code" "$out"
before=$(pending_ids)
status=$(call "$BERTHY_CIADMIN_TOKEN" POST lock/release null)
expect_contains "1.1 the lock is still held" "$(admin_get lock)" '"locked":true'
expect_eq "1.1 lock/release null is refused" 400 "$status"
expect_eq "1.1 lock/release null answers invalid request" "$INVALID" "$(answer)"
expect_eq "1.1 lock/release null adds no approval" "$before" "$(pending_ids)"

# On 9ecb7aa the null release already freed the lock (finding 1); after the fix the person still holds it.
adm lock release >/dev/null 2>&1 || true
expect_contains "the lock is free before 1.2" "$(admin_get lock)" '"locked":false'

# 1.2 POST lock/acquire null while the lock is free.
before=$(pending_ids)
status=$(call "$BERTHY_CIADMIN_TOKEN" POST lock/acquire null)
expect_eq "1.2 lock/acquire null is refused" 400 "$status"
expect_eq "1.2 lock/acquire null answers invalid request" "$INVALID" "$(answer)"
expect_contains "1.2 the lock stays free" "$(admin_get lock)" '"locked":false'
expect_eq "1.2 lock/acquire null adds no approval" "$before" "$(pending_ids)"

# 1.4 POST secrets null.
before=$(pending_ids)
status=$(call "$BERTHY_CIADMIN_TOKEN" POST secrets null)
expect_eq "1.4 POST secrets null is refused" 400 "$status"
expect_eq "1.4 POST secrets null answers invalid request" "$INVALID" "$(answer)"
expect_eq "1.4 POST secrets null adds no approval" "$before" "$(pending_ids)"

# 1.3 DELETE secrets/GATE_KEEP null.
out=$(printf v1 | adm secret set GATE_KEEP 2>&1); code=$?
expect_code "an admin stores GATE_KEEP" 0 "$code" "$out"
expect_contains "GATE_KEEP is listed before" "$(admin_get secrets)" 'GATE_KEEP'
before=$(pending_ids)
status=$(call "$BERTHY_CIADMIN_TOKEN" DELETE secrets/GATE_KEEP null)
expect_contains "1.3 GATE_KEEP is still there" "$(admin_get secrets)" 'GATE_KEEP'
expect_eq "1.3 secret delete null is refused" 400 "$status"
expect_eq "1.3 secret delete null answers invalid request" "$INVALID" "$(answer)"
expect_eq "1.3 secret delete null adds no approval" "$before" "$(pending_ids)"

clear_pending

# Part 2. An agent's cancel with no body is held; a null body is refused. Approving carries the cancel out.
{
  echo "protected: true"
  cat <<YML
project: $PROJECT
apps:
  web:
    kind: image
    image: $REG/e2e/web
    deploy:
      deploy_timeout: 300s
    hooks:
      pre_deploy:
        - run: ["sleep", "60"]
    roles:
      main:
        health:
          test: ["CMD", "true"]
          interval: 2s
          timeout: 2s
          start_period: 1s
          retries: 3
YML
} | write_manifest

adm deploy web --version "$WEB_V1_DIGEST" --manifest-dir "$MAN" > "$LOG/gate-deploy.out" 2>&1 &
pid=$!
if ! wait_until "the deploy to hold the lock" have_op; then
  adm cancel "$(lock_op)" >/dev/null 2>&1 || true
  wait "$pid" || true
else
  id=$(lock_op)

  # 2.1 POST operations/<id>/cancel with no body. The hold is proved by lock_op and the audit, not by the 202 alone.
  status=$(call "$BERTHY_CIADMIN_TOKEN" POST "operations/$id/cancel")
  expect_eq "2.1 the deploy still runs" "$id" "$(lock_op)"
  expect_not_contains "2.1 nothing cancelled it" "$(adm audit 2>&1)" "cancel:$id"
  expect_eq "2.1 cancel with no body is held" 202 "$status"
  held=$(answer | sed -n 's/.*"approval_id":"\([^"]*\)".*/\1/p')
  expect_contains "2.1 the held answer names an approval" "$(answer)" '"approval_id":"'

  # 2.2 POST operations/<id>/cancel null.
  before=$(pending_ids)
  status=$(call "$BERTHY_CIADMIN_TOKEN" POST "operations/$id/cancel" null)
  expect_eq "2.2 cancel null is refused" 400 "$status"
  expect_eq "2.2 cancel null answers invalid request" "$INVALID" "$(answer)"
  expect_eq "2.2 the deploy still runs after null" "$id" "$(lock_op)"
  expect_eq "2.2 cancel null adds no approval" "$before" "$(pending_ids)"

  # 2.3 Approve the held cancel from 2.1. On an earlier failure, cancel as admin so wait never sits out the 60 s hook.
  out=$(adm approve "$held" 2>&1); code=$?
  expect_code "2.3 approve the held cancel" 0 "$code" "$out"
  if [ "$code" != 0 ]; then
    adm cancel "$id" >/dev/null 2>&1 || true
  fi
  wait "$pid"; wcode=$?
  if [ "$code" = 0 ]; then
    expect_code "2.3 the approved cancel ends the deploy" 1 "$wcode" "$(cat "$LOG/gate-deploy.out")"
    expect_contains "2.3 the deploy names who cancelled" "$(cat "$LOG/gate-deploy.out")" "cancelled by ci-admin (approved by admin)"
    if lock_free; then pass "2.3 the lock is free after the approved cancel"; else fail "2.3 the lock is free after the approved cancel" "$(lock_json)"; fi
  fi
  # On 9ecb7aa the empty-body cancel can leave the operation in recovery_required with the lock held.
  # Free it so part 3's finding-2 checks are not blocked by a conflict.
  if ! lock_free; then
    rid=$(lock_op)
    if [ -n "$rid" ]; then
      adm recovery resolve "$rid" -m "scenario cleanup" >/dev/null 2>&1 || true
      adm cancel "$rid" >/dev/null 2>&1 || true
    fi
  fi
fi

clear_pending
# Do not assert here: this only clears finding-1 fallout so part 3 can exercise finding 2.
for i in $(seq 1 120); do lock_free && break; sleep 0.5; done

# Part 3. A command sent twice under different spellings is refused for every token.
{ echo "protected: true"; base_manifest; } | write_manifest
before_audit=$(adm audit 2>&1)
ambig='{"app":"web","command":["true"],"Command":["sh","-c","echo gate-ambiguous"],"request_id":"gate-1"}'
before=$(pending_ids)
status=$(call "$BERTHY_CIADMIN_TOKEN" POST run "$ambig")
expect_eq "3.1 ambiguous run is refused" 400 "$status"
expect_eq "3.1 ambiguous run answers invalid request" "$INVALID" "$(answer)"
expect_eq "3.1 ambiguous run adds no approval" "$before" "$(pending_ids)"

ambig2='{"app":"web","command":["true"],"Command":["sh","-c","echo gate-ambiguous"],"request_id":"gate-2"}'
status=$(call "$BERTHY_ADMIN_TOKEN" POST run "$ambig2")
expect_eq "3.2 person's ambiguous run is refused" 400 "$status"
expect_eq "3.2 person's ambiguous run answers invalid request" "$INVALID" "$(answer)"
expect_eq "3.2 person's ambiguous run leaves the audit unchanged" "$before_audit" "$(adm audit 2>&1)"

clear_pending

# Part 4. The released v0.8.0 CLI still talks to this agent.
CLI080=$E2E_ROOT/bin/berthy-cli-v080
OLD_SRC=$E2E_ROOT/src-v0.8.0
rm -rf "$OLD_SRC" && mkdir -p "$OLD_SRC" || exit 1
git -c safe.directory="$REPO" -C "$REPO" archive v0.8.0 | tar -x -C "$OLD_SRC" || { echo "cannot export v0.8.0" >&2; exit 1; }
find "$OLD_SRC" -type f \( -name '*.go' -o -name go.mod -o -name go.sum \) -exec sed -i 's/\r$//' {} + || exit 1
stamped "$CLI080" v0.8.0 "$OLD_SRC" || exit 1

# old / oldcia: the v0.8.0 CLI as the admin and as ci-admin, shaped like adm.
old() { local c=$1; shift; [ "$c" != deploy ] || record_deploy "$PROJECT" "$ENVN" "$@" || return 1; "$CLI080" "$c" -e "$ENVN" --config "$RUN/admin.json" "$@"; }
oldcia() { local c=$1; shift; [ "$c" != deploy ] || record_deploy "$PROJECT" "$ENVN" "$@" || return 1; "$CLI080" "$c" -e "$ENVN" --config "$RUN/ciadmin.json" "$@"; }

out=$(old lock acquire -m compat 2>&1); code=$?
expect_code "v0.8.0 lock acquire" 0 "$code" "$out"
out=$(old lock release 2>&1); code=$?
expect_code "v0.8.0 lock release" 0 "$code" "$out"

out=$(printf x | old secret set COMPAT_A 2>&1); code=$?
expect_code "v0.8.0 secret set" 0 "$code" "$out"
out=$(old secret delete COMPAT_A 2>&1); code=$?
expect_code "v0.8.0 secret delete" 0 "$code" "$out"

out=$(printf 'COMPAT_B=x\n' | old secret import --dry-run 2>&1); code=$?
expect_code "v0.8.0 secret import --dry-run" 0 "$code" "$out"
out=$(old secret generate COMPAT_C --dry-run --manifest-dir "$MAN" 2>&1); code=$?
expect_code "v0.8.0 secret generate --dry-run" 0 "$code" "$out"

out=$(old deploy web --version "$WEB_V1_DIGEST" --dry-run --manifest-dir "$MAN" 2>&1); code=$?
expect_code "v0.8.0 deploy --dry-run" 0 "$code" "$out"
out=$(old apply --dry-run --manifest-dir "$MAN" 2>&1); code=$?
expect_code "v0.8.0 apply --dry-run" 0 "$code" "$out"

out=$(old token create compat --dry-run 2>&1); code=$?
expect_code "v0.8.0 token create --dry-run" 0 "$code" "$out"
out=$(old token revoke ci-admin --dry-run 2>&1); code=$?
expect_code "v0.8.0 token revoke --dry-run" 0 "$code" "$out"
out=$(old key rotate --dry-run 2>&1); code=$?
expect_code "v0.8.0 key rotate --dry-run" 0 "$code" "$out"
out=$(old approvals 2>&1); code=$?
expect_code "v0.8.0 approvals" 0 "$code" "$out"

out=$(oldcia deploy web --version "$WEB_V1_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "v0.8.0 agent's deploy is held" 3 "$code" "$out"
id=$(printf '%s\n' "$out" | sed -n 's/^held for approval: \([^ ]*\) .*/\1/p')
out=$(old reject "$id" -m compat 2>&1); code=$?
expect_code "v0.8.0 reject the held deploy" 0 "$code" "$out"

finish
