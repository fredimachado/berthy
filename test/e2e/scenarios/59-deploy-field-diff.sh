#!/bin/bash
# Exercise a field diff across image, kept-image, and Compose deploys on the Swarm.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

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
          interval: 1s
          timeout: 2s
          retries: 1
  worker:
    kind: image
    image: $REG/e2e/web
    depends_on: [web]
    deploy:
      start_attempts: 1
      deploy_timeout: 30s
    roles:
      main:
        health:
          test: ["CMD", "$1"]
          interval: 1s
          timeout: 2s
          retries: 1
  zstack:
    kind: compose
    file: project-compose.yml
    depends_on: [worker]
YML
}

compose_file() {
  cat > "$MAN/project-compose.yml" <<YML
services:
  database:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sleep", "3600"]
    environment:
      MODE: "$1"
YML
}

field_apps() { sed -n "s#^Deploy $PROJECT/$ENVN app ##p"; }
applied() {
  printf 'Authorization: Bearer %s\n' "$BERTHY_ADMIN_TOKEN" |
    curl_local --fail --header @- "http://$AGENT_LISTEN/v1/projects/$PROJECT/environments/$ENVN/applied/$1" |
    jq -er .digest
}

manifest true | write_manifest
compose_file one
args=(--manifest-dir "$MAN" --version "web=$WEB_V1_DIGEST" --version "worker=$WEB_V1_DIGEST")
out=$(adm deploy "${args[@]}" --dry-run --diff 2>&1); code=$?
expect_code "preview the first project deploy" 0 "$code" "$out"
expect_eq "first project preview follows dependencies" $'web\nworker\nzstack' "$(field_apps <<<"$out")"
out=$(adm deploy "${args[@]}" --diff 2>&1); code=$?
expect_code "deploy the project for the first time" 0 "$code" "$out"
expect_eq "first project deploy prints each field block" $'web\nworker\nzstack' "$(field_apps <<<"$out")"

compose_file two
args=(--manifest-dir "$MAN" --version "web=$WEB_V2_DIGEST")
out=$(adm deploy "${args[@]}" --dry-run --diff 2>&1); code=$?
expect_code "preview an image change, kept image and Compose recreation" 0 "$code" "$out"
expect_eq "project preview includes the kept app in order" $'web\nworker\nzstack' "$(field_apps <<<"$out")"
expect_contains "project preview names the Compose recreation" "$out" "recreate database"
expect_eq "preview keeps the old web digest applied" "$REG/e2e/web@$WEB_V1_DIGEST" "$(applied web)"
out=$(adm deploy "${args[@]}" --diff 2>&1); code=$?
expect_code "deploy the image change, kept image and Compose recreation" 0 "$code" "$out"
expect_eq "project deploy prints field blocks in order" $'web\nworker\nzstack' "$(field_apps <<<"$out")"
expect_contains "project deploy names the Compose recreation" "$out" "recreate database"
expect_eq "web applies the new digest" "$REG/e2e/web@$WEB_V2_DIGEST" "$(applied web)"
expect_eq "worker keeps its digest" "$REG/e2e/web@$WEB_V1_DIGEST" "$(applied worker)"

manifest false | write_manifest
compose_file three
out=$(adm deploy --manifest-dir "$MAN" --version "worker=$WEB_V2_DIGEST" --diff 2>&1); code=$?
expect_code "the middle-app rollout fails the project deploy" 1 "$code" "$out"
expect_contains "the failed candidate rolls back" "$out" "rolled_back"
expect_contains "project deploy names the dependent that did not run" "$out" "deploy stopped at worker; not run: zstack"
expect_eq "the skipped app emits no field block" $'web\nworker' "$(field_apps <<<"$out")"
expect_eq "the failed deployment retains the previous applied digest" "$REG/e2e/web@$WEB_V1_DIGEST" "$(applied worker)"
expect_contains "the Swarm service runs the prior digest" "$(service_spec "$PROJECT-$ENVN-worker-main")" "$WEB_V1_DIGEST"

finish
