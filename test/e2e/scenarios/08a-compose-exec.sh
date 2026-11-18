#!/bin/bash
# 08a-compose-exec proves exec on a Compose app. --service picks the running container of a service, a dry run names the
# project and service the command would run in and runs nothing, and a flag the app's kind does not take is refused. The
# database service runs postgres:17-alpine, which the fixture has, and the second service runs the e2e web image with a
# command that only waits, because the fixture has no internet access to pull other images.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

cat > "$MAN/compose.yml" <<YML
services:
  postgres:
    image: postgres:17-alpine
    environment:
      POSTGRES_PASSWORD: \${POSTGRES_PASSWORD}
  side:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sleep", "3600"]
YML

# The manifest has the image app web (from base_manifest) and the Compose app db. POSTGRES_PASSWORD reaches the project's
# .env through env.plain, and the Compose file reads it as ${POSTGRES_PASSWORD}.
{
  base_manifest
  cat <<YML
  db:
    kind: compose
    file: compose.yml
    env:
      plain:
        POSTGRES_PASSWORD: e2e-pass
YML
} | write_manifest

out=$(adm deploy db --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy the Compose app" 0 "$code" "$out"

ready=1
for _ in $(seq 1 60); do
  out=$(adm exec db --service postgres -- pg_isready -U postgres 2>&1); code=$?
  if [ "$code" -eq 0 ]; then
    ready=0
    break
  fi
  sleep 1
done
expect_code "postgres accepts connections" 0 "$ready" "$out"

out=$(adm exec db --service postgres -- psql -U postgres -tAc "select 40+2" 2>&1); code=$?
expect_code "exec runs psql in the postgres service" 0 "$code" "$out"
# The CLI prints the operation's events and record before the command's output, so the answer is one line of it.
answer=1
printf '%s\n' "$out" | grep -qx 42 && answer=0
expect_code "psql prints the answer on its own line" 0 "$answer" "$out"

out=$(adm exec db --service side -- true 2>&1); code=$?
expect_code "exec picks the other service by name" 0 "$code" "$out"

out=$(adm exec db -- true 2>&1); code=$?
expect_code "exec without --service is refused, the app has two services" 1 "$code" "$out"
expect_contains "the refusal names --service" "$out" "name one with --service"

out=$(adm exec db --dry-run -- true 2>&1); code=$?
expect_code "a dry run without --service is refused too" 1 "$code" "$out"
expect_contains "the dry-run refusal names --service" "$out" "name one with --service"

out=$(adm exec db --role x -- true 2>&1); code=$?
expect_code "exec with --role is refused for a Compose app" 1 "$code" "$out"
expect_contains "the refusal says Compose apps have no roles" "$out" "compose apps have no roles"

before=$(adm audit 2>&1)
out=$(adm exec db --service postgres --dry-run -- psql -c x 2>&1); code=$?
expect_code "exec dry run plans for a service" 0 "$code" "$out"
expect_contains "the plan names the project and service" "$out" "target: $PROJECT-db/postgres"
expect_contains "the plan names the command" "$out" "psql"
expect_eq "the dry run records nothing" "$before" "$(adm audit 2>&1)"

out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy the image app beside it" 0 "$code" "$out"

out=$(adm exec web --dry-run -- true 2>&1); code=$?
expect_code "exec dry run for an image app plans" 0 "$code" "$out"
expect_contains "the plan names the role's Swarm service" "$out" "target: $PROJECT-web-main"

out=$(adm exec web --service x -- true 2>&1); code=$?
expect_code "--service is refused for an image app" 1 "$code" "$out"
expect_contains "the refusal says to use --role" "$out" "use --role for an image app"

finish
