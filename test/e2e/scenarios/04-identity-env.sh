#!/bin/bash
# 04-identity-env proves the BERTHY_* variables on the real Swarm. A container of an image app carries its environment,
# project, app, role, tag and digest, so the app can tag what it reports. A digest deploy has an empty tag, a rollback gives
# the tag and digest of the release it returns to, and a deploy that keeps the version gives the service the same environment
# twice. A pre_deploy run hook's container has the app's identity and an empty role. The manifest refuses a BERTHY_ name. A
# Compose app gets the environment, project and app in its .env file, which its Compose file passes on.
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

# expect_identity NAME VERSION DIGEST: the environment of a running web container carries the identity of a release. VERSION
# is the tag, or "" for a digest deploy, so its line must be exactly BERTHY_VERSION=.
expect_identity() {
  local out code
  out=$(adm exec web -- env 2>&1); code=$?
  expect_code "$1: exec reads the environment" 0 "$code" "$out"
  expect_line "$1: BERTHY_ENVIRONMENT" "$out" "BERTHY_ENVIRONMENT=$ENVN"
  expect_line "$1: BERTHY_PROJECT" "$out" "BERTHY_PROJECT=$PROJECT"
  expect_line "$1: BERTHY_APP" "$out" "BERTHY_APP=web"
  expect_line "$1: BERTHY_ROLE" "$out" "BERTHY_ROLE=main"
  expect_line "$1: BERTHY_VERSION" "$out" "BERTHY_VERSION=$2"
  expect_line "$1: BERTHY_IMAGE_DIGEST" "$out" "BERTHY_IMAGE_DIGEST=$3"
}

# env_json: the Env list of the Swarm service $PROJECT-$ENVN-web-main, as JSON.
env_json() { service_spec "$PROJECT-$ENVN-web-main" | grep -o '"Env":\[[^]]*\]'; }

# hook_manifest: the base manifest with a pre_deploy run hook that prints the Berthy variables of its container.
hook_manifest() {
  base_manifest
  cat <<'YML'
    hooks:
      pre_deploy:
        - run: ["sh", "-c", "env | grep ^BERTHY_"]
YML
}

# A tag deploy: the containers carry the tag and the digest that runs.
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1" 0 "$code" "$out"
expect_identity "tag deploy" v1 "$WEB_V1_DIGEST"

# A digest deploy: no tag is asked for, so BERTHY_VERSION is empty and the digest is the one deployed.
out=$(adm deploy web --version "$WEB_V2_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v2 by digest" 0 "$code" "$out"
expect_identity "digest deploy" "" "$WEB_V2_DIGEST"

# A rollback returns to v1, so the tag and the digest of v1 come back with it.
out=$(adm rollback web 2>&1); code=$?
expect_code "rollback" 0 "$code" "$out"
expect_identity "rollback" v1 "$WEB_V1_DIGEST"

# A deploy that keeps the version, twice: the service's environment is the same both times.
out=$(adm deploy --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy keeping the version" 0 "$code" "$out"
first=$(env_json)
expect_contains "the kept version has its tag in the service environment" "$first" '"BERTHY_VERSION=v1"'
out=$(adm deploy --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy keeping the version again" 0 "$code" "$out"
expect_eq "a deploy that keeps the version gives the same environment" "$first" "$(env_json)"

# A pre_deploy run hook: its one-off container belongs to the app, so BERTHY_ROLE is set and empty.
hook_manifest | write_manifest
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy with a pre_deploy run hook" 0 "$code" "$out"
hook_id=$(d ps -a -q --filter label=berthy.oneoff=pre_hook | head -n 1)
hook_logs=""
if [ -n "$hook_id" ]; then
  hook_logs=$(d logs "$hook_id" 2>&1)
fi
expect_line "the hook container has BERTHY_APP" "$hook_logs" "BERTHY_APP=web"
expect_line "the hook container has an empty BERTHY_ROLE" "$hook_logs" "BERTHY_ROLE="
expect_line "the hook container has BERTHY_VERSION" "$hook_logs" "BERTHY_VERSION=v1"

# The manifest refuses a BERTHY_ name in env.plain.
reserved=$RUN/reserved-manifest
mkdir -p "$reserved"
cat > "$reserved/berthy.yml" <<YML
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
      plain:
        BERTHY_X: "1"
YML
out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$reserved" 2>&1); code=$?
if [ "$code" -ne 0 ]; then
  pass "config validate exits non-zero on a BERTHY_ name (exit $code)"
else
  fail "config validate exits non-zero on a BERTHY_ name" "exit 0"
fi
expect_contains "config validate names the reserved prefix" "$out" "names starting with BERTHY_ are reserved for Berthy"

# A Compose app: its .env file names the environment, project and app, and the Compose file passes the app on.
cat > "$MAN/compose.yml" <<YML
services:
  who:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sleep", "3600"]
    environment: ["WHO=\${BERTHY_APP}"]
YML
write_manifest <<YML
project: $PROJECT
network: $NETWORK
apps:
  shop:
    kind: compose
    file: compose.yml
YML
out=$(adm deploy shop --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy the Compose app" 0 "$code" "$out"
env_file=$(cat "$RUN/state/compose/$PROJECT-$ENVN-shop/.env" 2>&1)
expect_line "the Compose .env names the environment" "$env_file" "BERTHY_ENVIRONMENT='$ENVN'"
expect_line "the Compose .env names the project" "$env_file" "BERTHY_PROJECT='$PROJECT'"
expect_line "the Compose .env names the app" "$env_file" "BERTHY_APP='shop'"
if grep -q '^BERTHY_\(ROLE\|VERSION\|IMAGE_DIGEST\)=' <<<"$env_file"; then
  fail "the Compose .env has no role, version or digest" "$env_file"
else
  pass "the Compose .env has no role, version or digest"
fi
cid=$(d ps -q --filter status=running --filter label=com.docker.compose.project=$PROJECT-$ENVN-shop --filter label=com.docker.compose.service=who | head -n 1)
who=""
if [ -n "$cid" ]; then
  who=$(d exec "$cid" env 2>&1)
fi
expect_line "the Compose service passes the app on: WHO" "$who" "WHO=shop"

finish
