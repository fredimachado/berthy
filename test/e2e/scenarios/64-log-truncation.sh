#!/bin/bash
# A long application line keeps its safe prefix without losing later logs or deploy diagnostics.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

write_manifest <<YML
project: $PROJECT
network: $NETWORK
apps:
  web:
    kind: image
    image: $REG/e2e/web
    env:
      secret:
        - SECRET_VAR: JOBSECRET
    roles:
      main:
        health:
          test: ["CMD", "true"]
          interval: 2s
          timeout: 2s
          start_period: 1s
          retries: 3
YML
printf 'LEFTdistinctSECRET-RIGHTdistinctSECRET' | adm secret set JOBSECRET >"$RUN/secret-result" 2>&1
expect_code "set mapped secret" 0 "$?" "$(cat "$RUN/secret-result")"
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy web" 0 "$code" "$out"
container=$(d ps -q --filter status=running --filter "label=com.docker.swarm.service.name=$PROJECT-$ENVN-web-main" | head -n 1)
if [ -z "$container" ]; then fail "find web container" "no running task"; finish; fi
python3 - "$RUN" <<'PY'
import pathlib, sys
run = pathlib.Path(sys.argv[1])
secret = b'LEFTdistinctSECRET-RIGHTdistinctSECRET'
line = b'x' * (16384 - len(b'LEFTdistinctSECRET')) + secret
line += b'x' * (300 * 1024 - len(line))
(run / 'long-line').write_bytes(line)
(run / 'log-input').write_bytes(b'long-before-64\n' + line + b'\nlong-after-64\n')
PY
d exec -i "$container" sh -c 'cat > /proc/1/fd/1' < "$RUN/log-input"
expect_code "write fixture logs" 0 "$?"
seen_after() { d logs "$container" 2>/dev/null | grep -qF 'long-after-64'; }
wait_for "Docker records the final sentinel" seen_after
expect_code "Docker records the final sentinel" 0 "$?"
adm logs web --tail 1000 --json >"$RUN/logs.json" 2>"$RUN/logs.err"; code=$?
expect_code "logs returns the long line and later evidence" 0 "$code" "$(cat "$RUN/logs.err")"
if [ "$code" -eq 0 ]; then
  if python3 - "$RUN" <<'PY'
import json, pathlib, sys
run = pathlib.Path(sys.argv[1])
rows = json.loads((run / 'logs.json').read_text())
messages = [r['message'] for r in rows]
before, after = messages.index('long-before-64'), messages.index('long-after-64')
assert after == before + 2, f'expected one record between sentinels, got {after-before-1}'
secret = b'LEFTdistinctSECRET-RIGHTdistinctSECRET'
expected = (run / 'long-line').read_bytes()[:262144-31].replace(secret, b'[REDACTED]').decode() + ' …[truncated]'
actual = messages[before+1]
if actual != expected:
    offset = next((i for i, (a, b) in enumerate(zip(actual, expected)) if a != b), min(len(actual), len(expected)))
    raise AssertionError(f'message differs at character {offset}; lengths {len(actual)}, {len(expected)}')
assert '[REDACTED]' in actual and 'LEFTdistinctSECRET' not in actual and 'RIGHTdistinctSECRET' not in actual
PY
  then pass "the complete retained prefix is redacted and marked"; else fail "the complete retained prefix is redacted and marked" "message mismatch"; fi
fi

mkdir -p "$LOG/tui-long-logs"
cat > "$LOG/tui-long-logs/script.json" <<'JSON'
[
  {"wait_text": "Logs", "timeout": "20s"},
  {"keys": ["2"]},
  {"wait_text": "web", "timeout": "20s"},
  {"keys": ["enter"]},
  {"wait_text": "long-after-64", "timeout": "30s"},
  {"snapshot": "logs"},
  {"keys": ["q"]}
]
JSON
out=$("$TUI_BIN" --command "$BIN ui -e $ENVN --config $RUN/admin.json" --size 120x40 --script "$LOG/tui-long-logs/script.json" --out "$LOG/tui-long-logs/out" --timeout 75s --redact-env BERTHY_ADMIN_TOKEN 2>&1); code=$?
expect_code "TUI follows logs past the long line" 0 "$code" "$out"

cat >> "$MAN/berthy.yml" <<'YML'
    hooks:
      pre_deploy:
        - run: ["sh", "-c", "head -c 307200 /dev/zero | tr '[:cntrl:]' x; echo; echo diagnostic-after-64; exit 1"]
YML
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "failing hook fails deploy" 1 "$code" "$out"
id=$(printf '%s\n' "$out" | grep -oE '\b[0-9a-f]{32}\b' | head -n 1)
if [ -z "$id" ]; then fail "capture failed operation" "$out"; finish; fi
adm diagnostics "$id" --json >"$RUN/diagnostics.json" 2>"$RUN/diagnostics.err"; code=$?
expect_code "read failed deploy diagnostics" 0 "$code" "$(cat "$RUN/diagnostics.err")"
if jq -e '.diagnostics | any(.truncated == true and (.logs | contains("diagnostic-after-64")))' "$RUN/diagnostics.json" >/dev/null; then
  pass "diagnostics keep the later line and report truncation"
else
  fail "diagnostics keep the later line and report truncation" "$(cat "$RUN/diagnostics.json")"
fi
finish
