#!/bin/bash
# 21-stop-first proves the update order of a role that mounts a named volume read-write, on the real Swarm. Such a role is
# updated stop-first: the old version's container stops before the new one starts, so two versions never write the volume at
# once. The dry run says so and why, the service's update and rollback configs carry "stop-first", and the containers' times
# show the handover. A read-only mount keeps the start-first default. `config validate` warns about deploy.order start-first
# over a read-write volume and still passes, and the setting is sent. A deploy whose health check fails ends rolled back to
# the previous version, and its reason says the role was down in between.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

svc="$PROJECT-$ENVN-web-main"
vol="uploads"

# manifest HEALTH MOUNT ORDER: the web app, whose main role mounts MOUNT and runs the health check HEALTH ("true" passes,
# "false" fails). ORDER, when it is not empty, is written as the app's deploy.order.
manifest() {
  local deploy=""
  # $(...) drops a trailing newline, so the newline is added back: the roles key must start its own line.
  if [ -n "${3:-}" ]; then deploy=$(printf '    deploy:\n      order: %s' "$3")$'\n'; fi
  cat <<YML
project: $PROJECT
network: $NETWORK
apps:
  web:
    kind: image
    image: $REG/e2e/web
${deploy}    roles:
      main:
        volumes:
          - $2
        health:
          test: ["CMD", "$1"]
          interval: 2s
          timeout: 2s
          start_period: 1s
          retries: 3
YML
}

# running_count: how many containers of the main role's service run now.
running_count() { d ps -q --filter label=com.docker.swarm.service.name="$svc" | wc -l; }

# times_of DIGEST: "StartedAt FinishedAt" of the newest container of the role's service that runs DIGEST, or nothing.
times_of() {
  local id
  for id in $(d ps -a -q --no-trunc --filter label=com.docker.swarm.service.name="$svc"); do
    case $(d inspect --format '{{.Config.Image}}' "$id") in
      *"$1"*)
        d inspect --format '{{.State.StartedAt}} {{.State.FinishedAt}}' "$id"
        return 0
        ;;
    esac
  done
}

# not_after A B: true when the time A is not after the time B. Both are RFC 3339 with nanoseconds, as docker prints them.
not_after() {
  awk -v a="$(date -d "$1" +%s.%N)" -v b="$(date -d "$2" +%s.%N)" 'BEGIN { exit !(a + 0 <= b + 0) }'
}

# 1. A read-only mount keeps the start-first default: the dry run names no stop-first role.
manifest true "$vol:/data:ro" "" | write_manifest
out=$(adm deploy web --version v1 --dry-run --manifest-dir "$MAN" 2>&1); code=$?
expect_code "a dry run with a read-only mount exits 0" 0 "$code" "$out"
expect_not_contains "a read-only mount keeps the start-first default" "$out" "stop-first"

# 2. A read-write mount is stop-first, and the dry run says why. The dry run creates no service.
manifest true "$vol:/data" "" | write_manifest
out=$(adm deploy web --version v1 --dry-run --manifest-dir "$MAN" 2>&1); code=$?
expect_code "a dry run with a read-write mount exits 0" 0 "$code" "$out"
expect_contains "the dry run says the role stops first, and why" "$out" "role main: stop-first, because it mounts volume $vol"
expect_eq "the dry run creates no service" "0" "$(d service ls -q --filter label=berthy.environment=$ENVN | wc -l)"

# 3. Deploying v1 creates the service with stop-first in both its update and its rollback config.
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1 with a read-write volume" 0 "$code" "$out"
spec=$(service_spec "$svc")
upd=$(printf '%s' "$spec" | grep -o '"UpdateConfig":{[^}]*}')
rb=$(printf '%s' "$spec" | grep -o '"RollbackConfig":{[^}]*}')
expect_contains "the update config is stop-first" "$upd" '"Order":"stop-first"'
expect_contains "the rollback config is stop-first" "$rb" '"Order":"stop-first"'

# 4. Updating to v2 stops the v1 container before the v2 container starts: the two never run together.
out=$(adm deploy web --version v2 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v2 over a stop-first service" 0 "$code" "$out"
spec=$(service_spec "$svc")
expect_contains "the service runs the v2 digest" "$spec" "$WEB_V2_DIGEST"
read -r _ v1_finished <<< "$(times_of "$WEB_V1_DIGEST")"
read -r v2_started _ <<< "$(times_of "$WEB_V2_DIGEST")"
if [ -n "$v1_finished" ] && [ "${v1_finished%%-*}" != 0001 ] && [ -n "$v2_started" ] && not_after "$v1_finished" "$v2_started"; then
  pass "the v1 container stopped before the v2 container started"
else
  fail "the v1 container stopped before the v2 container started" "v1 finished '$v1_finished', v2 started '$v2_started'"
fi

# 5. A deploy whose health check fails is rolled back: the previous version runs again, and the reason says the role was
# down until it came back.
manifest false "$vol:/data" "" | write_manifest
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "a deploy whose health check fails exits 1" 1 "$code" "$out"
expect_contains "the failed deploy says the role was stopped first and down until the previous version came back" "$out" \
  "role main was stopped first, so it was down until the previous version came back"
spec=$(service_spec "$svc")
expect_contains "the previous version runs again" "$spec" "$WEB_V2_DIGEST"
expect_not_contains "the failed version is not left in the service" "$spec" "$WEB_V1_DIGEST"
expect_eq "one container runs after the rollback" "1" "$(running_count)"

# 6. config validate warns about start-first over a read-write volume, and still passes. The setting is accepted and sent.
manifest true "$vol:/data" start-first | write_manifest
out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "config validate passes with a start-first warning" 0 "$code" "$out"
expect_contains "config validate warns with the file, line and setting" "$out" "warning: berthy.yml:"
expect_contains "the warning names the setting and the volume" "$out" \
  "apps.web.deploy.order: start-first lets two versions of role main write volume $vol at once"
expect_contains "config validate still says the manifest is valid" "$out" "manifest valid"
json=$("$BIN" config validate -e "$ENVN" --manifest-dir "$MAN" --json 2>/dev/null); code=$?
expect_code "config validate --json exits 0" 0 "$code" "$json"
expect_contains "--json carries the warning" "$json" '"path": "apps.web.deploy.order"'

out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1 with deploy.order start-first" 0 "$code" "$out"
spec=$(service_spec "$svc")
upd=$(printf '%s' "$spec" | grep -o '"UpdateConfig":{[^}]*}')
expect_contains "deploy.order start-first is sent to the update config" "$upd" '"Order":"start-first"'
expect_contains "the service runs the v1 digest" "$spec" "$WEB_V1_DIGEST"

finish
