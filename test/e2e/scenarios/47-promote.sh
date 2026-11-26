#!/bin/bash
# Promote an applied snapshot between independently authenticated agents on one fixture Swarm.
set -uo pipefail
umask 077
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"
DEST=${ENVN}c
IMAGE="$REG/e2e/web"
A="$IMAGE@$WEB_V1_DIGEST"
B="$IMAGE@$WEB_V2_DIGEST"
RUN_ID="promote-$(openssl rand -hex 16)"
SERVICE="$PROJECT-$DEST-web-main"
COLLISION_FILE="$RUN/promotion-collision-id"
preview_proxy_pid=

# Stop the agent before inspecting its private durable admissions, then delete only a service
# whose current scope and generation match an operation admitted under this run's request IDs.
# An inspect failure proves absence only if a complete, warning-free inventory excludes the name.
service_absent() {
  local listing errors status
  listing=$(mktemp "$RUN/service-inventory.XXXXXX") || return 1
  errors=$(mktemp "$RUN/service-errors.XXXXXX") || { rm -f "$listing"; return 1; }
  if ! d service ls --format '{{json .}}' >"$listing" 2>"$errors" || [ -s "$errors" ]; then
    echo 'destination service inventory unavailable' >&2
    rm -f "$listing" "$errors"
    return 1
  fi
  python3 - "$listing" "$SERVICE" <<'PYLIST'
import json, sys
path, target = sys.argv[1:]
try:
    with open(path) as f: rows = [json.loads(line) for line in f if line.strip()]
    if any(not isinstance(r, dict) or not isinstance(r.get('ID'), str) or not r['ID'] or not isinstance(r.get('Name'), str) or not r['Name'] for r in rows): raise ValueError()
    if len({r['ID'] for r in rows}) != len(rows) or len({r['Name'] for r in rows}) != len(rows): raise ValueError()
    sys.exit(1 if any(r['Name'] == target for r in rows) else 0)
except (ValueError, OSError):
    sys.exit(2)
PYLIST
  status=$?
  rm -f "$listing" "$errors"
  if [ "$status" -ne 0 ]; then echo 'destination service lookup is uncertain' >&2; return 1; fi
}
cleanup_owned_services() {
  local id spec generation owned
  [ -f "$RUN/state2/state.db" ] || return 0
  owned=$(python3 - "$RUN/state2/state.db" "$DEST" "$RUN_ID" <<'PYSQL'
import sqlite3, sys
path, env, marker = sys.argv[1:]
con = sqlite3.connect('file:' + path + '?mode=ro', uri=True)
for (op,) in con.execute("SELECT o.id FROM operations o JOIN audit a ON a.environment=o.environment AND a.operation_id=o.id WHERE o.environment=? AND substr(o.request_id,1,?)=? AND a.outcome='accepted'", (env, len(marker)+1, marker+'-')):
    print(op)
PYSQL
  ) || return 1
  if ! spec=$(d service inspect --format '{{json .}}' "$SERVICE" 2>/dev/null); then
    service_absent
    return $?
  fi
  jq -e --arg name "$SERVICE" 'type == "object" and (.ID | type == "string" and length == 25) and (.Spec | type == "object") and .Spec.Name == $name and (.Spec.Labels | type == "object")' <<<"$spec" >/dev/null || { echo 'invalid destination service inspection' >&2; return 1; }
  id=$(jq -er '.ID' <<<"$spec") || return 1
  generation=$(jq -er --arg project "$PROJECT" --arg env "$DEST" --arg name "$SERVICE" '
    select(.Spec.Name == $name and .Spec.Labels["berthy.project"] == $project and .Spec.Labels["berthy.environment"] == $env and .Spec.Labels["berthy.app"] == "web" and .Spec.Labels["berthy.role"] == "main") |
    .Spec.Labels["berthy.operation"] | select(type == "string" and length > 0)' <<<"$spec") || return 0
  if grep -Fxq -- "$generation" <<<"$owned"; then d service rm "$id" >/dev/null || return 1; fi
}
cleanup_destination() {
  local collision spec full_id labels collisions status=0
  if [ -n "$preview_proxy_pid" ]; then kill "$preview_proxy_pid" 2>/dev/null || :; wait "$preview_proxy_pid" 2>/dev/null || :; fi
  stop_agent2 || status=1
  cleanup_owned_services || status=1
  # The daemon can create the intentional collision before the child records its ID.
  if ! collisions=$(d service ls --format '{{.ID}}' --filter "label=fixture.promotion.collision=$RUN_ID"); then
    echo 'collision inventory unavailable' >&2
    status=1
    collisions=
  fi
  while IFS= read -r collision; do
    [ -n "$collision" ] || continue
    if ! spec=$(d service inspect --format '{{json .}}' "$collision" 2>/dev/null); then status=1; continue; fi
    if ! full_id=$(jq -er '.ID | select(type == "string" and length == 25)' <<<"$spec"); then status=1; continue; fi
    if ! labels=$(jq -c '.Spec.Labels' <<<"$spec"); then status=1; continue; fi
    if jq -e --arg marker "$RUN_ID" '.["fixture.promotion.collision"] == $marker' <<<"$labels" >/dev/null 2>&1; then
      d service rm "$full_id" >/dev/null || status=1
    else
      echo 'collision ownership is uncertain' >&2
      status=1
    fi
  done <<< "$collisions"
  [ "$status" -ne 0 ] || rm -f "$COLLISION_FILE"
  return "$status"
}

# Exercise an inspect failure and independent collision cleanup without using Docker.
selftest_cleanup() (
  local temp
  temp=$(mktemp -d "$RUN/cleanup-test.XXXXXX") || return 1
  trap 'rm -rf "$temp"' EXIT
  RUN=$temp
  preview_proxy_pid=
  mkdir -p "$RUN/state2"
  python3 - "$RUN/state2/state.db" <<'PYDB'
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
con.executescript('CREATE TABLE operations(id TEXT, environment TEXT, request_id TEXT); CREATE TABLE audit(operation_id TEXT, environment TEXT, outcome TEXT);')
con.commit()
PYDB
  stop_agent2() { :; }
  local inventory_case=present
  d() {
    case "$1 $2 ${3:-}" in
      "service inspect --format")
        if [ "$5" = "$SERVICE" ]; then return 1; fi
        printf '{"ID":"aaaaaaaaaaaaaaaaaaaaaaaaa","Spec":{"Labels":{"fixture.promotion.collision":"%s"}}}\n' "$RUN_ID" ;;
      "service ls --format")
        case "$*" in
          *fixture.promotion.collision*) echo aaaaaaaaaaaaaaaaaaaaaaaaa ;;
          *) case "$inventory_case" in
            present) printf '{"ID":"bbbbbbbbbbbb","Name":"%s"}\n' "$SERVICE" ;;
            absent) : ;;
            malformed) echo '{broken' ;;
            warning) echo warning >&2 ;;
            error) return 1 ;;
          esac ;;
        esac ;;
      "service rm aaaaaaaaaaaaaaaaaaaaaaaaa") touch "$RUN/removed" ;;
      *) return 1 ;;
    esac
  }
  if cleanup_destination; then return 1; fi
  [ -f "$RUN/removed" ] || return 1
  inventory_case=absent; service_absent || return 1
  for inventory_case in present malformed warning error; do
    if service_absent; then return 1; fi
  done
)
if [ "${PROMOTE_CLEANUP_SELFTEST:-}" = 1 ]; then selftest_cleanup; exit $?; fi
cleanup() { local status=$?; trap - EXIT; cleanup_destination || status=1; exit "$status"; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
start_agent2 || { fail 'start second agent' 'fixture failed'; finish; }

# Every command uses the one fixture config; each environment resolves its own token.
cmd() {
  local env=$1 verb=$2
  shift 2
  local -a manifest_arg=() request_arg=()
  if [ "$verb" = deploy ] && [ "$env" = "$DEST" ]; then request_arg=(--request-id "$RUN_ID-$(openssl rand -hex 8)"); fi
  case $verb in deploy|promote) manifest_arg=(--manifest-dir "$MAN");; esac
  (cd "$MAN" && { [ "$verb" != deploy ] || [ "$env" = "$DEST" ] || record_deploy "$PROJECT" "$env" "$@"; } &&
    "$BIN" "$verb" -e "$env" --project "$PROJECT" --config "$RUN/admin.json" "${manifest_arg[@]}" "${request_arg[@]}" "$@")
}
history() { cmd "$DEST" history web --json; }
# Refusals must leave both admissions and approval state unchanged; failed or malformed reads fail closed.
guard_snapshot() {
  local audit approvals
  audit=$(cmd "$DEST" audit --json) || return 1
  approvals=$(cmd "$DEST" approvals --all --json) || return 1
  jq -se 'length == 1 and (.[0] | type == "array" and all(.[]; (.operation_id | type == "string") and (.action | type == "string") and (.outcome | type == "string")))' <<<"$audit" >/dev/null || return 1
  jq -se 'length == 1 and (.[0] | type == "array" and all(.[]; (.id | type == "string" and length > 0) and (.status | type == "string" and length > 0)))' <<<"$approvals" >/dev/null || return 1
  jq -cnS --argjson audit "$audit" --argjson approvals "$approvals" '{accepted: [$audit[] | select(.outcome == "accepted") | {operation_id,action,outcome}] | sort_by(.operation_id), approvals: [$approvals[] | {id,status}] | sort_by(.id)}'
}
unchanged() {
  local label=$1 before=$2 after
  after=$(guard_snapshot) || { fail "$label" 'audit or approvals could not be read'; finish; }
  expect_eq "$label" "$before" "$after"
}
applied() {
  local token_var
  token_var=$(env_var "$PROJECT" "$DEST")
  curl_local -f -H "Authorization: Bearer ${!token_var}" "http://$AGENT2_LISTEN/v1/projects/$PROJECT/environments/$DEST/applied/web"
}

