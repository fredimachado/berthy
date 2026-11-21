#!/bin/bash
# 09a-secret-mapping proves an env.secret mapping on the real Swarm and in a Compose app. Two image apps, web and api, each map
# DATABASE_URL to their own secret, so each container has its own DATABASE_URL and no variable named after a secret. A mapping
# whose secret has no value stops its deploy before anything runs, and the refusal names the secret. A manifest that lists a variable twice fails config validate,
# and names it. A Compose app's .env file gives the mapped variable the secret's value.
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

# image_app NAME SECRET: an image app of the e2e web image, one role with a health check that passes, and DATABASE_URL taking
# the value of the secret SECRET.
image_app() {
  cat <<YML
  $1:
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
        - DATABASE_URL: $2
YML
}

# The manifest: web and api map DATABASE_URL to their own secrets, late maps it to a secret nobody sets, and shop is a Compose
# app whose DB_PASS takes the value of WEB_DB.
{
  cat <<YML
project: $PROJECT
network: $NETWORK
apps:
YML
  image_app web WEB_DB
  image_app api API_DB
  image_app late LATE_DB
  cat <<YML
  shop:
    kind: compose
    file: compose.yml
    env:
      secret:
        - DB_PASS: WEB_DB
YML
} | write_manifest
cat > "$MAN/compose.yml" <<YML
services:
  db:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sleep", "3600"]
    environment: ["DB_PASS=\${DB_PASS}"]
YML

out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "the manifest with mapped secrets validates" 0 "$code" "$out"

# One secret per app, set from a pipe so that no terminal is needed.
out=$(printf 'postgres://web' | adm secret set WEB_DB 2>&1); code=$?
expect_code "secret WEB_DB is set" 0 "$code" "$out"
out=$(printf 'postgres://api' | adm secret set API_DB 2>&1); code=$?
expect_code "secret API_DB is set" 0 "$code" "$out"

# web and api each get their own value under DATABASE_URL, and neither gets the other's value or a variable named after a secret.
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy web" 0 "$code" "$out"
out=$(adm deploy api --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy api" 0 "$code" "$out"

web_env=$(adm exec web -- env 2>&1); code=$?
expect_code "exec reads the environment of web" 0 "$code" "$web_env"
# exec redacts managed secret values, so the value is checked on the service, where Docker holds it.
expect_line "web: exec shows DATABASE_URL redacted" "$web_env" "DATABASE_URL=[redacted]"
expect_contains "web: the service runs DATABASE_URL with the value of WEB_DB" "$(service_spec "$PROJECT-$ENVN-web-main")" '"DATABASE_URL=postgres://web"'
expect_not_contains "web: no variable is named after WEB_DB" "$web_env" "WEB_DB="
expect_not_contains "web: does not have the value of API_DB" "$web_env" "postgres://api"

api_env=$(adm exec api -- env 2>&1); code=$?
expect_code "exec reads the environment of api" 0 "$code" "$api_env"
expect_line "api: exec shows DATABASE_URL redacted" "$api_env" "DATABASE_URL=[redacted]"
expect_contains "api: the service runs DATABASE_URL with the value of API_DB" "$(service_spec "$PROJECT-$ENVN-api-main")" '"DATABASE_URL=postgres://api"'
expect_not_contains "api: no variable is named after API_DB" "$api_env" "API_DB="
expect_not_contains "api: does not have the value of WEB_DB" "$api_env" "postgres://web"

# late maps DATABASE_URL to LATE_DB, which has no value. The deploy stops before it creates anything.
out=$(adm deploy late --version v1 --manifest-dir "$MAN" 2>&1); code=$?
if [ "$code" -ne 0 ]; then
  pass "deploy of a mapping whose secret is unset is refused (exit $code)"
else
  fail "deploy of a mapping whose secret is unset is refused" "exit 0"
fi
expect_contains "the refusal names the secret that has no value" "$out" "secret LATE_DB is not set"
if d service inspect "$PROJECT-$ENVN-late-main" >/dev/null 2>&1; then
  fail "no service was created for late" "$PROJECT-$ENVN-late-main exists"
else
  pass "no service was created for late"
fi

# A variable listed twice, once as a name and once as a mapping, is refused by config validate, which names it.
dup=$RUN/dup-manifest
mkdir -p "$dup"
cat > "$dup/berthy.yml" <<YML
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
    env:
      secret:
        - DATABASE_URL
        - DATABASE_URL: WEB_DB
YML
out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$dup" 2>&1); code=$?
if [ "$code" -ne 0 ]; then
  pass "config validate exits non-zero on a variable listed twice (exit $code)"
else
  fail "config validate exits non-zero on a variable listed twice" "exit 0"
fi
expect_contains "config validate names the variable listed twice" "$out" "variable DATABASE_URL is listed twice"

# A Compose app: its .env file gives DB_PASS the value of WEB_DB, the name of the secret is not written, and the container has
# the variable.
out=$(adm deploy shop --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy the Compose app" 0 "$code" "$out"
env_file=$(cat "$RUN/state/compose/$PROJECT-$ENVN-shop/.env" 2>&1)
expect_line "the Compose .env gives DB_PASS the value of WEB_DB" "$env_file" "DB_PASS='postgres://web'"
expect_not_contains "the Compose .env names no WEB_DB" "$env_file" "WEB_DB="
cid=$(d ps -q --filter status=running --filter label=com.docker.compose.project="$PROJECT-$ENVN-shop" --filter label=com.docker.compose.service=db | head -n 1)
db=""
if [ -n "$cid" ]; then
  db=$(d exec "$cid" env 2>&1)
fi
expect_line "the Compose container has DB_PASS" "$db" "DB_PASS=postgres://web"

finish
