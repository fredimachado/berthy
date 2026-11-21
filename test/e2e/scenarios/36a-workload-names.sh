#!/bin/bash
# Two environments of one project deploy the same apps on one agent without sharing their workload names.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

OTHER=${ENVN}b
other_service="$PROJECT-$OTHER-web-main"
other_project="$PROJECT-$OTHER-db"
other_volume="$PROJECT-${OTHER}_data"
cleanup_other() {
  d service rm "$other_service" >/dev/null 2>&1 || true
  d ps -aq --filter "label=com.docker.compose.project=$other_project" | while read -r id; do d rm -f "$id" >/dev/null 2>&1 || true; done
  d volume rm "$other_volume" >/dev/null 2>&1 || true
}
trap cleanup_other EXIT

add_environment "$PROJECT" "$OTHER"; code=$?
expect_code "add a second environment" 0 "$code"

for env in "$ENVN" "$OTHER"; do
  dir=$(env_dir "$PROJECT" "$env")
  cat > "$dir/berthy.yml" <<YML
project: $PROJECT
network: $NETWORK
apps:
  web:
    kind: image
    image: $REG/e2e/web
    roles:
      main:
        volumes: ["data:/data"]
        health:
          test: ["CMD", "true"]
          interval: 2s
          timeout: 2s
          retries: 3
  db:
    kind: compose
    file: compose.yml
YML
  cat > "$dir/compose.yml" <<YML
services:
  db:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sleep", "3600"]
YML
  out=$(env_cmd "$PROJECT" "$env" deploy web --version v1 2>&1); code=$?
  expect_code "deploy image app in $env" 0 "$code" "$out"
  out=$(env_cmd "$PROJECT" "$env" deploy db 2>&1); code=$?
  expect_code "deploy Compose app in $env" 0 "$code" "$out"
done

for env in "$ENVN" "$OTHER"; do
  svc="$PROJECT-$env-web-main"
  vol="$PROJECT-${env}_data"
  project="$PROJECT-$env-db"
  spec=$(service_spec "$svc" 2>&1); code=$?
  expect_code "service $svc exists" 0 "$code" "$spec"
  expect_contains "service $svc mounts its environment volume" "$spec" "\"Source\":\"$vol\""
  out=$(d ps -q --filter "label=com.docker.compose.project=$project" --filter status=running); code=$?
  expect_code "inspect Compose project $project" 0 "$code" "$out"
  if [ -n "$out" ]; then pass "Compose project $project runs"; else fail "Compose project $project runs" "no running container"; fi
done

out=$(env_cmd "$PROJECT" "$OTHER" remove web --volumes 2>&1); code=$?
expect_code "remove the second environment's image app and volume" 0 "$code" "$out"
out=$(env_cmd "$PROJECT" "$OTHER" remove db --volumes 2>&1); code=$?
expect_code "remove the second environment's Compose app" 0 "$code" "$out"
spec=$(service_spec "$PROJECT-$ENVN-web-main" 2>&1); code=$?
expect_code "the first environment's service remains" 0 "$code" "$spec"
d volume inspect "$PROJECT-${ENVN}_data" >/dev/null 2>&1; code=$?
expect_code "the first environment's volume remains" 0 "$code"

finish