printf 'apps:\n  web:\n    env:\n      plain:\n        SNAPSHOT: before\n' > "$MAN/berthy.$DEST.yml"
out=$(cmd "$ENVN" deploy web --version v1 2>&1); code=$?
expect_code 'source applies image A under v1' 0 "$code" "$out"
[ "$code" -eq 0 ] || finish

# Retag v1 to B: the source's applied pointer still identifies A and its label.
curl_local -f -D "$RUN/v2.headers" -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json' \
  "$REG/v2/e2e/web/manifests/v2" -o "$RUN/v2.manifest" || { fail 'read B manifest' 'registry request failed'; finish; }
media=$(sed -n 's/^[Cc]ontent-[Tt]ype: *\([^;\r]*\).*/\1/p' "$RUN/v2.headers" | tr -d '\r' | tail -n 1)
curl_local -f -X PUT -H "Content-Type: $media" --data-binary "@$RUN/v2.manifest" "$REG/v2/e2e/web/manifests/v1" -o /dev/null || { fail 'retag v1 to B' 'registry request failed'; finish; }

# This is intentionally the first promotion call: before dispatch exists it is a clean red.
out=$(cmd "$DEST" promote web --from "$ENVN" --dry-run --json 2>&1); code=$?
expect_code 'promotion dry-run returns one JSON plan' 0 "$code" "$out"
[ "$code" -eq 0 ] || finish
if jq -e --arg image "$A" '.app == "web" and .image == $image and .tag == "v1"' <<<"$out" >/dev/null; then
  pass 'dry-run selects applied A after tag moves to B'
