#!/bin/bash
# 20-secret-generate proves `secret generate` on the real Swarm. The agent makes the value, so the CLI prints the name, the length
# and the format and never the value. A deploy gives the container the value, 64 hex characters by default. A second generate
# without --replace is refused and changes nothing, so a redeploy still runs the first value. --replace with --format alnum and
# --bytes 20 replaces it with 20 letters and digits, and the next deploy runs those. A registry password, a name that is not a
# secret name and an agent token without admin scope are refused. The audit has one secret_generate row per successful generate,
# and no value is in the audit or the agent's logs.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

# gen_value SPEC: the value of GEN_PASS in a Swarm service spec, or nothing when the spec does not hold GEN_PASS.
gen_value() { grep -oE 'GEN_PASS=[0-9A-Za-z]*' <<<"$1" | head -n 1 | cut -d= -f2; }

# manifest_gen: the e2e web image app, one role with a health check that passes, and GEN_PASS as its secret variable.
manifest_gen() {
  cat <<YML
project: $PROJECT
network: $NETWORK
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
      secret:
        - GEN_PASS
YML
}
manifest_gen | write_manifest

out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "the manifest that names GEN_PASS validates" 0 "$code" "$out"

# 1. A generate prints the name, the length and the format, and the value is not on the screen.
out=$(adm secret generate GEN_PASS 2>&1); code=$?
expect_code "secret generate GEN_PASS exits 0" 0 "$code" "$out"
expect_eq "it prints one line that names the secret and holds no value" "generated GEN_PASS (32 bytes, hex)" "$out"

# 2. A deploy gives the container the value: 64 hex characters.
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy web" 0 "$code" "$out"
first=$(gen_value "$(service_spec "$PROJECT-$ENVN-web-main")")
if [[ $first =~ ^[0-9a-f]{64}$ ]]; then
  pass "the service runs GEN_PASS with 64 hex characters"
else
  fail "the service runs GEN_PASS with 64 hex characters" "got ${#first} characters"
fi

# 3. A second generate without --replace is refused, and a dry run says so. A redeploy still runs the first value.
out=$(adm secret generate GEN_PASS 2>&1); code=$?
expect_code "a second generate without --replace exits 1" 1 "$code" "$out"
expect_contains "the refusal says --replace replaces it" "$out" "secret GEN_PASS exists; --replace replaces it"
out=$(adm secret generate GEN_PASS --dry-run 2>&1); code=$?
expect_code "a dry run of a stored name exits 0" 0 "$code" "$out"
expect_eq "the dry run says a real run is refused without --replace" "GEN_PASS exists: a real run is refused unless --replace is given" "$out"
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "a redeploy after the refusal exits 0" 0 "$code" "$out"
expect_eq "the refused generate left the value the service runs" "$first" "$(gen_value "$(service_spec "$PROJECT-$ENVN-web-main")")"

# 4. --replace with --format alnum and --bytes 20 replaces the value, and the next deploy runs 20 letters and digits.
out=$(adm secret generate GEN_PASS --format alnum --bytes 20 --replace 2>&1); code=$?
expect_code "a replacing generate exits 0" 0 "$code" "$out"
expect_eq "it says it replaced the secret, counted in characters" "replaced GEN_PASS (20 characters, alnum)" "$out"
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy after the replace" 0 "$code" "$out"
second=$(gen_value "$(service_spec "$PROJECT-$ENVN-web-main")")
if [[ $second =~ ^[A-Za-z0-9]{20}$ ]]; then
  pass "the service runs GEN_PASS with 20 letters and digits"
else
  fail "the service runs GEN_PASS with 20 letters and digits" "got ${#second} characters"
fi
if [ -n "$second" ] && [ "$second" != "$first" ]; then
  pass "the replaced value is not the first one"
else
  fail "the replaced value is not the first one" "the service still runs the first value"
fi

# 5. Requests the agent refuses: a length out of range, an unknown format, a name that is not a secret name, and a registry password.
out=$(adm secret generate GEN_PASS --bytes 15 --replace 2>&1); code=$?
expect_code "bytes below 16 is refused" 1 "$code" "$out"
expect_contains "the refusal gives the range" "$out" "bytes must be from 16 to 256"
out=$(adm secret generate GEN_PASS --format uuid --replace 2>&1); code=$?
expect_code "an unknown format is refused" 1 "$code" "$out"
expect_contains "the refusal lists the formats" "$out" "format must be hex, base64 or alnum"
out=$(adm secret generate 1BAD 2>&1); code=$?
expect_code "a name that is not a secret name is refused" 1 "$code" "$out"
out=$(printf 'registry-token-e2e' | adm secret set REG_TOKEN --registry ghcr.io 2>&1); code=$?
expect_code "a registry password is set for the refusal" 0 "$code" "$out"
out=$(adm secret generate REG_TOKEN --replace 2>&1); code=$?
expect_code "a registry password is refused" 1 "$code" "$out"
expect_contains "the refusal says a registry password cannot be generated" "$out" "REG_TOKEN is a registry password and cannot be generated"

# 6. A generate needs an admin token. The ci token has deploy scope, so it is refused and stores nothing.
out=$(ci secret generate GEN_CI 2>&1); code=$?
expect_code "an agent token with deploy scope cannot generate" 1 "$code" "$out"
expect_not_contains "the refused generate stored nothing" "$(adm secret list 2>&1)" "GEN_CI"

# 7. A dry run of a new name says what it would do and stores nothing.
out=$(adm secret generate NEW_ONE --dry-run 2>&1); code=$?
expect_code "a dry run of a new name exits 0" 0 "$code" "$out"
expect_eq "it says it would generate the new secret" "would generate NEW_ONE (32 bytes, hex)" "$out"
expect_not_contains "the dry run stored nothing" "$(adm secret list 2>&1)" "NEW_ONE"

# 8. The audit has one succeeded secret_generate row per successful generate, and none for the refused ones. No value is in it.
out=$(adm audit 2>&1); code=$?
expect_code "audit answers" 0 "$code" "$out"
expect_eq "audit has two succeeded secret_generate rows for GEN_PASS" "2" \
  "$(printf '%s\n' "$out" | awk 'BEGIN { RS = "" } /action: secret_generate:GEN_PASS/ && /outcome: succeeded/ { n++ } END { print n + 0 }')"
if [ -n "$first" ] && [ -n "$second" ]; then
  expect_not_contains "the audit holds neither value" "$out" "$first"
  expect_not_contains "the audit holds the replacement value too" "$out" "$second"
fi

# 9. No generated value is in the agent's logs.
found=0
for v in "$first" "$second"; do
  if [ -n "$v" ] && grep -rqF -- "$v" "$LOG"; then
    found=1
  fi
done
expect_eq "no generated value is in the agent's logs" "0" "$found"

finish
