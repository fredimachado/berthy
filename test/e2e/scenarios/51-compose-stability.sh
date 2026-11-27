#!/bin/bash
# A worker without a health check must stay up through the readiness hold. An immediate crash loop fails with its exit code.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

proj="$PROJECT-$ENVN-db"
budget=120
write_manifest <<YML || exit 1
project: $PROJECT
network: $NETWORK
apps:
  db:
    kind: compose
    file: compose.yml
    deploy:
      deploy_timeout: ${budget}s
YML

write_compose() {
  cat > "$MAN/compose.yml" <<YML
services:
  worker:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sh", "-c", "$1"]
    restart: unless-stopped
    healthcheck:
      disable: true
YML
}

applied() {
  python3 - "$RUN/state/state.db" "$ENVN" <<'PY'
import sqlite3, sys
with sqlite3.connect('file:' + sys.argv[1] + '?mode=ro', uri=True) as db:
    row = db.execute("SELECT release_id FROM applied WHERE environment=? AND app='db'", (sys.argv[2],)).fetchone()
    print(row[0] if row else 'none')
PY
}

# Replay from before deploy so a fast exit is visible regardless of when the reader connects.
worker_events() {
  DOCKER_CONFIG="$E2E_ROOT/docker-config" timeout --kill-after=1s 30s python3 - "$DOCKER_BIN" "$SOCK" "$proj" "$1" <<'PY'
import json, subprocess, sys
docker, sock, project, since = sys.argv[1:]
proc = subprocess.Popen([docker, '--host', 'unix://' + sock, 'events', '--since', since,
    '--filter', 'type=container', '--filter', 'label=com.docker.compose.project=' + project,
    '--filter', 'label=com.docker.compose.service=worker', '--filter', 'event=start',
    '--filter', 'event=die', '--format', '{{json .}}'], stdout=subprocess.PIPE, text=True)
try:
    starts = {}
    for line in proc.stdout:
        event = json.loads(line)
        container = event['Actor']['ID']
        if event['Action'] == 'start':
            starts[container] = line
        elif container in starts:
            print(starts[container] + line, end='')
            sys.exit(0 if event['Actor']['Attributes'].get('exitCode') == '3' else 1)
finally:
    proc.terminate()
    proc.wait()
sys.exit(1)
PY
}

deploy_worker() {
  local attempt=$1 request before after code out operation elapsed candidates
  request="compose-stability-$attempt-$(openssl rand -hex 16)"
  before=$(applied) || exit 1
  out=$(adm deploy db --manifest-dir "$MAN" --request-id "$request" 2>&1); code=$?
  after=$(applied) || exit 1
  printf '%s\n' "$out" > "$LOG/compose-stability-$attempt-deploy.out"
  printf '  %s worker CLI exit=%s applied release before=%s after=%s\n' "$attempt" "$code" "$before" "$after"
  expect_code "$attempt worker deploy fails" 1 "$code" "$out"
  expect_contains "$attempt worker failure names the service and exit code" "$out" 'service worker exited with code 3'
  expect_eq "$attempt worker leaves the applied release unchanged" "$before" "$after"

  if operation=$(python3 - "$RUN/state/state.db" "$ENVN" "$request" <<'PY'
import sqlite3, sys
with sqlite3.connect('file:' + sys.argv[1] + '?mode=ro', uri=True) as db:
    row = db.execute('''SELECT e.created - o.started,
        (SELECT count(*) FROM releases r WHERE r.operation_id=o.id), o.state, o.error
        FROM operations o JOIN events e ON e.operation_id=o.id AND e.kind=o.state
        WHERE o.environment=? AND o.request_id=?''', sys.argv[2:]).fetchone()
    if row is None:
        sys.exit('no terminal operation for request')
    print(*row, sep='\t')
PY
  ); then
    printf '  %s worker operation (ms, candidate releases, state, reason) %s\n' "$attempt" "$operation" |
      tee "$LOG/compose-stability-$attempt-operation.txt"
    read -r elapsed candidates _ <<< "$operation"
    expect_eq "$attempt worker records no candidate release" 0 "$candidates"
    if [ "$attempt" = immediate ]; then
      if (( elapsed < budget * 1000 )); then
        pass 'immediate worker operation ends before the deploy deadline'
      else
        fail 'immediate worker operation ends before the deploy deadline' "operation took ${elapsed} ms with a ${budget}s budget"
      fi
    fi
  else
    fail "$attempt worker has a terminal operation" 'no terminal operation for request'
  fi
}

write_compose 'sleep 3; exit 3' || exit 1
stamp=$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)
deploy_worker delayed
events="$LOG/compose-stability-delayed-docker-events.jsonl"
if worker_events "$stamp" > "$events"; then
  pass 'delayed worker starts and exits with code 3 on the same container'
else
  fail 'delayed worker starts and exits with code 3 on the same container' "$(cat "$events")"
fi

write_compose 'exit 3' || exit 1
deploy_worker immediate
d ps --all --no-trunc --filter "label=com.docker.compose.project=$proj" \
  --filter label=com.docker.compose.service=worker > "$LOG/compose-stability-immediate-containers.txt" 2>&1
finish
