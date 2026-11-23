#!/bin/bash
# Exercise the terminal guide on a fresh Swarm fixture.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"

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
      main:
        health:
          test: ["CMD", "true"]
          interval: 2s
          timeout: 2s
          start_period: 1s
          retries: 3
    env:
      secret: [GEN_PASS, API_KEY]
secrets:
  GEN_PASS:
    generate:
      format: hex
YML
printf '%s' 'initial-fixture-password' | adm secret set REG_PASS >/dev/null
trap 'rm -f "$LOG/tui-secret-guide/script.json"' EXIT
export BERTHY_GUIDE_API_TOKEN=$(openssl rand -hex 16)
export BERTHY_GUIDE_REG_TOKEN=$(openssl rand -hex 16)
out=$(drive secret-guide "$BIN secret set -e $ENVN --config $RUN/admin.json --manifest-dir $MAN" <<JSON
[
 {"wait_text":"generated GEN_PASS", "timeout":"30s"},
 {"wait_text":"Value of API_KEY (hidden"},
 {"type":"$BERTHY_GUIDE_API_TOKEN"}, {"keys":["enter"]},
 {"wait_text":"stored API_KEY"},
 {"wait_text":"REG_PASS is stored without the registry $REG"},
 {"type":"$BERTHY_GUIDE_REG_TOKEN"}, {"keys":["enter"]},
 {"wait_text":"checked: $REG lets this credential pull e2e/web"},
 {"snapshot":"complete"}, {"expect_exit":0}
]
JSON
); code=$?
rm -f "$LOG/tui-secret-guide/script.json"
expect_code "guided secret set" 0 "$code" "$out"
if grep -rF -e "$BERTHY_GUIDE_API_TOKEN" -e "$BERTHY_GUIDE_REG_TOKEN" "$LOG/tui-secret-guide" >/dev/null; then
  fail "guide artifacts contain no values" "a value appeared in a driver artifact"
else
  pass "guide artifacts contain no values"
fi
out=$(adm secret list --manifest-dir "$MAN" 2>&1); code=$?
expect_code "list after guide" 0 "$code" "$out"
expect_contains "all needs are stored" "$out" "every secret the manifest needs is stored"
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy with guided secrets" 0 "$code" "$out"
out=$(adm secret set --json --manifest-dir "$MAN" 2>&1); code=$?
expect_code "nameless JSON is refused" 2 "$code" "$out"
# Add another registry to the same manifest without changing any secret.
sed -i "/^apps:/i\\  registry.example.com:\n    username: fixture\n    password_secret: REG_PASS" "$MAN/berthy.yml"
out=$(adm secret list --manifest-dir "$MAN" 2>&1); code=$?
expect_code "list shared registry secret" 0 "$code" "$out"
expect_contains "shared registry conflict" "$out" "conflict: REG_PASS is the password_secret of"
finish
