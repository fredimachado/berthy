#!/bin/bash
# Run guided setup against a disposable SSH server and no credential store.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
export FS_HOME="$RUN/guided-setup-home" FS_DOCKER_HOST="unix://$SOCK" DOCKER_CONFIG="$E2E_ROOT/docker-config"
export HOME="$FS_HOME"
mkdir -p "$FS_HOME/bin"
ln -sf "$DOCKER_BIN" "$FS_HOME/bin/docker"
export PATH="$FS_HOME/bin:/usr/bin:/bin"
fixture="$REPO/test/fakeserver/fakeserver.sh"
private=$(mktemp -d /dev/shm/berthy-guided.XXXXXXXX) || exit 1
trap 'rm -rf "$private"' EXIT
chmod 700 "$private" || exit 1
scope=$(printf '%s' "$PROJECT/$ENVN" | sha256sum | cut -c1-8)
nonce=$(printf '%s' "$private" | sha256sum | cut -c1-16)
server="g${scope}-${nonce}"
[[ $server =~ ^[a-z][a-z0-9-]{0,30}$ ]] || exit 1
existing=$(d ps -aq --no-trunc --filter "name=^/${server}\$") || { echo "could not check fixture containers" >&2; exit 1; }
if [ -n "$existing" ]; then echo "fixture name already in use" >&2; exit 1; fi
for volume in "$server-docker" "$server-containerd"; do
 if d volume inspect "$volume" >/dev/null 2>&1; then echo "fixture volume already in use" >&2; exit 1; fi
done
volumes=$(d volume ls -q) || { echo "could not check fixture volumes" >&2; exit 1; }
for volume in "$server-docker" "$server-containerd"; do
 if printf '%s\n' "$volumes" | grep -Fxq -- "$volume"; then echo "fixture volume already in use" >&2; exit 1; fi
done
cleanup() { bash "$fixture" rm "$server" >/dev/null 2>&1 || echo "could not remove setup fixture" >&2; rm -rf "$private"; }
trap cleanup EXIT
bash "$fixture" start "$server" >/dev/null || exit 1
ip=$(bash "$fixture" ip "$server") || exit 1
cfg="$RUN/guided-setup.json"
cat <<YML | write_manifest
project: $PROJECT
network: $NETWORK
registries:
  "$REG":
    username: fixture
    password_secret: REG_PASS
apps:
  web:
    kind: image
    image: $REG/e2e/web
    roles:
      main: {}
    env:
      secret: [GEN_PASS, API_KEY]
secrets:
  GEN_PASS:
    generate:
      format: hex
YML
printf 'servers:\n  primary:\n    ssh: ops@%s\n    address: %s\n' "$server" "$ip" > "$MAN/berthy.$ENVN.yml" || exit 1
cmd="$BIN setup -e $ENVN --manifest-dir $MAN --config $cfg"
mkdir -p "$private/gate" "$LOG/tui-guided-setup" || exit 1
mkfifo "$private/ssh-gate" || exit 1
cat > "$private/gate/ssh" <<'SH'
#!/bin/sh
read -r unused < "$SSH_GATE"
exec /usr/bin/ssh "$@"
SH
chmod 700 "$private/gate/ssh" || exit 1
# key_absent KEYFILE DIR [ENVNAME...]: succeeds when no form of the key in KEYFILE, and no value of the named
# variables, appears in a file under DIR.
key_absent() {
 python3 - "$@" <<'PYCODE'
import base64, os, pathlib, sys
try:
    key = pathlib.Path(sys.argv[1]).read_bytes()
    if len(key) != 32:
        raise ValueError()
    forms = [key, key.hex().encode()]
    for f in (base64.b64encode, base64.urlsafe_b64encode):
        encoded = f(key)
        forms.extend((encoded, encoded.rstrip(b"=")))
    forms.extend(os.environ[name].encode() for name in sys.argv[3:])
    for path in pathlib.Path(sys.argv[2]).rglob("*"):
        if path.is_file() and any(form in path.read_bytes() for form in forms):
            raise ValueError()
except Exception:
    sys.exit(1)
PYCODE
}
redact() { sed -E 's/bty\.[A-Za-z0-9._-]+/[redacted]/g; s/[0-9A-Fa-f]{32,}/[redacted]/g' "$1"; }
# keep_failure NAME: writes $LOG/tui-guided-setup/NAME-failure with the driver's fixed-form
# error lines and only known-safe fields from the failed frames (step status or spinner, step
# name, elapsed time; secret prompt/names; section headings; that an admin token was shown).
# Free-form detail is dropped, so no key scan is needed. Prints see tui-guided-setup/NAME-failure
# on success, or its output was not kept when writing failed (with nothing partial left).
keep_failure() {
 local name=$1 kept="$private/$1-kept" dest="$LOG/tui-guided-setup/$1-failure" f
 rm -rf "$kept" "$dest"
 mkdir -p "$kept" || { echo "its output was not kept"; return 1; }
 grep -E '^tuidriver: ' "$private/$name.err" > "$kept/driver-error.raw" 2>/dev/null || : > "$kept/driver-error.raw"
 if ! redact "$kept/driver-error.raw" > "$kept/driver-error.txt"; then
  rm -rf "$kept"; echo "its output was not kept"; return 1
 fi
 rm -f "$kept/driver-error.raw"
 if ! : > "$kept/frame.txt"; then
  rm -rf "$kept"; echo "its output was not kept"; return 1
 fi
 for f in "$private/$name"/FAILED-*.txt; do
  [ -f "$f" ] || continue
  if ! awk '
   BEGIN { secret = "API_KEY|GEN_PASS|REG_PASS" }
   /^(ok|changed|would_change|skipped|failed|[|\/\\-]) +(connect|check|docker|swarm|network|user|binary|key|token|config|service|cli_config|cli_token|verify|cli_key)( |$)/ {
    t = ""; if (match($0, /\([0-9]+\.[0-9]s\)$/)) t = " " substr($0, RSTART, RLENGTH); print $1, $2 t; next }
   $0 ~ ("^Value of (" secret ")( |:|$)") {
    match($0, "(" secret ")"); print "Value of", substr($0, RSTART, RLENGTH); next }
   $0 ~ ("^(stored|generated|kept) (" secret ")( |:|$)") {
    match($0, "(" secret ")"); print $1, substr($0, RSTART, RLENGTH); next }
   /^(secrets|next steps):$/ { print; next }
   /^admin token for / { print "admin token shown"; next }
  ' "$f" >> "$kept/frame.txt"; then
   rm -rf "$kept"; echo "its output was not kept"; return 1
  fi
 done
 if ! mv "$kept" "$dest"; then
  rm -rf "$kept" "$dest"; echo "its output was not kept"; return 1
 fi
 echo "see tui-guided-setup/$name-failure"
 return 0
}
cat > "$private/cancel.json" <<'JSON'
[
 {"wait_text":"connect (", "timeout":"100s"},
 {"snapshot":"pending"},
 {"keys":["ctrl+c"]},
 {"expect_exit":1, "timeout":"100s"},
 {"snapshot":"cancelled"}
]
JSON
if ! "$TUI_BIN" --command "$cmd --listen $ip:7710" --size 120x40 --script "$private/cancel.json" --out "$private/cancel" --timeout 180s --env "PATH=$private/gate:$PATH" --env "SSH_GATE=$private/ssh-gate" >"$private/cancel.err" 2>&1; then
 note=$(keep_failure cancel)
 fail "cancel pending setup" "terminal driver failed (${note:-its output was not kept})"; exit 1
