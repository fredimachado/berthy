#!/usr/bin/env bash
set -euo pipefail

# Run on Linux with the dedicated daemon already in Swarm mode.
docker=${DOCKER_BIN:-docker}
go=${GO_BIN:-go}
: "${DOCKER_HOST:?Set DOCKER_HOST to the dedicated test daemon unix socket}"
[[ $DOCKER_HOST == unix:///* ]] || { echo 'DOCKER_HOST must name an explicit local unix socket' >&2; exit 1; }
root=$(cd "$(dirname "$0")/../.." && pwd)
name="berthy-ingress-$(date +%s)-$$"
trace="/var/tmp/$name"
mkdir -p "$trace"
echo "trace=$trace"
cleanup() {
  [[ ${load_pid:-0} == 0 ]] || kill "$load_pid" 2>/dev/null || true
  [[ ${events_pid:-0} == 0 ]] || kill "$events_pid" 2>/dev/null || true
  "$docker" rm -f "$name-proxy" >/dev/null 2>&1 || true
  "$docker" service rm "$name" >/dev/null 2>&1 || true
  for i in {1..30}; do "$docker" network rm "$name" >/dev/null 2>&1 && break; sleep 1; done
}
trap cleanup EXIT
CGO_ENABLED=0 "$go" build -o "$trace/fixture" "$root/test/phase1/ingress.go"
cat > "$trace/nginx.conf" <<EOF
events {}
http {
  upstream backend { server $name:8080; }
  server { listen 80; location / { proxy_http_version 1.1; proxy_set_header Connection close; proxy_next_upstream off; proxy_connect_timeout 1s; proxy_read_timeout 1s; proxy_pass http://backend; } }
}
EOF
"$docker" network create --driver overlay --attachable "$name" >/dev/null
"$docker" events --format '{{json .}}' > "$trace/events.jsonl" & events_pid=$!
"$docker" service create --name "$name" --network "$name" --replicas 1 --update-order start-first --update-parallelism 1 --update-max-failure-ratio 0 --update-failure-action rollback --update-monitor 20s --rollback-order start-first --rollback-parallelism 1 --health-cmd 'wget -q -O /dev/null http://127.0.0.1:8080/health' --health-interval 2s --health-timeout 1s --health-retries 2 --health-start-period 10s --mount "type=bind,src=$trace,dst=/trace" --env REVISION=old alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc /trace/fixture serve > "$trace/create.txt"
for i in {1..60}; do
  id=$("$docker" ps -q --filter "label=com.docker.swarm.service.name=$name" | head -1)
  if [[ -n $id ]] && [[ $("$docker" inspect -f '{{.State.Health.Status}}' "$id") == healthy ]]; then break; fi
  sleep 1
done
[[ -n ${id:-} ]] && [[ $("$docker" inspect -f '{{.State.Health.Status}}' "$id") == healthy ]]
"$docker" run -d --name "$name-proxy" --network "$name" -p 127.0.0.1::80 --mount "type=bind,src=$trace/nginx.conf,dst=/etc/nginx/nginx.conf,readonly" nginx@sha256:fed7e14be83cbd2661a75cdc5b872d936cf155d46c327084d30b8df19ae88d72 > "$trace/proxy.txt"
port=$("$docker" port "$name-proxy" 80/tcp | sed 's/.*://')
"$trace/fixture" load "http://127.0.0.1:$port/" > "$trace/requests.jsonl" & load_pid=$!
sleep 5
date -u +%FT%T.%NZ > "$trace/delayed-start"
"$docker" service update --env-add REVISION=delayed "$name" > "$trace/delayed-update.txt" 2>&1
date -u +%FT%T.%NZ > "$trace/delayed-end"
sleep 3
date -u +%FT%T.%NZ > "$trace/bad-start"
"$docker" service update --env-add REVISION=bad "$name" > "$trace/bad-update.txt" 2>&1 & update_pid=$!
for i in {1..30}; do
  for candidate in $("$docker" ps -q --filter "label=com.docker.swarm.service.name=$name"); do
    if [[ $("$docker" inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$candidate" | grep -c '^REVISION=bad$') == 1 ]]; then
      "$docker" exec "$candidate" wget -q -O - http://127.0.0.1:8080/ > "$trace/direct.txt" && break 2
    fi
  done
  sleep 1
done
wait "$update_pid" || true
"$docker" service inspect "$name" > "$trace/rollback.json"
date -u +%FT%T.%NZ > "$trace/bad-end"
sleep 3
date -u +%FT%T.%NZ > "$trace/control-start"
"$docker" service update --no-healthcheck --env-add REVISION=bad "$name" > "$trace/control-update.txt" 2>&1
date -u +%FT%T.%NZ > "$trace/control-end"
sleep 5
"$docker" service inspect "$name" > "$trace/service.json"
"$docker" service ps --no-trunc "$name" > "$trace/tasks.txt"
kill "$load_pid"; wait "$load_pid" 2>/dev/null || true; load_pid=0
kill "$events_pid"; wait "$events_pid" 2>/dev/null || true; events_pid=0
python3 "$root/test/phase1/ingress-report.py" "$trace"
