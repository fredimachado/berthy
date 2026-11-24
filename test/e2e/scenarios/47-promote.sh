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
trap 'stop_agent2' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
start_agent2 || { fail 'start second agent' 'fixture failed'; finish; }

# Every command uses the one fixture config; each environment resolves its own token.
cmd() {
  local env=$1 verb=$2
  shift 2
  local -a manifest_arg=()
  case $verb in deploy|promote) manifest_arg=(--manifest-dir "$MAN");; esac
  (cd "$MAN" && { [ "$verb" != deploy ] || record_deploy "$PROJECT" "$env" "$@"; } &&
    "$BIN" "$verb" -e "$env" --project "$PROJECT" --config "$RUN/admin.json" "${manifest_arg[@]}" "$@")
}
history() { cmd "$DEST" history web --json; }
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
out=$(cmd "$DEST" promote web --from "$ENVN" --json </dev/null 2>&1); code=$?
[ "$code" -ne 0 ] && pass 'JSON without dry-run refuses mutation' || fail 'JSON without dry-run refuses mutation' 'command succeeded'
expect_eq 'noninteractive JSON makes no admission' 0 "$(history | jq 'length')"

# Journal both services before a PTY child can create either. The driver changes state only
# after reading the real prompt; its private log is redacted before it is saved.
(cd "$MAN" && record_deploy "$PROJECT" "$ENVN" web --manifest-dir "$MAN" && record_deploy "$PROJECT" "$DEST" web --manifest-dir "$MAN") || { fail 'record fixture workloads' 'ownership tracking failed'; finish; }
terminal_promote() {
  local name=$1 answer=$2 change=$3 expected=$4 config=${5:-$RUN/admin.json}
  python3 - "$BIN" "$ENVN" "$DEST" "$PROJECT" "$config" "$MAN" "$answer" "$change" "$expected" "$WEB_V1_DIGEST" "$LOG/promote-$name.log" <<'PY'
import errno, os, pty, select, subprocess, sys, time
binary, source, dest, project, config, manifest, answer, change, expected, digest, log = sys.argv[1:]
master, slave = pty.openpty()
cmd = [binary, 'promote', 'web', '-e', dest, '--project', project, '--config', config, '--manifest-dir', manifest, '--from', source]
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
                                            '--manifest-dir', manifest, '--version', version], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=240)
                    data.extend(b'\n[concurrent deploy]\n' + other.stdout)
                    if other.returncode: raise RuntimeError('concurrent fixture deploy failed')
                if change == 'source':
                    with open(os.path.join(manifest, 'berthy.' + dest + '.yml'), 'w') as f:
                        f.write('apps:\n  web:\n    env:\n      plain:\n        SNAPSHOT: after\n')
                os.write(master, (answer + '\n').encode())
        if proc.poll() is not None and not ready: break
    else: raise RuntimeError('promotion prompt timed out')
    code = proc.wait(timeout=10)
    if not prompted or code != int(expected): raise RuntimeError('promotion did not prompt or had unexpected exit')
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
out=$(cmd "$DEST" promote web --from "$ENVN" --dry-run 2>&1); code=$?
[ "$code" -ne 0 ] && pass 'equal digest refuses promotion' || fail 'equal digest refuses promotion' 'command succeeded'
expect_eq 'equal digest makes no destination admission' "$before" "$(history | jq 'length')"

out=$(cmd "$ENVN" deploy web --version v2 2>&1); code=$?
expect_code 'advance source to B' 0 "$code" "$out"
for answer in n ''; do
  label=${answer:-default}
  if terminal_promote "$label" "$answer" none 1; then pass "$label declines promotion"; else fail "$label declines promotion" 'terminal driver failed'; fi
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
id=$(sed -n 's/.*held for approval: \([^ ]*\) .*/\1/p' "$LOG/promote-held.log" | tr -d '\r' | head -n 1)
[ -n "$id" ] || { fail 'held promotion names approval' 'approval ID missing'; finish; }
out=$(cmd "$DEST" approve "$id" 2>&1); code=$?
expect_code 'approve held promotion' 0 "$code" "$out"
row=$(applied)
if jq -e --arg image "$B" '.digest == $image and .tag == "v2"' <<<"$row" >/dev/null; then pass 'approved hold applies B and v2'; else fail 'approved hold applies B and v2' 'wrong applied release'; fi
finish
