#!/bin/bash
# Protected tag selection on a private registry, including mixed batches.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$(dirname "$0")/../deploy-ux-fixture.sh" || exit 1
trap 'stop_private_registry trap' EXIT
start_private_registry || { fail 'start private registry' 'fixture failed'; exit 1; }

cat > "$MAN/berthy.yml" <<YML
project: $PROJECT
network: $NETWORK
protected: true
registries:
  "$PRIVATE_REG":
    username: $PRIVATE_USER
    password_secret: REG_PASS
apps:
  web:
    kind: image
    image: $PRIVATE_REG/$PRIVATE_IMAGE_REPO
    roles:
      main:
        health:
          test: ["CMD", "true"]
          interval: 2s
          timeout: 2s
          start_period: 1s
          retries: 3
YML
printf 'apps:\n  web:\n    env:\n      plain:\n        SNAPSHOT: before\n' > "$MAN/berthy.$ENVN.yml"
git -C "$MAN" init -q
git -C "$MAN" config user.name e2e
git -C "$MAN" config user.email e2e@example.com
git -C "$MAN" add -A
git -C "$MAN" commit -qm manifest
head=$(git -C "$MAN" rev-parse HEAD)

out=$(printf '%s' "$PRIVATE_PASSWORD" | adm secret set REG_PASS --registry "$PRIVATE_REG" --manifest-dir "$MAN" 2>&1); code=$?
expect_code 'store private registry credential' 0 "$code" "$out"
[ "$code" -eq 0 ] || { finish; exit 1; }

# Save both registry manifests, then point stable at A.
for v in v1 v2; do
  private_curl -f -K "$PRIVATE_CURL_CONFIG" -D "$PRIVATE_DIR/$v.headers" \
    -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json' \
    "$PRIVATE_URL/v2/$PRIVATE_IMAGE_REPO/manifests/$v" -o "$PRIVATE_DIR/$v.manifest" || { fail 'read fixture image' "$v unavailable"; finish; exit 1; }
done
media=$(sed -n 's/^[Cc]ontent-[Tt]ype: *\([^;\r]*\).*/\1/p' "$PRIVATE_DIR/v1.headers" | tr -d '\r' | tail -n 1)
private_curl -f -K "$PRIVATE_CURL_CONFIG" -X PUT -H "Content-Type: $media" --data-binary "@$PRIVATE_DIR/v1.manifest" "$PRIVATE_URL/v2/$PRIVATE_IMAGE_REPO/manifests/stable" -o /dev/null || { fail 'seed stable tag' 'registry refused'; finish; exit 1; }

out=$(adm versions web --manifest-dir "$MAN" --json 2>&1); code=$?
expect_code 'list private tags before first deployment' 0 "$code" "$out"
if jq -e '.tags | index("stable") != null' <<< "$out" >/dev/null; then pass 'private listing includes stable'; else fail 'private listing includes stable' 'tag absent'; fi
out=$(adm deploy web --version stable --manifest-dir "$MAN" --dry-run --json 2>&1); code=$?
expect_code 'preview protected tag' 0 "$code" "$out"
if jq -e --arg image "$PRIVATE_REG/$PRIVATE_IMAGE_REPO@$PRIVATE_A" '.image == $image and .tag == "stable"' <<< "$out" >/dev/null; then pass 'preview pins A'; else fail 'preview pins A' 'unexpected plan'; fi
printf '# dirty\n' >> "$MAN/berthy.$ENVN.yml"
out=$(adm deploy web --version stable --manifest-dir "$MAN" --dry-run 2>&1); code=$?
expect_code 'dirty protected checkout refuses tag' 1 "$code" "$out"
expect_contains 'dirty refusal names checkout' "$out" 'commit them first'
git -C "$MAN" checkout -- "berthy.$ENVN.yml"