else fail 'dry-run selects applied A after tag moves to B' 'plan is invalid or does not pin A'; fi
guard=$(guard_snapshot) || { fail 'read destination audit and approvals' 'invalid response'; finish; }
out=$(cmd "$DEST" promote web --from "$ENVN" --json </dev/null 2>&1); code=$?
[ "$code" -ne 0 ] && pass 'JSON without dry-run refuses mutation' || fail 'JSON without dry-run refuses mutation' 'command succeeded'
expect_eq 'noninteractive JSON makes no admission' 0 "$(history | jq 'length')"
unchanged 'noninteractive JSON leaves audit and approvals unchanged' "$guard"

# A loopback proxy forwards destination reads but returns a deliberately malformed HTTP 200
# plan for a dry run. Compare the real destination audit and approvals after the refusal.
guard=$(guard_snapshot) || { fail 'read destination audit and approvals' 'invalid response'; finish; }
coproc PREVIEW_PROXY { exec python3 - "$AGENT2_LISTEN" "$RUN" 2>"$RUN/preview-proxy.log" <<'PYPROXY'
import http.server, json, sys, urllib.error, urllib.request
origin, run_dir = sys.argv[1:]
class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args): pass
    def do_GET(self):
        req = urllib.request.Request('http://' + origin + self.path, headers={'Authorization': self.headers.get('Authorization', '')})
        try:
            with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(req, timeout=30) as r:
                body, status = r.read(), r.status
        except urllib.error.HTTPError as e:
            body, status = e.read(), e.code
        self.send_response(status); self.end_headers(); self.wfile.write(body)
    def do_POST(self):
        try: body = json.loads(self.rfile.read(int(self.headers.get('Content-Length', '0'))))
        except (ValueError, TypeError): body = {}
        if self.path.endswith('/deploys') and body.get('dry_run') is True:
            open(run_dir+'/invalid-preview-seen', 'w').close()
            response, status = b'{"app":"web","image":"invalid","roles":["main"]}', 200
        else:
            open(run_dir+'/invalid-preview-mutation', 'w').close()
            response, status = b'{}', 400
        self.send_response(status); self.end_headers(); self.wfile.write(response)
