#!/bin/bash
# 35-compose-one-shot proves that a Compose deploy judges readiness itself. A project with a long-running service web and a
# standalone one-shot job that exits with code 0 deploys: the job is a finished one-shot and does not fail the deploy, as it
# would under compose up --wait. A one-shot that exits with code 3 fails the deploy and the message names the service and
# the code. A service whose health check always fails fails it too, as unhealthy or, if Docker has not judged it in time, as
# not ready. A migrate one-shot that web depends on with service_completed_successfully deploys, because Compose waits for it.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

proj="$PROJECT-db"

# The manifest has the Compose app db, with a short deploy_timeout so a service that never becomes ready fails quickly.
write_manifest <<YML
project: $PROJECT
network: $NETWORK
apps:
  db:
    kind: compose
    file: compose.yml
    deploy:
      deploy_timeout: 15s
YML

# write_compose JOB_ENTRYPOINT WEB_TEST: a long-running web with a health check and a standalone job.
write_compose() {
  cat > "$MAN/compose.yml" <<YML
services:
  web:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sleep", "3600"]
    healthcheck:
      test: $2
      interval: 1s
      timeout: 2s
      retries: 1
  job:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: $1
YML
}

# container_state SERVICE: the state and exit code of the container of the service in the project, such as "exited 0".
container_state() {
  local id
  id=$(d ps -aq --filter label=com.docker.compose.project="$proj" --filter label=com.docker.compose.service="$1" | head -n 1)
  d inspect --format '{{.State.Status}} {{.State.ExitCode}}' "$id" 2>/dev/null
}

# a. A one-shot that exits with code 0 beside a healthy service.
write_compose '["true"]' '["CMD", "true"]'
out=$(adm deploy db --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy a project with a finished one-shot" 0 "$code" "$out"
expect_eq "the one-shot finished with code 0" "exited 0" "$(container_state job)"
expect_eq "the long-running service runs" "running 0" "$(container_state web)"

# b. The one-shot now fails with code 3.
write_compose '["sh", "-c", "exit 3"]' '["CMD", "true"]'
out=$(adm deploy db --manifest-dir "$MAN" 2>&1); code=$?
expect_code "a one-shot that exits with code 3 fails the deploy" 1 "$code" "$out"
expect_contains "the failure names the service and its exit code" "$out" "service job exited with code 3"

# c. A service whose health check always fails.
write_compose '["true"]' '["CMD", "false"]'
out=$(adm deploy db --manifest-dir "$MAN" 2>&1); code=$?
expect_code "a service that never becomes healthy fails the deploy" 1 "$code" "$out"
if [[ $out == *"service web is unhealthy"* || $out == *"service web not ready after 15s"* ]]; then
  pass "the failure names the service and says unhealthy or not ready"
else
  fail "the failure names the service and says unhealthy or not ready" "$out"
fi

# d. A migrate one-shot that web depends on: Compose waits for it during up, and it is a finished one-shot afterwards.
# The 15s deploy_timeout is for step c to fail fast. This deploy must wait for migrate (sleep 2s) and web's healthcheck.
write_manifest <<YML
project: $PROJECT
network: $NETWORK
apps:
  db:
    kind: compose
    file: compose.yml
    deploy:
      deploy_timeout: 120s
YML
cat > "$MAN/compose.yml" <<YML
services:
  migrate:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sh", "-c", "sleep 2"]
  web:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sleep", "3600"]
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 2s
      retries: 1
    depends_on:
      migrate:
        condition: service_completed_successfully
YML
out=$(adm deploy db --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy a service that depends on a one-shot completing" 0 "$code" "$out"
expect_eq "the migrate one-shot finished with code 0" "exited 0" "$(container_state migrate)"
expect_eq "web runs after it" "running 0" "$(container_state web)"

finish
