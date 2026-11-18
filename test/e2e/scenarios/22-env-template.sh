#!/bin/bash
# 22-env-template proves env.template on the real Swarm and in a Compose app. The web app's DATABASE_URL is a template built
# from the secret TPL_PASS: the service runs the filled value, config show prints the template and not the value, exec shows
# the variable redacted, and the stored release keeps the template. secret list --manifest-dir names a secret only a template
# reads when it is not stored. A deploy whose template reads a secret that is not stored fails and names the secret, and the
# running service is unchanged. A malformed template fails config validate with its file and line. A Compose app's .env file
# gets the filled value.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

# expect_line NAME TEXT LINE: TEXT has LINE as one whole line.
expect_line() {
  if grep -qxF -- "$3" <<<"$2"; then
    pass "$1"
  else
    fail "$1" "no line '$3'"
    detail_block "$2"
  fi
}

# manifest TEMPLATE: the web image app and the shop Compose app. Both build DATABASE_URL from TEMPLATE.
manifest() {
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
      plain:
        LOG_LEVEL: info
      template:
        DATABASE_URL: "$1"
  shop:
    kind: compose
    file: compose.yml
    env:
      template:
        DB_URL: "$1"
YML
}
mkdir -p "$MAN"
cat > "$MAN/compose.yml" <<YML
services:
  db:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sleep", "3600"]
    environment: ["DB_URL=\${DB_URL}"]
YML

TEMPLATE='postgres://u:${TPL_PASS}@db:5432/x'
FILLED='postgres://u:tpl-pass-9f3a@db:5432/x'
manifest "$TEMPLATE" | write_manifest

out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "the manifest with a template validates" 0 "$code" "$out"

# config show prints the template, which holds no secret value.
out=$("$BIN" config show -e "$ENVN" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "config show answers" 0 "$code" "$out"
expect_contains "config show prints the template" "$out" '${TPL_PASS}'

# secret list --manifest-dir names a secret that only a template reads, while it has no value.
out=$(adm secret list --manifest-dir "$MAN" 2>&1); code=$?
expect_code "secret list --manifest-dir exits 0" 0 "$code" "$out"
expect_contains "it names the secret the templates read, and both apps" "$out" "missing: TPL_PASS (needed by app shop, app web)"

# A deploy with the secret not stored stops before any change and names the secret.
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
if [ "$code" -ne 0 ]; then pass "a deploy that needs TPL_PASS is refused while it is not stored (exit $code)"; else fail "a deploy that needs TPL_PASS is refused while it is not stored" "exit 0"; fi
expect_contains "the refusal names the secret" "$out" "secret TPL_PASS is not set"
if d service inspect "$PROJECT-web-main" >/dev/null 2>&1; then
  fail "no service was created" "$PROJECT-web-main exists"
else
  pass "no service was created"
fi

out=$(printf 'tpl-pass-9f3a' | adm secret set TPL_PASS 2>&1); code=$?
expect_code "secret TPL_PASS is set" 0 "$code" "$out"
out=$(adm secret list --manifest-dir "$MAN" 2>&1); code=$?
expect_not_contains "secret list no longer reports TPL_PASS" "$out" "missing: TPL_PASS"

# The service runs the filled value; exec shows it redacted; the dry-run plan and the release do not hold it.
out=$(adm deploy web --version v1 --dry-run --manifest-dir "$MAN" 2>&1); code=$?
expect_code "the dry run answers" 0 "$code" "$out"
expect_not_contains "the plan shows no filled value" "$out" "tpl-pass-9f3a"
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy web" 0 "$code" "$out"
expect_not_contains "the deploy output shows no filled value" "$out" "tpl-pass-9f3a"
expect_contains "the service runs the filled DATABASE_URL" "$(service_spec "$PROJECT-web-main")" "\"DATABASE_URL=$FILLED\""
env_out=$(adm exec web -- env 2>&1); code=$?
expect_code "exec reads the environment of web" 0 "$code" "$env_out"
expect_line "exec shows DATABASE_URL redacted" "$env_out" "DATABASE_URL=[redacted]"
expect_line "exec shows the plain variable" "$env_out" "LOG_LEVEL=info"
expect_not_contains "exec shows no part of the password" "$env_out" "tpl-pass-9f3a"
out=$(adm history web 2>&1); code=$?
expect_code "history answers" 0 "$code" "$out"
expect_not_contains "history shows no filled value" "$out" "tpl-pass-9f3a"

# A second deploy whose template reads a secret that is not stored fails and names it. The running service does not change.
before=$(service_spec "$PROJECT-web-main")
manifest 'postgres://u:${TPL_GONE}@db:5432/x' | write_manifest
out=$(adm deploy web --version v2 --manifest-dir "$MAN" 2>&1); code=$?
if [ "$code" -ne 0 ]; then pass "a deploy with a missing template secret is refused (exit $code)"; else fail "a deploy with a missing template secret is refused" "exit 0"; fi
expect_contains "the refusal names TPL_GONE" "$out" "secret TPL_GONE is not set"
expect_eq "the running service is unchanged" "$before" "$(service_spec "$PROJECT-web-main")"
expect_contains "it still runs v1" "$(service_spec "$PROJECT-web-main")" "$WEB_V1_DIGEST"

# A malformed template fails config validate, and names its file and line.
manifest 'postgres://u:$TPL_PASS@db:5432/x' | write_manifest
out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$MAN" 2>&1); code=$?
if [ "$code" -ne 0 ]; then pass "config validate exits non-zero on a malformed template (exit $code)"; else fail "config validate exits non-zero on a malformed template" "exit 0"; fi
expect_contains "the problem names the file and line" "$out" "berthy.yml:19"
expect_contains "the problem names the variable" "$out" "apps.web.env.template.DATABASE_URL"
expect_contains "the problem says where the reference is wrong" "$out" "malformed reference at offset"

# The Compose app: its .env file gets the filled value, and the container has it.
manifest "$TEMPLATE" | write_manifest
out=$(adm deploy shop --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy the Compose app" 0 "$code" "$out"
env_file=$(cat "$RUN/state/compose/$PROJECT-shop/.env" 2>&1)
expect_line "the Compose .env gives DB_URL the filled value" "$env_file" "DB_URL='$FILLED'"
cid=$(d ps -q --filter status=running --filter label=com.docker.compose.project="$PROJECT-shop" --filter label=com.docker.compose.service=db | head -n 1)
db=""
if [ -n "$cid" ]; then
  db=$(d exec "$cid" env 2>&1)
fi
expect_line "the Compose container has DB_URL" "$db" "DB_URL=$FILLED"

finish