# The Python driver waits for the real prompt before interrupting or changing the registry tag.
record_deploy "$PROJECT" "$ENVN" web --manifest-dir "$MAN" || { fail 'record deploy ownership' 'fixture names unavailable'; finish; exit 1; }
terminal_deploy() {
  python3 - "$BIN" "$ENVN" "$RUN/admin.json" "$MAN" "$1" "$PRIVATE_CURL_CONFIG" "$media" "$PRIVATE_DIR/v2.manifest" "$PRIVATE_URL/v2/$PRIVATE_IMAGE_REPO/manifests/stable" "$LOG/tag-$1.log" "$PROJECT" "$RUN/state/state.db" <<'PY'
import errno, os, pty, select, signal, sqlite3, subprocess, sys, termios, time, uuid
binary, env, config, manifest, answer, curl_config, media, image, url, log, project, state = sys.argv[1:]
master, slave = pty.openpty()
before = termios.tcgetattr(slave) if answer == 'term' else None
args = [binary, 'deploy', 'web', '-e', env, '--config', config, '--manifest-dir', manifest, '--version', 'stable']
if answer == 'term':
    request_id = f'{project}-{env}-confirm-{uuid.uuid4().hex}'
    args.extend(['--request-id', request_id])
proc = subprocess.Popen(args, stdin=slave, stdout=slave, stderr=slave, close_fds=True)
if before is None:
    os.close(slave)
data = bytearray()
deadline = time.monotonic() + 120
prompted = False
try:
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
                if answer == 'yes':
                    subprocess.run(['curl', '-q', '--noproxy', '*', '-fsS', '-K', curl_config, '-X', 'PUT', '-H', 'Content-Type: '+media, '--data-binary', '@'+image, url, '-o', '/dev/null'], check=True)
                    with open(os.path.join(manifest, 'berthy.'+env+'.yml'), 'w') as f:
                        f.write('apps:\n  web:\n    env:\n      plain:\n        SNAPSHOT: after\n')
                if answer == 'term':
                    if termios.tcgetattr(slave) == before:
                        raise RuntimeError('confirmation prompt did not change terminal mode')
                    proc.send_signal(signal.SIGTERM)
                else:
                    os.write(master, (('y' if answer == 'yes' else 'n')+'\n').encode())
        if proc.poll() is not None and not ready: break
    else: raise RuntimeError('terminal deploy timed out')
    code = proc.wait(timeout=10)
    with open(log, 'wb') as f: f.write(data)
    if not prompted or code != (0 if answer == 'yes' else 1):
        raise RuntimeError('terminal deploy did not finish as expected')
    if answer == 'term':
        if termios.tcgetattr(slave) != before:
            raise RuntimeError('SIGTERM did not restore the full confirmation terminal mode')
        with sqlite3.connect('file:' + state + '?mode=ro', uri=True) as db:
            admitted = db.execute('SELECT id FROM operations WHERE environment=? AND request_id=?', (env, request_id)).fetchone()
        if admitted is not None:
            raise RuntimeError('SIGTERM at confirmation admitted a deploy')
finally:
    if proc.poll() is None: proc.kill(); proc.wait()
    if before is not None:
        termios.tcsetattr(slave, termios.TCSANOW, before)
        os.close(slave)
    os.close(master)
PY
}
if terminal_deploy term; then pass 'SIGTERM at confirmation restores terminal and admits no deploy'; else fail 'SIGTERM at confirmation restores terminal and admits no deploy' 'terminal driver failed'; fi
if terminal_deploy no; then pass 'declining preview does not deploy'; else fail 'declining preview does not deploy' 'terminal driver failed'; fi
out=$(adm history web --json 2>&1)
if jq -e 'length == 0' <<< "$out" >/dev/null; then pass 'decline leaves history empty'; else fail 'decline leaves history empty' 'unexpected history'; fi
if terminal_deploy yes; then pass 'confirm after retag'; else fail 'confirm after retag' 'terminal driver failed'; fi
spec=$(service_spec "$PROJECT-$ENVN-web-main")
expect_contains 'Swarm task spec retains A' "$spec" "$PRIVATE_REG/$PRIVATE_IMAGE_REPO@$PRIVATE_A"
expect_contains 'Swarm environment retains tag' "$spec" 'BERTHY_VERSION=stable'
expect_contains 'Swarm environment retains old overlay' "$spec" 'SNAPSHOT=before'
out=$(adm history web --json 2>&1)
expect_contains 'history retains A' "$out" "$PRIVATE_A"
expect_contains 'history retains checkout commit' "$out" "$head"
expect_contains 'history retains selected tag' "$out" 'stable'
# Restore a clean checkout for an explicit digest deploy under the existing automation route.
git -C "$MAN" checkout -- "berthy.$ENVN.yml"
out=$(adm deploy web --version "$PRIVATE_B" --manifest-dir "$MAN" 2>&1); code=$?
expect_code 'explicit digest deployment needs no prompt' 0 "$code" "$out"
spec=$(service_spec "$PROJECT-$ENVN-web-main")
expect_contains 'digest automation deploys B' "$spec" "$PRIVATE_REG/$PRIVATE_IMAGE_REPO@$PRIVATE_B"

