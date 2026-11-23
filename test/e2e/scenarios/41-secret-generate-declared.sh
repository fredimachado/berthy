#!/bin/bash
# 41-secret-generate-declared proves nameless `secret generate` on the real Swarm: declared secrets are generated before
# any deploy, kept on a second run, and refused when a reader was deployed or a fixed external volume already exists. The
# named form keeps the same guard and `--replace` escape hatch. Values never appear in assertions or the agent's logs.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

# Fixture-owned external volume: refuse a pre-existing same name, label what this run creates, remove only that.
# EXT_OWNER is per-run so two concurrent fixtures cannot claim each other's create.
EXT_VOL="${PROJECT}_${ENVN}_cache_ext"
EXT_OWNER="${FIXTURE_ID}.$(openssl rand -hex 8)"
created_ext=

# ext_fixture_label NAME: the berthy.e2e.fixture label, or empty when absent / missing.
ext_fixture_label() {
  d volume inspect -f '{{index .Labels "berthy.e2e.fixture"}}' "$1" 2>/dev/null || true
}

# remove_owned_ext [trap]: remove created_ext only when it carries this run's EXT_OWNER. Mid-scenario (default) fails
# the scenario on a foreign label or rm error; EXIT trap mode is best-effort and silent. Ownership clears only after rm.
remove_owned_ext() {
  local mode=${1:-}
  if [ -z "$created_ext" ]; then
    return 0
  fi
  name=$created_ext
  if [ "$(ext_fixture_label "$name")" != "$EXT_OWNER" ]; then
    if [ "$mode" = trap ]; then
      return 0
    fi
    fail "remove only a fixture-owned external volume" "label is not this run's; left untouched"
    exit 1
  fi
  if ! d volume rm "$name" >/dev/null; then
    if [ "$mode" = trap ]; then
      return 0
    fi
    fail "remove the fixture-owned external volume" "docker volume rm failed"
    exit 1
  fi
  created_ext=
  if [ "$mode" != trap ]; then
    pass "removed the fixture-owned external volume"
  fi
}

cleanup_ext() { remove_owned_ext trap; }
trap cleanup_ext EXIT

# create_owned_ext: create EXT_VOL for this run, or exit before touching a colliding name.
create_owned_ext() {
  if d volume inspect "$EXT_VOL" >/dev/null 2>&1; then
    fail "the fixture volume name is free before create" "$EXT_VOL exists"
    exit 1
  fi
  if ! d volume create --label "berthy.e2e.fixture=$EXT_OWNER" "$EXT_VOL" >/dev/null; then
    fail "create the fixture-owned external volume" "docker volume create failed"
    exit 1
  fi
  if [ "$(ext_fixture_label "$EXT_VOL")" != "$EXT_OWNER" ]; then
    fail "the new external volume is labeled for this fixture" "label missing or foreign"
    exit 1
  fi
  created_ext=$EXT_VOL
  pass "created the fixture-owned external volume"
}

# gen_value SPEC: the value of GEN_PASS in a Swarm service spec, or nothing when absent.
gen_value() { grep -oE 'GEN_PASS=[0-9A-Za-z]*' <<<"$1" | head -n 1 | cut -d= -f2; }

# write_base_manifest: image web + Compose db with a named volume, both declared (GEN_PASS alnum 20).
write_base_manifest() {
  cat <<YML | write_manifest
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
  db:
    kind: compose
    file: compose.yml
    env:
      secret:
        - DB_PASS
secrets:
  GEN_PASS:
    generate:
      format: alnum
      bytes: 20
  DB_PASS:
    generate:
      format: hex
YML
  cat > "$MAN/compose.yml" <<YML
services:
  db:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sleep", "3600"]
    environment: ["DB_PASS=\${DB_PASS}"]
    volumes: ["data:/data"]
volumes:
  data: {}
YML
}

write_base_manifest

out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "the declared-secrets manifest validates" 0 "$code" "$out"

