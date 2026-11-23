#!/bin/bash
# Run guided setup against a disposable SSH server and no credential store.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
server="g${PROJECT}-${ENVN}-${FIXTURE_ID:0:6}"
[[ $server =~ ^[a-z][a-z0-9-]{0,30}$ ]] || exit 1
export FS_HOME="$RUN/guided-setup-home" FS_DOCKER_HOST="unix://$SOCK" DOCKER_CONFIG="$E2E_ROOT/docker-config"
mkdir -p "$FS_HOME/bin"
ln -sf "$DOCKER_BIN" "$FS_HOME/bin/docker"
export PATH="$FS_HOME/bin:/usr/bin:/bin"
fixture="$REPO/test/fakeserver/fakeserver.sh"
private=$(mktemp -d /dev/shm/berthy-guided.XXXXXXXX) || exit 1
chmod 700 "$private"
cleanup() { bash "$fixture" rm "$server" >/dev/null 2>&1 || echo "could not remove setup fixture" >&2; rm -rf "$private"; }
trap 'rm -rf "$private"' EXIT
if [ -n "$(d ps -aq --filter "name=^/${server}\$" --filter 'label=berthy.fakeserver=1')" ]; then echo "fixture name already in use" >&2; exit 1; fi
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
 {"wait_text":"next steps:"}, {"snapshot":"complete"}, {"expect_exit":0}
]
JSON
if ! "$TUI_BIN" --command "$cmd" --size 300x100 --script "$private/fresh.json" --out "$private/fresh" --timeout 180s >/dev/null 2>&1; then fail "fresh guided setup" "terminal driver failed"; exit 1; fi
screen="$private/fresh/token.txt"
token=$(sed -n 's/.*admin token for .*: \(bty\.[a-z0-9.-]*\).*/\1/p' "$screen" | head -1)
if [[ $token != bty.* ]]; then fail "fresh token" "token was not captured"; exit 1; fi
for text in 'secrets:' 'generated GEN_PASS' 'stored API_KEY' 'next steps:'; do
 if ! grep -Fq "$text" "$private/fresh/complete.txt"; then fail "fresh guide" "missing $text"; exit 1; fi
done
if grep -Fq 'set the secrets the manifest names:' "$private/fresh/complete.txt"; then fail "fresh guide" "manual secrets step remains"; exit 1; fi
pass "fresh guided setup"
token_env=$(jq -r --arg p "$PROJECT" --arg e "$ENVN" '.projects[$p].environments[$e].token_env' "$cfg")
export "$token_env=$token"
out=$("$BIN" secret list -e "$ENVN" --config "$cfg" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "list guided secrets" 0 "$code" "$out"
expect_contains "all secrets stored" "$out" "every secret the manifest needs is stored"
cat > "$private/repeat.json" <<'JSON'
[{"wait_text":"every secret the manifest needs is stored", "timeout":"100s"}, {"snapshot":"complete"}, {"expect_exit":0}]
JSON
if "$TUI_BIN" --command "$cmd" --size 300x100 --script "$private/repeat.json" --out "$LOG/tui-guided-setup" --timeout 180s --env "$token_env=$token" --redact-env "$token_env" >/dev/null 2>&1; then
 repeat="$LOG/tui-guided-setup/complete.txt"
 if grep -Fq 'Value of ' "$repeat" || ! grep -Fq "the admin token is in \$$token_env" "$repeat"; then fail "repeat guided setup" "unexpected prompt or missing token source"; else pass "repeat guided setup without prompts"; fi
else fail "repeat guided setup" "terminal driver failed"; fi
out=$("$BIN" setup -e "$ENVN" --host "ops@$server" --listen "$ip:7710" --manifest-dir "$MAN" --config "$cfg" </dev/null 2>&1); code=$?
expect_code "piped setup" 0 "$code" "$out"
expect_contains "piped next steps" "$out" "set the secrets the manifest names:"
unset "$token_env" token
finish
