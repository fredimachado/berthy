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
  hook_requests | jq -ce '
    first(.[] | select(.method == "POST") | .body | fromjson |
      select(.kind == "check" and (.server | type == "string" and length > 0) and (has("environment") | not) and .name == "memory"))'
}

warning_delivered() {
  memory_post | jq -e '.state == "warning" and .detail == "9.9% available"' >/dev/null || return 1
  "$CLUSTER_BIN" notified "$RUN/cluster/state/state.db" memory warning
}

write_memory() {
  printf 'MemTotal: 1000 kB\nMemAvailable: %s kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n' "$1" > "$meminfo"
}

existing=$(d ps -aq --no-trunc --filter "name=^/${name}\$") || exit 1
[ -z "$existing" ] || { fail "create memory agent" "container name already exists"; finish; }
stop_agent
jq '.cluster.alerts = {reminder: "1ms", webhook: {url_secret: "ALERT_WEBHOOK_URL"}}' "$RUN/agent.json" > "$RUN/agent.json.new" &&
  mv -f "$RUN/agent.json.new" "$RUN/agent.json" || exit 1
printf 'http://%s/memory' "$HOOK_LISTEN" | "$CLUSTER_BIN" secret "$RUN/agent.json" ALERT_WEBHOOK_URL || exit 1
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
out=$(adm checks --cluster --json 2>&1); code=$?
expect_code "read checks after passing memory sample" 0 "$code" "$out"
expect_eq "memory retains its warning and detail" 'warning 9.9% available' \
  "$(jq -r '.[] | select(.name == "memory") | "\(.state) \(.detail)"' <<< "$out")"
label=$(d inspect -f '{{index .Config.Labels "berthy.e2e.fixture"}}' "$name" 2>/dev/null) || label=
if [ "$label" != "$owner" ]; then
  fail "remove memory agent fixture" "container label is missing or foreign"
elif d rm -f "$name" >/dev/null; then
  pass "removed memory agent fixture"
else
  fail "remove memory agent fixture" "docker rm failed"
fi
finish