server = http.server.HTTPServer(('127.0.0.1', 0), Handler)
print(server.server_port, flush=True)
server.serve_forever()
PYPROXY
}
preview_proxy_pid=$PREVIEW_PROXY_PID
IFS= read -r preview_port <&"${PREVIEW_PROXY[0]}" || { fail 'start invalid-preview proxy' 'no port'; finish; }
jq --arg project "$PROJECT" --arg env "$DEST" --arg url "http://127.0.0.1:$preview_port" '.projects[$project].environments[$env].url = $url' "$RUN/admin.json" > "$RUN/invalid-preview.json" || { fail 'write proxy config' 'jq failed'; finish; }

# The destination is never placed in the name-only ledger. The driver changes state only
# after reading the real prompt; its private log is redacted before it is saved.
terminal_promote() {
  local name=$1 answer=$2 change=$3 expected=$4 config=${5:-$RUN/admin.json}
  python3 - "$BIN" "$ENVN" "$DEST" "$PROJECT" "$config" "$MAN" "$answer" "$change" "$expected" "$WEB_V1_DIGEST" "$LOG/promote-$name.log" "$RUN_ID-$name" "$RUN_ID" "$COLLISION_FILE" "$DOCKER_BIN" "$SOCK" "$E2E_ROOT/docker-config" <<'PY'
import errno, os, pty, re, select, subprocess, sys, time
binary, source, dest, project, config, manifest, answer, change, expected, digest, log, request_id, marker, collision_file, docker, socket, docker_config = sys.argv[1:]
master, slave = pty.openpty()
cmd = [binary, 'promote', 'web', '-e', dest, '--project', project, '--config', config, '--manifest-dir', manifest, '--from', source, '--request-id', request_id]
proc = subprocess.Popen(cmd, stdin=slave, stdout=slave, stderr=slave, close_fds=True)
os.close(slave)
data = bytearray()
prompted = False
try:
    deadline = time.monotonic() + 300
    while time.monotonic() < deadline:
        ready, _, _ = select.select([master], [], [], 1)
        if ready:
            try: chunk = os.read(master, 65536)
            except OSError as e:
                if e.errno == errno.EIO: break
                raise
            if not chunk: break
            data.extend(chunk)
            if not prompted and b'Proceed with this deploy? [y/N] ' in data:
                prompted = True
                if change in ('source', 'destination'):
                    env, version = (source, 'v2') if change == 'source' else (dest, digest)
                    other = subprocess.run([binary, 'deploy', 'web', '-e', env, '--project', project, '--config', config,
                                            '--manifest-dir', manifest, '--version', version] + (['--request-id', marker+'-concurrent'] if change == 'destination' else []), stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=240)
                    data.extend(b'\n[concurrent deploy]\n' + other.stdout)
                    if other.returncode: raise RuntimeError('concurrent fixture deploy failed')
                if change == 'collision':
                    env = dict(os.environ, DOCKER_CONFIG=docker_config)
                    created = subprocess.run([docker, '--host', 'unix://'+socket, 'service', 'create', '--detach', '--name', project+'-'+dest+'-web-main', '--replicas', '0', '--no-resolve-image', '--label', 'fixture.promotion.collision='+marker, '--label', 'berthy.project='+project, '--label', 'berthy.environment='+dest, '--label', 'berthy.app=web', '--label', 'berthy.role=main', '--label', 'berthy.operation=foreign-'+marker, 'busybox:latest'], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=60)
                    if created.returncode: raise RuntimeError('collision fixture creation failed')
                    service_id = created.stdout.strip()
                    if not re.fullmatch(rb'[a-z0-9]{25}', service_id): raise RuntimeError('invalid collision service ID')
                    with open(collision_file, 'wb') as f: f.write(service_id+b'\n')
                if change == 'source':
                    with open(os.path.join(manifest, 'berthy.' + dest + '.yml'), 'w') as f:
                        f.write('apps:\n  web:\n    env:\n      plain:\n        SNAPSHOT: after\n')
                os.write(master, (answer + '\n').encode())
        if proc.poll() is not None and not ready: break
    else: raise RuntimeError('promotion prompt timed out')
    code = proc.wait(timeout=10)
    if change == 'invalid':
        if prompted or code == 0 or b'invalid agent deploy plan' not in data: raise RuntimeError('invalid plan was not refused before the prompt')
    elif not prompted or code != int(expected): raise RuntimeError('promotion did not prompt or had unexpected exit')
finally:
    if proc.poll() is None: proc.kill(); proc.wait()
    os.close(master)
    # Only fixture token variables exist in this child; never archive their values.
    for key, value in os.environ.items():
        if key.startswith('BERTHY_') and 'TOKEN' in key and value:
            data = data.replace(value.encode(), b'[redacted]')
    with open(log, 'wb') as f: f.write(data)
PY
}

