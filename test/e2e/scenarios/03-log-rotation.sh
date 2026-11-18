#!/bin/bash
# 03-log-rotation proves the default log rotation of image apps on the real Swarm. A role that sets no logging gets json-file
# with max-size 10m and max-file 3 (the fixture daemon's default driver is json-file). A role whose manifest sets logging
# gets those options instead. A service Berthy did not create shows the rotation its adoption would change in the plan, and
# the dry run changes nothing.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

# logging_manifest: the base manifest with a logging block on the role main, which the first deploy does not have.
logging_manifest() {
  base_manifest
  cat <<'YML'
        logging:
          driver: json-file
          options:
            max-size: 50m
            max-file: "5"
YML
}

# container_log_config SERVICE: the HostConfig.LogConfig of a running task container of the Swarm service.
container_log_config() {
  local id
  id=$(d ps -q --filter status=running --filter "label=com.docker.swarm.service.name=$1" | head -n 1)
  [ -n "$id" ] || return 1
  d inspect --format '{{json .HostConfig.LogConfig}}' "$id"
}

# adopt_healthy: true once the service $PROJECT-adopt has a healthy task.
adopt_healthy() {
  [ -n "$(d ps -q --filter status=running --filter health=healthy --filter label=com.docker.swarm.service.name=$PROJECT-adopt)" ]
}

# The role sets no logging, so the default rotation applies.
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1 with no logging" 0 "$code" "$out"
spec=$(service_spec "$PROJECT-web-main")
expect_contains "service has the default rotation" "$spec" '"LogDriver":{"Name":"json-file","Options":{"max-file":"3","max-size":"10m"}}'
log=$(container_log_config "$PROJECT-web-main" 2>&1); code=$?
expect_code "a running task container exists" 0 "$code" "$log"
expect_contains "container logs with the default rotation" "$log" '{"Type":"json-file","Config":{"max-file":"3","max-size":"10m"}}'

# The role's own logging replaces the default.
logging_manifest | write_manifest
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1 with the role's logging" 0 "$code" "$out"
spec=$(service_spec "$PROJECT-web-main")
expect_contains "service has the role's rotation" "$spec" '"LogDriver":{"Name":"json-file","Options":{"max-file":"5","max-size":"50m"}}'
expect_not_contains "the default rotation is gone" "$spec" '"max-size":"10m"'
log=$(container_log_config "$PROJECT-web-main" 2>&1); code=$?
expect_code "a running task container exists after the change" 0 "$code" "$log"
expect_contains "container logs with the role's rotation" "$log" '{"Type":"json-file","Config":{"max-file":"5","max-size":"50m"}}'

# Adoption: a service Berthy did not create has no log driver, so its plan names the rotation the next deploy will apply.
out=$(d service create --name "$PROJECT-adopt" --label berthy.environment=$ENVN --network "$NETWORK" --health-cmd true --health-interval 2s "$REG/e2e/web@$WEB_V1_DIGEST" 2>&1); code=$?
expect_code "create a service Berthy did not make" 0 "$code" "$out"
wait_for "the adopted service is healthy" adopt_healthy
expect_code "the adopted service has a healthy task" 0 "$?"

{
  logging_manifest
  cat <<YML
  adopted:
    kind: image
    image: $REG/e2e/web
    roles:
      main:
        service: $PROJECT-adopt
YML
} | write_manifest
out=$(adm deploy adopted --adopt --dry-run --version "$WEB_V1_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "adoption dry run" 0 "$code" "$out"
expect_contains "the plan names the rotation the next deploy applies" "$out" "max-size=10m"
expect_contains "the plan says the next deploy applies it" "$out" "applied by the next deploy"
expect_not_contains "the dry run leaves the service without a log driver" "$(service_spec "$PROJECT-adopt")" "LogDriver"

finish
