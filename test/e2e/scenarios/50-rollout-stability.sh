#!/bin/bash
# A candidate passes a probe, then exits while the rollout is still being monitored.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

svc="$PROJECT-$ENVN-web-main"
manifest() {
  cat <<YML
project: $PROJECT
network: $NETWORK
apps:
  web:
    kind: image
    image: $REG/e2e/web
    deploy:
      start_attempts: 1
    roles:
      main:
YML
  if [ "$1" = crash ]; then
    cat <<'YML'
        entrypoint: ["sh", "-c", "mkfifo /tmp/probe; read signal < /tmp/probe; sleep 3; exit 1"]
        health:
          test: ["CMD-SHELL", "test -p /tmp/probe && (test -e /tmp/signaled || (echo ready > /tmp/probe && touch /tmp/signaled))"]
          interval: 1s
          timeout: 2s
          retries: 3
YML
  else
    cat <<'YML'
        health:
          test: ["CMD", "true"]
          interval: 2s
          timeout: 2s
          retries: 3
YML
  fi
}
applied() {
  python3 - "$RUN/state/state.db" "$ENVN" <<'PY'
import sqlite3, sys
with sqlite3.connect('file:' + sys.argv[1] + '?mode=ro', uri=True) as db:
    row = db.execute('SELECT releases.digest FROM applied JOIN releases ON releases.id=applied.release_id WHERE applied.environment=? AND applied.app=?', (sys.argv[2], 'web')).fetchone()
    print(row[0] if row else '')
PY
}
live() { d service inspect --format '{{.Spec.TaskTemplate.ContainerSpec.Image}}' "$svc"; }
serves() { adm exec web -- wget -qO- http://127.0.0.1/ 2>&1; }

manifest healthy | write_manifest
out=$(adm deploy web --version "$WEB_V1_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
expect_code 'deploy healthy v1' 0 "$code" "$out"
previous=$(applied)
expect_contains 'v1 applied digest' "$previous" "$WEB_V1_DIGEST"
expect_contains 'v1 live digest' "$(live)" "$WEB_V1_DIGEST"
out=$(serves); code=$?
expect_code 'v1 serves HTTP' 0 "$code" "$out"
expect_contains 'v1 serves nginx' "$out" nginx

manifest crash | write_manifest
request="stability-$(openssl rand -hex 16)"
stamp=$(date +%s)
timeout 90 python3 -u - "$DOCKER_BIN" "$SOCK" "$svc" "$WEB_V2_DIGEST" "$stamp" > "$LOG/stability-docker-events.jsonl" <<'PY' &
import json, subprocess, sys
docker, sock, service, digest, stamp = sys.argv[1:]
base = [docker, '--host', 'unix://' + sock]
proc = subprocess.Popen(base + ['events', '--since', stamp, '--filter', 'type=container', '--filter', 'label=com.docker.swarm.service.name=' + service, '--format', '{{json .}}'], stdout=subprocess.PIPE, text=True)
healthy = set()
try:
    for line in proc.stdout:
        print(line, end='', flush=True)
        event = json.loads(line)
        container = event['Actor']['ID']
        action = event.get('Action', '')
        if action == 'health_status: healthy':
            image = subprocess.run(base + ['inspect', '--format', '{{.Config.Image}}', container], text=True, capture_output=True)
            if image.returncode == 0 and digest in image.stdout:
                healthy.add(container)
        if action == 'die' and container in healthy and event['Actor']['Attributes'].get('exitCode') not in ('0', '', None):
            sys.exit(0)
finally:
    proc.terminate()
    proc.wait()
sys.exit(1)
PY
events_pid=$!
trap 'kill "$events_pid" 2>/dev/null || true; wait "$events_pid" 2>/dev/null || true' EXIT
out=$(adm deploy web --version "$WEB_V2_DIGEST" --manifest-dir "$MAN" --request-id "$request" 2>&1); code=$?
printf '%s\n' "$out" > "$LOG/stability-deploy.out"
if wait "$events_pid"; then pass 'candidate passes a probe then exits nonzero'; else fail 'candidate passes a probe then exits nonzero' "$(cat "$LOG/stability-docker-events.jsonl")"; fi
trap - EXIT
d service ps --no-trunc "$svc" > "$LOG/stability-tasks.txt" 2>&1 || true
id=$(python3 - "$RUN/state/state.db" "$ENVN" "$request" <<'PY'
import sqlite3, sys
with sqlite3.connect('file:' + sys.argv[1] + '?mode=ro', uri=True) as db:
    row = db.execute('SELECT id FROM operations WHERE environment=? AND request_id=?', sys.argv[2:]).fetchone()
    print(row[0] if row else '')
PY
)
printf '%s\n' "$id" > "$LOG/stability-operation-id.txt"
if [ -z "$id" ]; then fail 'identify candidate operation' 'no operation for request'; finish; fi
pass 'identify candidate operation'
curl --noproxy '*' --fail --silent --show-error --max-time 30 -H "Authorization: Bearer $BERTHY_ADMIN_TOKEN" \
  "http://$AGENT_LISTEN/v1/projects/$PROJECT/environments/$ENVN/operations/$id/events?progress=1" > "$LOG/stability-events.sse"
if python3 - "$LOG/stability-events.sse" <<'PY'
import json, sys
records = []
for record in open(sys.argv[1]).read().split('\n\n'):
    fields = dict(line.split(': ', 1) for line in record.splitlines() if ': ' in line)
    if fields.get('event') == 'rollout':
        records.append(json.loads(fields['data']))
    if fields.get('event') == 'terminal':
        break
else:
    sys.exit(1)
sys.exit(0 if any(r.get('role') == 'main' and r.get('status') == 'started' for r in records) and not any(r.get('role') == 'main' and r.get('status') == 'ready' for r in records) else 1)
PY
then pass 'started without ready before terminal'; else fail 'started without ready before terminal' "$(cat "$LOG/stability-events.sse")"; fi
expect_code 'crashing deploy exits 1' 1 "$code" "$out"
expect_contains 'crashing deploy rolls back' "$out" rolled_back
expect_contains 'crashing deploy exhausts attempts' "$out" 'gave up after 1 failed starts'
expect_eq 'previous release remains applied' "$previous" "$(applied)"
expect_contains 'previous live digest restored' "$(live)" "$WEB_V1_DIGEST"
out=$(serves); code=$?
expect_code 'restored version serves HTTP' 0 "$code" "$out"
expect_contains 'restored version serves nginx' "$out" nginx
expect_contains 'deploy lock is free' "$(adm lock status --json 2>&1)" '"locked": false'
manifest healthy | write_manifest
out=$(adm deploy web --version "$WEB_V1_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
expect_code 'a later healthy deploy succeeds' 0 "$code" "$out"
finish