# A mixed batch previews both apps before one confirmation. Use a separate Compose
# project and record its ownership before creating it.
. "$RUN/digests.env"
cat >> "$MAN/berthy.yml" <<YML
  aux:
    kind: image
    image: $REG/e2e/web
    depends_on: [web]
    roles:
      main:
        health:
          test: ["CMD", "true"]
          interval: 2s
          timeout: 2s
          start_period: 1s
          retries: 3
  zstack:
    kind: compose
    file: batch-compose.yml
    stateful: true
    depends_on: [aux]
YML
cat > "$MAN/batch-compose.yml" <<YML
services:
  side:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sleep", "3600"]
    environment:
      BATCH_MODE: one
YML
git -C "$MAN" add berthy.yml batch-compose.yml
git -C "$MAN" commit -qm batch
record_deploy "$PROJECT" "$ENVN" zstack --manifest-dir "$MAN" || { fail 'record batch ownership' 'fixture names unavailable'; finish; exit 1; }
record_deploy "$PROJECT" "$ENVN" aux --manifest-dir "$MAN" || { fail 'record batch image ownership' 'fixture names unavailable'; finish; exit 1; }
out=$(adm deploy --version web=stable --version "aux=$WEB_V1_DIGEST" --manifest-dir "$MAN" --dry-run --json 2>&1); code=$?
expect_code 'preview image and Compose batch' 0 "$code" "$out"
if jq -e '.apps | length == 3 and .[0].app == "web" and .[1].app == "aux" and .[2].app == "zstack"' <<< "$out" >/dev/null; then pass 'batch preview preserves order'; else fail 'batch preview preserves order' 'unexpected plans'; fi
terminal_batch() {
python3 - "$BIN" "$ENVN" "$RUN/admin.json" "$MAN" "$1" "$WEB_V1_DIGEST" <<'PY'
import errno, os, pty, select, subprocess, sys, time
binary, env, config, manifest, mode, digest = sys.argv[1:]
master, slave = pty.openpty()
proc = subprocess.Popen([binary, 'deploy', '-e', env, '--config', config, '--manifest-dir', manifest, '--version', 'web=stable', '--version', 'aux='+digest], stdin=slave, stdout=slave, stderr=slave, close_fds=True)
os.close(slave)
data = bytearray()
prompted = False
try:
    deadline = time.monotonic() + 180
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
                os.write(master, b'y\n' if mode == 'yes' else b'n\n')
        if proc.poll() is not None and not ready: break
    else: raise RuntimeError('batch timed out')
    code = proc.wait(timeout=10)
    if mode == 'yes' and (not prompted or code != 0):
        raise RuntimeError('batch failed: prompted=%s code=%s stopped_at_aux=%s missing_health=%s' % (prompted, code, b'deploy stopped at aux' in data, b'healthcheck required' in data))
    if mode == 'blocked' and (prompted or code == 0 or b'blocker' not in data): raise RuntimeError('blocked batch was not refused before confirmation')
finally:
    if proc.poll() is None: proc.kill(); proc.wait()
    os.close(master)
PY
}
if terminal_batch yes; then pass 'confirmed image then Compose batch'; else fail 'confirmed image then Compose batch' 'terminal batch failed'; fi