# 1. Before any deploy, the dry run prints two would-generate lines; named dry-run of DB_PASS agrees.
out=$(adm secret generate --dry-run --manifest-dir "$MAN" 2>&1); code=$?
expect_code "nameless dry run before deploy exits 0" 0 "$code" "$out"
expect_contains "dry run would generate DB_PASS" "$out" "would generate DB_PASS"
expect_contains "dry run would generate GEN_PASS" "$out" "would generate GEN_PASS"
out=$(adm secret generate DB_PASS --dry-run --manifest-dir "$MAN" 2>&1); code=$?
expect_code "named dry run of DB_PASS exits 0" 0 "$code" "$out"
expect_contains "named dry run would generate DB_PASS" "$out" "would generate DB_PASS"

# 2. Real nameless run generates both; compare the whole output to the metadata lines only (never print $out on failure).
want_gen=$'generated DB_PASS (32 bytes, hex)\ngenerated GEN_PASS (20 characters, alnum)\n'
out=$(adm secret generate --manifest-dir "$MAN" 2>&1); code=$?
expect_code "nameless generate exits 0" 0 "$code"
if [ "$out" = "$want_gen" ]; then
  pass "nameless generate prints only the two metadata lines"
else
  fail "nameless generate prints only the two metadata lines" "output did not match the expected metadata lines"
fi