fi
if ! grep -Eq '^[|/\\-] connect \([0-9]+\.[0-9]s\)' "$private/cancel/pending.txt" ||
   ! grep -Eq '^failed +connect ' "$private/cancel/cancelled.txt" ||
   grep -Eq '^[|/\\-] connect \(' "$private/cancel/cancelled.txt"; then
 fail "cancel pending setup" "missing frame or stale spinner after cancellation"; exit 1
fi
grep -E '^[|/\\-] connect \([0-9]+\.[0-9]s\)' "$private/cancel/pending.txt" > "$LOG/tui-guided-setup/pending-frame.txt"
awk '/^failed +connect / { print $1, $2 }' "$private/cancel/cancelled.txt" > "$LOG/tui-guided-setup/cancelled-frame.txt"
printf '  frame %s\n' "$(cat "$LOG/tui-guided-setup/pending-frame.txt")"
printf '  frame %s\n' "$(cat "$LOG/tui-guided-setup/cancelled-frame.txt")"
pass "cancel pending setup without stale spinner"
# Setup installs Docker from the internet when it is missing, which can take minutes. Scenarios 27 and 44 run that
# path; here Docker is installed first, without a deadline, so the guided run's waits cover setup's own steps.
if ! bash "$fixture" install-docker "$server" > "$LOG/tui-guided-setup/install-docker.log" 2>&1; then
 fail "install Docker on the setup fixture" "see tui-guided-setup/install-docker.log"; exit 1
fi
cat > "$private/fresh.json" <<'JSON'
[
 {"wait_text":"Value of API_KEY", "timeout":"100s"},
 {"snapshot":"token"},
 {"type":"fixture-api-value"}, {"keys":["enter"]},
 {"wait_text":"stored API_KEY"},
 {"wait_text":"Value of REG_PASS"},
 {"type":"fixture-registry-value"}, {"keys":["enter"]},
 {"wait_text":"deploy every app in depends_on order"}, {"snapshot":"complete"}, {"expect_exit":0}
]
JSON
if ! "$TUI_BIN" --command "$cmd --server primary" --size 300x100 --script "$private/fresh.json" --out "$private/fresh" --timeout 180s >"$private/fresh.err" 2>&1; then
 note=$(keep_failure fresh)
 fail "fresh guided setup" "terminal driver failed (${note:-its output was not kept})"; exit 1
