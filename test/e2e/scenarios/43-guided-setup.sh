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
cmd="$BIN setup -e $ENVN --host ops@$server --listen $ip:7710 --manifest-dir $MAN --config $cfg"
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
if ! "$TUI_BIN" --command "$cmd" --size 300x100 --script "$private/fresh.json" --out "$private/fresh" --timeout 180s >/dev/null 2>&1; then fail "fresh guided setup" "terminal driver failed"; exit 1; fi
screen="$private/fresh/token.txt"
# Preserve only the setup step names and status words from the real terminal frame.
mkdir -p "$LOG/tui-guided-setup" || exit 1
awk '/^admin token for / { exit } /^(ok|changed|would_change|skipped|failed) +(connect|check|docker|swarm|network|user|binary|key|token|config|service|cli_config|cli_token|verify|cli_key) / { print $1, $2 }' "$screen" > "$LOG/tui-guided-setup/setup-frame.txt"
if [ ! -s "$LOG/tui-guided-setup/setup-frame.txt" ]; then fail "setup frame" "no terminal step lines captured"; exit 1; fi
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
if "$TUI_BIN" --command "$cmd" --size 300x100 --script "$private/repeat.json" --out "$private/repeat" --timeout 180s --redact-env "$token_env" >/dev/null 2>&1; then
 repeat="$private/repeat/complete.txt"
 keyfile=$(find "$FS_HOME/.berthy/keys" -type f -name "berthy.$PROJECT.$ENVN.*.key" -print) || exit 1
 if ! python3 - "$keyfile" "$private/repeat" "$token_env" <<'PYCODE'
import base64, os, pathlib, sys
try:
    key = pathlib.Path(sys.argv[1]).read_bytes()
    if len(key) != 32:
        raise ValueError()
    forms = [key, key.hex().encode()]
    for f in (base64.b64encode, base64.urlsafe_b64encode):
        encoded = f(key)
        forms.extend((encoded, encoded.rstrip(b"=")))
    forms.append(os.environ[sys.argv[3]].encode())
    for path in pathlib.Path(sys.argv[2]).rglob("*"):
        if path.is_file() and any(form in path.read_bytes() for form in forms):
            raise ValueError()
except Exception:
    sys.exit(1)
PYCODE
 then fail "repeat capture" "private data check failed"; exit 1; fi
 mkdir -p "$LOG/tui-guided-setup" || exit 1
 cp "$repeat" "$LOG/tui-guided-setup/complete.txt" || exit 1
 repeat_text=$(< "$repeat")
 if [[ $repeat_text == *"Value of "* || $repeat_text == *"$token"* || $repeat_text != *"the admin token is in \$$token_env"* ]] || ! awk '/^secrets:/ { guide=NR } /every secret the manifest needs is stored/ { inventory=NR } END { exit !(guide && inventory > guide) }' "$repeat"; then fail "repeat guided setup" "unexpected prompt or token, or missing inventory"; else pass "repeat guided setup without prompts"; fi
else fail "repeat guided setup" "terminal driver failed"; fi
out=$("$BIN" setup -e "$ENVN" --host "ops@$server" --listen "$ip:7710" --manifest-dir "$MAN" --config "$cfg" </dev/null 2>&1); code=$?
expect_code "piped setup" 0 "$code"
if [[ $out == *"keep the copy of the encryption key"* && $out != *" --project "* && $out == *"generate the secrets the manifest declares:"* && $out == *"set the secrets the manifest names:"* && $out != *$'\nsecrets:'* && $out != *"Value of "* ]]; then
 pass "piped setup prints manual secrets steps"
else
 fail "piped setup prints manual secrets steps" "missing manual step or unexpected guide or prompt"
fi
unset "$token_env" token
finish