# 3. Second run keeps both; deploy web and db; service holds 20-character GEN_PASS.
out=$(adm secret generate --manifest-dir "$MAN" 2>&1); code=$?
expect_code "second nameless generate exits 0" 0 "$code" "$out"
expect_contains "kept DB_PASS" "$out" "kept DB_PASS: it is already stored"
expect_contains "kept GEN_PASS" "$out" "kept GEN_PASS: it is already stored"
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy web" 0 "$code" "$out"
out=$(adm deploy db --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy db" 0 "$code" "$out"
gen=$(gen_value "$(service_spec "$PROJECT-$ENVN-web-main")")
if [[ $gen =~ ^[A-Za-z0-9]{20}$ ]]; then
  pass "the service runs GEN_PASS with 20 letters and digits"
else
  fail "the service runs GEN_PASS with 20 letters and digits" "got ${#gen} characters"
fi

# 4. After secret rm DB_PASS, nameless refuses because db was deployed; list still lacks DB_PASS.
out=$(adm secret rm DB_PASS 2>&1); code=$?
expect_code "secret rm DB_PASS" 0 "$code" "$out"
out=$(adm secret generate --manifest-dir "$MAN" 2>&1); code=$?
expect_code "nameless after deploy refuses DB_PASS" 1 "$code" "$out"
expect_contains "refusal names deployed app db" "$out" "not generated DB_PASS: app db was deployed here and may hold the old value"
expect_not_contains "secret list still lacks DB_PASS" "$(adm secret list 2>&1)" "DB_PASS"

# 5. Named generate without --replace keeps the guard, from the manifest directory and from an empty directory.
out=$(adm secret generate DB_PASS --manifest-dir "$MAN" 2>&1); code=$?
expect_code "named generate without replace exits 1" 1 "$code" "$out"
expect_contains "named refusal names db" "$out" "app db was deployed here"
empty=$(mktemp -d "$RUN/empty-manifest.XXXXXX")
out=$(adm secret generate DB_PASS --manifest-dir "$empty" 2>&1); code=$?
expect_code "named generate from empty directory exits 1" 1 "$code" "$out"
expect_contains "empty-dir named refusal names db" "$out" "app db was deployed here"
out=$(adm secret generate DB_PASS --dry-run --manifest-dir "$MAN" 2>&1); code=$?
expect_code "named dry run after deploy exits 0" 0 "$code" "$out"
expect_contains "named dry run says was deployed here" "$out" "was deployed here"
rmdir "$empty"

# 6. Manifest without db, only web reads DB_PASS: nameless still names db. After remove db, the same (releases stay).
cat <<YML | write_manifest
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
        - DB_PASS
secrets:
  GEN_PASS:
    generate:
      format: alnum
      bytes: 20
  DB_PASS:
    generate:
      format: hex
YML
out=$(adm secret generate --manifest-dir "$MAN" 2>&1); code=$?
expect_code "nameless without db app still refuses DB_PASS" 1 "$code" "$out"
expect_contains "refusal still names db from releases" "$out" "not generated DB_PASS: app db was deployed here"
out=$(adm remove db 2>&1); code=$?
expect_code "remove db" 0 "$code" "$out"
out=$(adm secret generate --manifest-dir "$MAN" 2>&1); code=$?
expect_code "nameless after remove db still refuses" 1 "$code" "$out"
expect_contains "refusal after remove still names db" "$out" "not generated DB_PASS: app db was deployed here"

# 7. Piped secret set DB_PASS, then nameless keeps it.
out=$(printf 'set-by-pipe-for-db' | adm secret set DB_PASS 2>&1); code=$?
expect_code "piped secret set DB_PASS" 0 "$code" "$out"
out=$(adm secret generate --manifest-dir "$MAN" 2>&1); code=$?
expect_code "nameless after set keeps DB_PASS" 0 "$code" "$out"
expect_contains "kept DB_PASS after set" "$out" "kept DB_PASS: it is already stored"

# 8. Never-deployed Compose reader with an external volume: refuse, then interpolating name, then generate after remove.
write_base_manifest
cat <<YML | write_manifest
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
  db:
    kind: compose
    file: compose.yml
    env:
      secret:
        - DB_PASS
  cache:
    kind: compose
    file: cache.yml
    env:
      secret:
        - CACHE_PASS
secrets:
  GEN_PASS:
    generate:
      format: alnum
      bytes: 20
  DB_PASS:
    generate:
      format: hex
  CACHE_PASS:
    generate:
      format: hex
YML
cat > "$MAN/compose.yml" <<YML
services:
  db:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sleep", "3600"]
    environment: ["DB_PASS=\${DB_PASS}"]
    volumes: ["data:/data"]
volumes:
  data: {}
YML
cat > "$MAN/cache.yml" <<YML
services:
  cache:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sleep", "3600"]
    environment: ["CACHE_PASS=\${CACHE_PASS}"]
    volumes: ["ext:/data"]
volumes:
  ext:
    external: true
    name: $EXT_VOL
YML
create_owned_ext
out=$(adm secret generate --manifest-dir "$MAN" 2>&1); code=$?
expect_code "nameless with existing external volume exits 1" 1 "$code" "$out"
expect_contains "nameless names the external volume" "$out" "not generated CACHE_PASS: the volume $EXT_VOL of app cache exists"
out=$(adm secret generate CACHE_PASS --manifest-dir "$MAN" 2>&1); code=$?
expect_code "named with existing external volume exits 1" 1 "$code" "$out"
expect_contains "named names the external volume" "$out" "volume $EXT_VOL of app cache exists"
cat > "$MAN/cache.yml" <<YML
services:
  cache:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sleep", "3600"]
    environment: ["CACHE_PASS=\${CACHE_PASS}"]
    volumes: ["ext:/data"]
volumes:
  ext:
    external: true
    name: \${STACK}_ext
YML
out=$(adm secret generate --manifest-dir "$MAN" 2>&1); code=$?
expect_code "interpolated volume name exits 1" 1 "$code" "$out"
expect_contains "interpolated name cannot be established" "$out" "the state of app cache cannot be established"
remove_owned_ext
cat > "$MAN/cache.yml" <<YML
services:
  cache:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sleep", "3600"]
    environment: ["CACHE_PASS=\${CACHE_PASS}"]
    volumes: ["ext:/data"]
volumes:
  ext:
    external: true
    name: $EXT_VOL
YML
out=$(adm secret generate --manifest-dir "$MAN" 2>&1); code=$?
expect_code "nameless generates CACHE_PASS after volume removed" 0 "$code" "$out"
expect_contains "generated CACHE_PASS" "$out" "generated CACHE_PASS"

# 9. Failed first deploy of late: LATE_PASS generates, deploy fails, rm, nameless refuses.
cat <<YML | write_manifest
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
  db:
    kind: compose
    file: compose.yml
    env:
      secret:
        - DB_PASS
  late:
    kind: image
    image: $REG/e2e/web
    roles:
      main:
        health:
          test: ["CMD", "false"]
          interval: 1s
          timeout: 1s
          start_period: 0s
          retries: 1
    env:
      secret:
        - LATE_PASS
secrets:
  GEN_PASS:
    generate:
      format: alnum
      bytes: 20
  DB_PASS:
    generate:
      format: hex
  LATE_PASS:
    generate:
      format: hex
YML
cat > "$MAN/compose.yml" <<YML
services:
  db:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sleep", "3600"]
    environment: ["DB_PASS=\${DB_PASS}"]
    volumes: ["data:/data"]
volumes:
  data: {}
YML
out=$(adm secret generate --manifest-dir "$MAN" 2>&1); code=$?
expect_code "nameless generates LATE_PASS before failed deploy" 0 "$code" "$out"
expect_contains "generated LATE_PASS" "$out" "generated LATE_PASS"
out=$(adm deploy late --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy late with failing health exits 1" 1 "$code" "$out"
out=$(adm secret rm LATE_PASS 2>&1); code=$?
expect_code "secret rm LATE_PASS" 0 "$code" "$out"
out=$(adm secret generate --manifest-dir "$MAN" 2>&1); code=$?
expect_code "nameless after failed late deploy exits 1" 1 "$code" "$out"
expect_contains "refusal names late" "$out" "not generated LATE_PASS: app late was deployed here"

# 10. Named --replace generates DB_PASS.
out=$(adm secret generate DB_PASS --replace --manifest-dir "$MAN" 2>&1); code=$?
expect_code "named generate --replace exits 0" 0 "$code" "$out"
expect_contains "replaced or generated DB_PASS" "$out" "DB_PASS"

# 11. Audit has one succeeded secret_generate row per generated secret (DB_PASS twice with --replace); no known value in logs or audit.
out=$(adm audit 2>&1); code=$?
expect_code "audit answers" 0 "$code" "$out"
expect_eq "audit has one succeeded secret_generate for GEN_PASS" "1" \
  "$(printf '%s\n' "$out" | awk 'BEGIN { RS = "" } /action: secret_generate:GEN_PASS/ && /outcome: succeeded/ { n++ } END { print n + 0 }')"
expect_eq "audit has two succeeded secret_generate rows for DB_PASS" "2" \
  "$(printf '%s\n' "$out" | awk 'BEGIN { RS = "" } /action: secret_generate:DB_PASS/ && /outcome: succeeded/ { n++ } END { print n + 0 }')"
expect_eq "audit has one succeeded secret_generate for CACHE_PASS" "1" \
  "$(printf '%s\n' "$out" | awk 'BEGIN { RS = "" } /action: secret_generate:CACHE_PASS/ && /outcome: succeeded/ { n++ } END { print n + 0 }')"
expect_eq "audit has one succeeded secret_generate for LATE_PASS" "1" \
  "$(printf '%s\n' "$out" | awk 'BEGIN { RS = "" } /action: secret_generate:LATE_PASS/ && /outcome: succeeded/ { n++ } END { print n + 0 }')"
found=0
if [ -n "$gen" ]; then
  if grep -rqF -- "$gen" "$LOG"; then
    found=1
  fi
  if grep -qF -- "$gen" <<<"$out"; then
    found=1
  fi
fi
expect_eq "no GEN_PASS value is in the agent's logs or audit" "0" "$found"

finish