fi
screen="$private/fresh/token.txt"
# Preserve only the setup step names and status words from the real terminal frame.
mkdir -p "$LOG/tui-guided-setup" || exit 1
awk '/^admin token for / { exit } /^(ok|changed|would_change|skipped|failed) +(connect|check|docker|swarm|network|user|binary|key|token|config|service|cli_config|cli_token|verify|cli_key) / { print $1, $2 }' "$screen" > "$LOG/tui-guided-setup/setup-frame.txt"
if [ ! -s "$LOG/tui-guided-setup/setup-frame.txt" ]; then fail "setup frame" "no terminal step lines captured"; exit 1; fi
if ! grep -Fxq 'ok docker' "$LOG/tui-guided-setup/setup-frame.txt"; then fail "setup frame" "docker step was not ok"; exit 1; fi
while IFS= read -r line; do printf '  frame %s\n' "$line"; done < "$LOG/tui-guided-setup/setup-frame.txt"
token=$(sed -n 's/.*admin token for .*: \(bty\.[a-z0-9.-]*\).*/\1/p' "$screen" | head -1)
if [[ $token != bty.* ]]; then fail "fresh token" "token was not captured"; exit 1; fi
if ! awk '/admin token for / && !token { token=NR } /^secrets:/ && !guide { guide=NR } /Value of / && !prompt { prompt=NR } END { exit !(token && guide > token && prompt > guide) }' "$screen"; then
 fail "fresh output order" "expected token, secrets, then prompt"; exit 1
fi
for text in 'secrets:' 'generated GEN_PASS' 'stored API_KEY' 'next steps:' 'keep the copy of the encryption key'; do
 if ! grep -Fq "$text" "$private/fresh/complete.txt"; then fail "fresh guide" "missing $text"; exit 1; fi
done
if grep -Fq ' --project ' "$private/fresh/complete.txt"; then fail "fresh guide" "unexpected project flag"; exit 1; fi
for step in 'set the secrets the manifest names:' 'set the secrets still missing (' 'generate the secrets the manifest declares:'; do
 if grep -Fq "$step" "$private/fresh/complete.txt"; then fail "fresh guide" "secrets step remains"; exit 1; fi
done
pass "fresh guided setup"
token_env=$(jq -er --arg p "$PROJECT" --arg e "$ENVN" '.projects[$p].environments[$e].token_env | strings' "$cfg") || { fail "token variable" "missing token_env"; exit 1; }
expected_env=$(printf 'BERTHY_%s_%s_TOKEN' "$PROJECT" "$ENVN" | tr '[:lower:]-' '[:upper:]_')
if [[ ! $token_env =~ ^[A-Za-z_][A-Za-z0-9_]*$ || $token_env != "$expected_env" ]]; then fail "token variable" "unexpected token_env"; exit 1; fi
export "$token_env=$token"
out=$("$BIN" secret list -e "$ENVN" --config "$cfg" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "list guided secrets" 0 "$code"
if [[ $out == *"every secret the manifest needs is stored"* ]]; then pass "all secrets stored"; else fail "all secrets stored" "inventory did not report all secrets stored"; fi
cat > "$private/repeat.json" <<'JSON'
[{"wait_text":"every secret the manifest needs is stored", "timeout":"100s"}, {"snapshot":"complete"}, {"expect_exit":0}]
JSON
if "$TUI_BIN" --command "$cmd" --size 300x100 --script "$private/repeat.json" --out "$private/repeat" --timeout 180s --redact-env "$token_env" >"$private/repeat.err" 2>&1; then
 repeat="$private/repeat/complete.txt"
 keyfile=$(find "$FS_HOME/.berthy/keys" -type f -name "berthy.$PROJECT.$ENVN.*.key" -print) || exit 1
 if ! key_absent "$keyfile" "$private/repeat" "$token_env"; then fail "repeat capture" "private data check failed"; exit 1; fi
 mkdir -p "$LOG/tui-guided-setup" || exit 1
 cp "$repeat" "$LOG/tui-guided-setup/complete.txt" || exit 1
 repeat_text=$(< "$repeat")
 if [[ $repeat_text == *"Value of "* || $repeat_text == *"$token"* || $repeat_text != *"the admin token is in \$$token_env"* ]] || ! awk '/^secrets:/ { guide=NR } /every secret the manifest needs is stored/ { inventory=NR } END { exit !(guide && inventory > guide) }' "$repeat"; then fail "repeat guided setup" "unexpected prompt or token, or missing inventory"; else pass "repeat guided setup without prompts"; fi
else note=$(keep_failure repeat); fail "repeat guided setup" "terminal driver failed (${note:-its output was not kept})"; fi
out=$("$BIN" setup -e "$ENVN" --host "ops@$server" --listen "$ip:7710" --manifest-dir "$MAN" --config "$cfg" </dev/null 2>&1); code=$?
expect_code "piped setup" 0 "$code"
if [[ $out == *"keep the copy of the encryption key"* && $out != *" --project "* && $out == *"generate the secrets the manifest declares:"* && $out == *"set the secrets the manifest names:"* && $out != *$'\nsecrets:'* && $out != *"Value of "* ]]; then
 pass "piped setup prints manual secrets steps"
else
 fail "piped setup prints manual secrets steps" "missing manual step or unexpected guide or prompt"
fi
unset "$token_env" token
finish
