#!/bin/bash
# 16-spec-from-release proves that a role's Swarm spec is a function of the release on the real Swarm. A deploy over a
# service sends the spec a create would send, so a setting someone changed by hand (`docker service update --user`,
# `--hostname`) is gone after the next deploy, and a deploy again succeeds. The role keys `user` and `entrypoint` put a
# setting in the release, and taking the key out takes it out of the service. An adoption plan lists what the first deploy
# drops from a service another tool made: its user, hostname, placement and environment variables by name, never by value.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

svc="$PROJECT-$ENVN-web-main"

# 1. The web app is deployed at v1.
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1" 0 "$code" "$out"
spec=$(service_spec "$svc")
expect_not_contains "a service made by Berthy has no user" "$spec" '"User"'

# 2. Someone changes the running service by hand. The task it starts may not become healthy, so the update does not wait.
out=$(d service update --detach --user 0:0 --hostname handmade "$svc" 2>&1); code=$?
expect_code "docker service update by hand" 0 "$code" "$out"
spec=$(service_spec "$svc")
expect_contains "the hand-made user is in the live spec" "$spec" '"User":"0:0"'
expect_contains "the hand-made hostname is in the live spec" "$spec" '"Hostname":"handmade"'

# 3. A deploy sends the release's spec: both are gone.
out=$(adm deploy web --version v2 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v2 over the hand-made changes" 0 "$code" "$out"
spec=$(service_spec "$svc")
expect_contains "the service runs the v2 digest" "$spec" "$WEB_V2_DIGEST"
expect_not_contains "the user changed by hand is gone" "$spec" '"User"'
expect_not_contains "the hostname changed by hand is gone" "$spec" '"Hostname"'

# 4. A deploy again succeeds: the spec of the update verifies against what Docker stores.
out=$(adm deploy web --version v2 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v2 again" 0 "$code" "$out"

# 5. The role key `user` is part of the release, and the container runs as that user.
{ base_manifest; } | sed 's/^      main:$/      main:\n        user: "0:0"/' | write_manifest
out=$(adm deploy web --version v2 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy with a user" 0 "$code" "$out"
spec=$(service_spec "$svc")
expect_contains "the manifest's user is in the service" "$spec" '"User":"0:0"'
out=$(adm exec web -- id -u 2>&1); code=$?
expect_code "exec in the container" 0 "$code" "$out"
expect_contains "the container runs as the manifest's user" "$out" "0"

# 6. Taking the key out of the manifest takes the user out of the service.
base_manifest | write_manifest
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy without the user" 0 "$code" "$out"
spec=$(service_spec "$svc")
expect_not_contains "the user is gone with the key" "$spec" '"User"'

# 7. The plan of an adoption lists what the first deploy drops from a service another tool made.
# It carries no Berthy label, so the fixture does not remove it: the scenario removes it before it starts and when it ends.
legacy="$PROJECT-legacy"
d service rm "$legacy" >/dev/null 2>&1
trap 'd service rm "$legacy" >/dev/null 2>&1' EXIT
out=$(d service create --detach --name "$legacy" --network "$NETWORK" --user 0:0 --hostname handmade \
  --env LEGACY_FLAG=very-secret-value --constraint node.role==manager \
  --health-cmd true --health-interval 2s --health-timeout 2s --health-retries 3 \
  "$REG/e2e/web@$WEB_V1_DIGEST" 2>&1); code=$?
expect_code "docker service create by hand" 0 "$code" "$out"
for _ in $(seq 1 60); do
  [ "$(d ps -q --filter label=com.docker.swarm.service.name="$legacy" --filter health=healthy | wc -l)" -ge 1 ] && break
  sleep 1
done
{
  base_manifest
  cat <<YML
  legacy:
    kind: image
    image: $REG/e2e/web
    roles:
      main:
        service: $legacy
        health:
          test: ["CMD", "true"]
          interval: 2s
          timeout: 2s
          retries: 3
YML
} | write_manifest
out=$(adm deploy legacy --adopt --dry-run --version "$WEB_V1_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "adoption dry run" 0 "$code" "$out"
expect_contains "the plan names the user" "$out" 'role main: user "0:0" is not in the manifest; the first deploy drops it (set roles.main.user to keep it)'
expect_contains "the plan names the hostname" "$out" 'role main: hostname "handmade" is not in the manifest; the first deploy drops it'
expect_contains "the plan names the placement" "$out" 'role main: placement '
expect_contains "the plan names the variable" "$out" 'role main: env LEGACY_FLAG is not in the manifest; the first deploy drops it'
expect_not_contains "the plan never shows an environment value" "$out" "very-secret-value"

# 8. The adoption changes nothing; the first deploy drops what the plan listed, and keeps the service healthy.
out=$(adm deploy legacy --adopt --version "$WEB_V1_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "adopt the service" 0 "$code" "$out"
lspec=$(service_spec "$legacy")
expect_contains "adoption leaves the user in place" "$lspec" '"User":"0:0"'
out=$(adm deploy legacy --version v2 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "the first deploy of the adopted service" 0 "$code" "$out"
lspec=$(service_spec "$legacy")
expect_contains "the first deploy runs the v2 digest" "$lspec" "$WEB_V2_DIGEST"
expect_not_contains "the first deploy drops the user" "$lspec" '"User"'
expect_not_contains "the first deploy drops the hostname" "$lspec" '"Hostname"'
expect_not_contains "the first deploy drops the placement" "$lspec" '"Constraints"'
expect_not_contains "the first deploy drops the variable" "$lspec" 'LEGACY_FLAG'

finish
