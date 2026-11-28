#!/bin/bash
# 69-host-alert-recovery checks that a passing memory sample does not immediately clear a warning.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

name="$PROJECT-$ENVN-memory-agent"
owner="$FIXTURE_ID.$(openssl rand -hex 8)"
meminfo="$RUN/meminfo"

cleanup() {
  local label
  label=$(d inspect -f '{{index .Config.Labels "berthy.e2e.fixture"}}' "$name" 2>/dev/null) || return 0
  [ "$label" = "$owner" ] || return 0
  d rm -f "$name" >/dev/null 2>&1 || echo "could not remove memory agent fixture" >&2
}
trap cleanup EXIT

memory_post() {
  hook_requests | jq -ce --arg env "$ENVN" '
    first(.[] | select(.method == "POST") | .body | fromjson |
      select(.kind == "check" and .environment == $env and .name == "memory"))'
}

warning_delivered() {
  memory_post | jq -e '.state == "warning" and .detail == "9.9% available"' >/dev/null || return 1
  python3 - "$RUN/state/state.db" "$ENVN" <<'PY'
import sqlite3, sys
with sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True) as db:
    row = db.execute("SELECT notified_state FROM checks WHERE environment=? AND name='memory'", (sys.argv[2],)).fetchone()
sys.exit(row != ("warning",))
PY
}

write_memory() {
  printf 'MemTotal: 1000 kB\nMemAvailable: %s kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n' "$1" > "$meminfo"
}

out=$(printf 'http://%s/memory' "$HOOK_LISTEN" | adm secret set ALERT_WEBHOOK_URL 2>&1); code=$?
expect_code "configure webhook secret" 0 "$code" "$out"
[ "$code" -eq 0 ] || finish
{
  base_manifest
  cat <<'YML'
alerts:
  reminder: 1s
  webhook:
    url_secret: ALERT_WEBHOOK_URL
YML
} | write_manifest || exit 1
out=$(adm apply --manifest-dir "$MAN" 2>&1); code=$?
expect_code "apply webhook settings" 0 "$code" "$out"
[ "$code" -eq 0 ] || finish

existing=$(d ps -aq --no-trunc --filter "name=^/${name}\$") || exit 1
[ -z "$existing" ] || { fail "create memory agent" "container name already exists"; finish; }
stop_agent
write_memory 99 || exit 1
hook_reset || exit 1
groups=()
for group in $(id -G); do groups+=(--group-add "$group"); done
socket_group=$(stat -c '%g' "$SOCK") || exit 1
groups+=(--group-add "$socket_group")
out=$(d create --name "$name" --label "berthy.e2e.fixture=$owner" --network host \
  --user "$(id -u):$(id -g)" "${groups[@]}" \
  --mount "type=bind,src=$BIN,dst=$BIN,readonly" \
  --mount "type=bind,src=$RUN,dst=$RUN" \
  --mount "type=bind,src=$SOCK,dst=$SOCK" \
  --mount "type=bind,src=$COMPOSE_BIN,dst=$COMPOSE_BIN,readonly" \
  --mount "type=bind,src=$meminfo,dst=/proc/meminfo,readonly" \
  --entrypoint "$BIN" "$REG/e2e/web@$WEB_V1_DIGEST" agent --config "$RUN/agent.json" 2>&1); code=$?
expect_code "create memory agent" 0 "$code" "$out"
[ "$code" -eq 0 ] || finish
d start "$name" >/dev/null || { fail "start memory agent" "docker start failed"; finish; }
if wait_for "memory warning delivery" warning_delivered; then
  pass "memory warning was delivered and recorded"
else
  fail "memory warning delivery" "warning or delivery checkpoint missing"
  d logs "$name" >&2
  finish
fi

d stop "$name" >/dev/null || { fail "stop memory agent" "docker stop failed"; finish; }
write_memory 101 || exit 1
hook_reset || exit 1
d start "$name" >/dev/null || { fail "restart memory agent" "docker start failed"; finish; }
if wait_for "first memory post after restart" memory_post; then
  post=$(memory_post)
  expect_eq "first memory post remains a warning" warning "$(jq -r '.state' <<< "$post")"
  expect_eq "first memory post retains the warning detail" '9.9% available' "$(jq -r '.detail' <<< "$post")"
  expect_contains "first memory post is a reminder" "$(jq -r '.text' <<< "$post")" 'still failing'
else
  fail "first memory post after restart" "no memory post received"
fi
out=$(adm checks --json 2>&1); code=$?
expect_code "read checks after passing memory sample" 0 "$code" "$out"
expect_eq "memory retains its warning and detail" 'warning 9.9% available' \
  "$(jq -r '.[] | select(.name == "memory") | "\(.state) \(.detail)"' <<< "$out")"
finish