if terminal_promote invalid y invalid 1 "$RUN/invalid-preview.json"; then pass 'interactive malformed plan refuses without consent'; else fail 'interactive malformed plan refuses without consent' 'terminal driver failed'; fi
[ -f "$RUN/invalid-preview-seen" ] && pass 'malformed dry-run plan reached the CLI' || fail 'malformed dry-run plan reached the CLI' 'preview was not requested'
[ ! -e "$RUN/invalid-preview-mutation" ] && pass 'malformed plan sends no mutation' || fail 'malformed plan sends no mutation' 'proxy received a mutation'
unchanged 'malformed preview leaves audit and approvals unchanged' "$guard"
kill "$preview_proxy_pid" 2>/dev/null || :
wait "$preview_proxy_pid" 2>/dev/null || :
preview_proxy_pid=

# An unconfirmed preview must not grant ownership of a name claimed during its prompt.
guard=$(guard_snapshot) || { fail 'read destination audit and approvals' 'invalid response'; finish; }
if terminal_promote collision n collision 1; then pass 'decline after destination name collision'; else fail 'decline after destination name collision' 'terminal driver failed'; finish; fi
collision=$(cat "$COLLISION_FILE")
cleanup_owned_services || { fail 'check collision cleanup' 'ownership check failed'; finish; }
if [ "$(d service inspect --format '{{.ID}}' "$collision" 2>/dev/null)" = "$collision" ]; then pass 'foreign collision survives decline and fixture cleanup'; else fail 'foreign collision survives decline and fixture cleanup' 'service disappeared'; fi
unchanged 'collision decline leaves audit and approvals unchanged' "$guard"
labels=$(d service inspect --format '{{json .Spec.Labels}}' "$collision") || { fail 'read collision label' 'service unavailable'; finish; }
if jq -e --arg marker "$RUN_ID" '.["fixture.promotion.collision"] == $marker' <<<"$labels" >/dev/null; then
  d service rm "$collision" >/dev/null || { fail 'remove intentional collision by ID' 'removal failed'; finish; }
  rm -f "$COLLISION_FILE"
else fail 'remove intentional collision by ID' 'ownership marker missing'; finish; fi

# Change the source and overlay at the prompt. The submitted request must retain A, v1 and the
# original destination overlay, even though the source now applies B.
if terminal_promote first y source 0; then pass 'confirmed source snapshot'; else fail 'confirmed source snapshot' 'terminal driver failed'; finish; fi
expect_contains 'preview names source environment' "$(cat "$LOG/promote-first.log")" "$ENVN"
expect_contains 'preview names the pinned digest' "$(cat "$LOG/promote-first.log")" "$A"
row=$(applied) || { fail 'read destination applied release' 'request failed'; finish; }
if jq -e --arg image "$A" '.id > 0 and .digest == $image and .tag == "v1"' <<<"$row" >/dev/null; then pass 'destination applied A and v1'; else fail 'destination applied A and v1' 'wrong applied release'; fi
out=$(history)
if jq -e --arg image "$A" 'any(.[]; .digest == $image and .tag == "v1")' <<<"$out" >/dev/null; then pass 'history records A and v1'; else fail 'history records A and v1' 'release missing'; fi
spec=$(service_spec "$PROJECT-$DEST-web-main")
expect_contains 'Swarm pins A' "$spec" "$A"
expect_contains 'Swarm keeps v1 label' "$spec" 'BERTHY_VERSION=v1'
expect_contains 'Swarm keeps the previewed overlay' "$spec" 'SNAPSHOT=before'