# Change only the kept release's tag while the batch is waiting for consent.
if python3 - "$BIN" "$ENVN" "$PROJECT" "$RUN/admin.json" "$MAN" "$AGENT_LISTEN" <<'PY'
import errno, json, os, pty, select, subprocess, sys, time, urllib.error, urllib.request, uuid
binary, env, project, config, directory, listen = sys.argv[1:]
prefix = '/v1/projects/' + project + '/environments/' + env + '/'
base = 'http://' + listen
token = os.environ['BERTHY_ADMIN_TOKEN']
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
def request(method, path, body=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(base + path, data=data, method=method, headers={'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json'})
    with opener.open(req, timeout=120) as response:
        return json.load(response)
def applied():
    return request('GET', prefix + 'applied/aux')
def history():
    result = subprocess.run([binary, 'history', '-e', env, '--config', config, 'aux', '--json'], capture_output=True, check=True)
    rows = json.loads(result.stdout)
    if not isinstance(rows, list): raise RuntimeError('invalid history')
    return rows
manifest = json.loads(subprocess.run([binary, 'config', 'show', '-e', env, '--manifest-dir', directory, '--json'], capture_output=True, check=True).stdout)
def retag(tag):
    current = applied()
    body = {'app':'aux', 'request_id':uuid.uuid4().hex, 'version':current['digest'], 'version_tag':tag, 'expected_applied':current['id'], 'manifest':manifest}
    accepted = request('POST', prefix + 'deploys', body)
    opid = accepted['operation_id']
    event_path = prefix + 'operations/' + opid + '/events'
    if accepted['events_url'] != event_path: raise RuntimeError('invalid event path')
    req = urllib.request.Request(base + event_path, headers={'Authorization':'Bearer ' + token})
    with opener.open(req, timeout=120) as stream:
        for line in stream:
            if line.strip() == b'event: terminal': break
        else: raise RuntimeError('operation event stream ended early')
    if request('GET', prefix + 'operations/' + opid).get('state') != 'succeeded': raise RuntimeError('tag-only deploy failed')
    return applied()
snapshot = retag('batch-a')
if snapshot.get('tag') != 'batch-a': raise RuntimeError('initial kept label missing')
master, slave = pty.openpty()
proc = subprocess.Popen([binary, 'deploy', '-e', env, '--config', config, '--manifest-dir', directory, '--version', 'web=stable'], stdin=slave, stdout=slave, stderr=slave, close_fds=True)
os.close(slave)
data = bytearray()
try:
    deadline = time.monotonic() + 180
    while time.monotonic() < deadline:
        ready, _, _ = select.select([master], [], [], 1)
        if ready:
            try: chunk = os.read(master, 65536)
            except OSError as e:
                if e.errno == errno.EIO: break
                raise
            if not chunk: break
            data.extend(chunk)
            if b'Proceed with this deploy? [y/N] ' in data: break
        if proc.poll() is not None: raise RuntimeError('batch exited before confirmation')
    else: raise RuntimeError('batch confirmation timed out')
    if b'batch-a' not in data or snapshot['digest'].encode() not in data: raise RuntimeError('kept preview missing snapshot')
    before = len(history())
    newer = retag('batch-b')
    if newer['id'] == snapshot['id'] or newer['digest'] != snapshot['digest'] or newer.get('tag') != 'batch-b': raise RuntimeError('tag-only release did not advance')
    if len(history()) != before + 1: raise RuntimeError('tag-only release missing from history')
    os.write(master, b'y\n')
    while True:
        try: chunk = os.read(master, 65536)
        except OSError as e:
            if e.errno == errno.EIO: break
            raise
        if not chunk: break
        data.extend(chunk)
    if proc.wait(timeout=120) == 0 or b'HTTP 409' not in data or b'deploy stopped at aux' not in data: raise RuntimeError('stale kept step was not refused')
    final = applied()
    if final['id'] != newer['id'] or final['digest'] != newer['digest'] or final.get('tag') != 'batch-b' or len(history()) != before + 1: raise RuntimeError('stale batch changed kept release')
finally:
    if proc.poll() is None: proc.kill(); proc.wait()
    os.close(master)
PY
then pass 'kept batch refuses a changed release'; else fail 'kept batch refuses a changed release' 'snapshot check failed'; fi
before=''
if before_json=$(adm history web --json 2>/dev/null) && before=$(jq -esr 'if length == 1 and (.[0] | type) == "array" then .[0] | length else error("invalid history") end' <<< "$before_json" 2>/dev/null); then
  pass 'read history before blocked batch'
else
  fail 'read history before blocked batch' 'history unavailable or invalid'
  before=''
fi
sed -i 's/BATCH_MODE: one/BATCH_MODE: two/' "$MAN/batch-compose.yml"
git -C "$MAN" add batch-compose.yml
git -C "$MAN" commit -qm 'change Compose service'
out=$(adm deploy --version web=stable --version "aux=$WEB_V1_DIGEST" --manifest-dir "$MAN" --dry-run --json 2>&1); code=$?
expect_code 'stateful Compose blocker refuses mixed batch' 1 "$code" "$out"
expect_contains 'batch reports nested blocker' "$out" 'blocker'
if terminal_batch blocked; then pass 'blocked batch refuses before confirmation'; else fail 'blocked batch refuses before confirmation' 'terminal batch failed'; fi
after=''
if after_json=$(adm history web --json 2>/dev/null) && after=$(jq -esr 'if length == 1 and (.[0] | type) == "array" then .[0] | length else error("invalid history") end' <<< "$after_json" 2>/dev/null); then
  pass 'read history after blocked batch'
else
  fail 'read history after blocked batch' 'history unavailable or invalid'
  after=''
fi
if [ -n "$before" ] && [ -n "$after" ] && [ "$before" = "$after" ]; then pass 'blocked batch leaves image history unchanged'; else fail 'blocked batch leaves image history unchanged' 'history unavailable, invalid or changed'; fi
finish