# Restore source A by digest-independent v1 only after returning the tag to A: rollback preserves
# the original release and label, even though the tag itself now resolves to B.
out=$(cmd "$ENVN" rollback web --to 1 2>&1); code=$?
expect_code 'restore source A' 0 "$code" "$out"
before=$(history | jq 'length')
guard=$(guard_snapshot) || { fail 'read destination audit and approvals' 'invalid response'; finish; }
out=$(cmd "$DEST" promote web --from "$ENVN" --dry-run 2>&1); code=$?
[ "$code" -ne 0 ] && pass 'equal digest refuses promotion' || fail 'equal digest refuses promotion' 'command succeeded'
expect_eq 'equal digest makes no destination admission' "$before" "$(history | jq 'length')"
unchanged 'invalid preview leaves audit and approvals unchanged' "$guard"

out=$(cmd "$ENVN" deploy web --version v2 2>&1); code=$?
expect_code 'advance source to B' 0 "$code" "$out"
for answer in n ''; do
  label=${answer:-default}
  guard=$(guard_snapshot) || { fail 'read destination audit and approvals' 'invalid response'; finish; }
  if terminal_promote "$label" "$answer" none 1; then pass "$label declines promotion"; else fail "$label declines promotion" 'terminal driver failed'; fi
  unchanged "$label decline leaves audit and approvals unchanged" "$guard"
done
expect_eq 'declined promotions make no admission' "$before" "$(history | jq 'length')"
if terminal_promote stale y destination 1; then pass 'stale destination refuses confirmed promotion'; else fail 'stale destination refuses confirmed promotion' 'terminal driver failed'; fi
row=$(applied)
if jq -e --arg image "$A" '.digest == $image' <<<"$row" >/dev/null; then pass 'stale promotion leaves concurrent destination release in place'; else fail 'stale promotion leaves concurrent destination release in place' 'wrong applied digest'; fi
expect_eq 'stale promotion adds no release beyond concurrent deploy' "$((before + 1))" "$(history | jq 'length')"

# Make the destination protected, then use a destination agent token while the source entry
# continues to use the source admin credential. The ordinary approval path applies the hold.
printf 'protected: true\napps:\n  web:\n    env:\n      plain:\n        SNAPSHOT: after\n' > "$MAN/berthy.$DEST.yml"
out=$(cmd "$DEST" deploy web --version "$WEB_V1_DIGEST" 2>&1); code=$?
expect_code 'protect destination with A' 0 "$code" "$out"
[ "$code" -eq 0 ] || finish
created=$(cmd "$DEST" token create promote-agent --scope deploy --agent --json 2>/dev/null) || { fail 'create fixture agent token' 'token creation failed'; finish; }
BERTHY_PROMOTION_AGENT_TOKEN=$(jq -er '.token' <<<"$created") || { fail 'create fixture agent token' 'invalid response'; finish; }
export BERTHY_PROMOTION_AGENT_TOKEN
unset created
jq --arg project "$PROJECT" --arg env "$DEST" '.projects[$project].environments[$env].token_env = "BERTHY_PROMOTION_AGENT_TOKEN"' "$RUN/admin.json" > "$RUN/promote-agent.json" || { fail 'write fixture agent config' 'jq failed'; finish; }
if terminal_promote held y none 3 "$RUN/promote-agent.json"; then pass 'destination agent token holds promotion'; else fail 'destination agent token holds promotion' 'terminal driver failed'; finish; fi
id=$(sed -n -e "s/$(printf '\033')\\[[0-9;]*m//g" -e 's/.*held for approval: \([^ ]*\) .*/\1/p' "$LOG/promote-held.log" | tr -d '\r' | head -n 1)
[ -n "$id" ] || { fail 'held promotion names approval' 'approval ID missing'; finish; }
out=$(cmd "$DEST" approve "$id" 2>&1); code=$?
expect_code 'approve held promotion' 0 "$code" "$out"
row=$(applied)
if jq -e --arg image "$B" '.digest == $image and .tag == "v2"' <<<"$row" >/dev/null; then pass 'approved hold applies B and v2'; else fail 'approved hold applies B and v2' 'wrong applied release'; fi
finish
