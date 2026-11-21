#!/bin/bash
# 30-drift proves the drift check on the real Swarm and Compose. A deployed image app matches its release. A service changed by
# hand with docker (replicas, a variable) shows as a warning that names the field and never a value, in doctor, in describe and,
# once the agent runs its checks, in berthy checks. A deploy clears it. A role scaled with berthy scale is reported as scaled,
# not as drift. A Compose project recreated by hand with another environment shows the service as one a deploy would recreate,
# and a deploy clears that too. A Compose app is described under its project name.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

svc="$PROJECT-$ENVN-web-main"
project="$PROJECT-$ENVN-db"

cat > "$MAN/compose.yml" <<YML
services:
  side:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sleep", "3600"]
    environment:
      MODE: one
YML
{
  base_manifest
  cat <<YML
  db:
    kind: compose
    file: compose.yml
YML
} | write_manifest

# drift_line APP: the doctor's drift:APP entry, from its name line to the next blank line.
drift_entry() { adm doctor 2>&1 | awk -v n="name: drift:$1" 'BEGIN { RS = "" } index($0, n) { print }'; }

out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy web v1" 0 "$code" "$out"
out=$(adm deploy db --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy the Compose app" 0 "$code" "$out"

# 1. A fresh deploy matches its release, for both kinds of app.
out=$(drift_entry web)
expect_contains "web matches its release" "$out" "matches release"
out=$(drift_entry db)
expect_contains "db matches its release" "$out" "matches release"
out=$(adm describe web 2>&1)
expect_not_contains "describe shows no drift for a fresh deploy" "$out" "drift"

# 2. Changing the replicas and a variable with docker is drift. The lines name the fields, never a value.
d service update --detach --replicas 2 --env-add DRIFT_X=hand-secret-value "$svc" >/dev/null 2>&1
out=$(drift_entry web)
expect_contains "doctor warns about the replica count" "$out" "web/main: replicas 2, release says 1"
expect_contains "doctor names the variable" "$out" "web/main: env DRIFT_X differs"
expect_not_contains "doctor never shows the value" "$out" "hand-secret-value"
expect_contains "the warning is not ok" "$out" "ok: false"
expect_contains "the warning is not critical" "$out" "critical: false"
adm doctor >/dev/null 2>&1; code=$?
expect_code "a drift warning does not fail doctor" 0 "$code"
out=$(adm describe web 2>&1)
expect_contains "describe lists the replica line" "$out" "web/main: replicas 2, release says 1"
expect_contains "describe lists the variable line" "$out" "web/main: env DRIFT_X differs"
expect_not_contains "describe never shows the value" "$out" "hand-secret-value"

# 3. The monitor's check shows the same warning. A new agent runs every check at once, so the test does not wait five minutes.
stop_agent
start_agent >/dev/null 2>&1; code=$?
expect_code "the agent restarts" 0 "$code"
found=1
for _ in $(seq 1 90); do
  out=$(adm checks 2>&1)
  if printf '%s\n' "$out" | grep -E '^drift:web +warning' | grep -q 'replicas 2, release says 1'; then found=0; break; fi
  sleep 1
done
expect_code "berthy checks shows drift:web as a warning" 0 "$found" "$out"
expect_contains "the check names the variable" "$out" "env DRIFT_X differs"
expect_not_contains "the check never shows the value" "$out" "hand-secret-value"

# 4. A deploy clears it.
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy web again" 0 "$code" "$out"
out=$(drift_entry web)
expect_contains "the deploy clears the drift" "$out" "matches release"
out=$(adm describe web 2>&1)
expect_not_contains "describe shows no drift after the deploy" "$out" "release says"

# 5. A role scaled with berthy scale is scaled, not drifted.
out=$(adm scale web --replicas 2 2>&1); code=$?
expect_code "scale web to 2" 0 "$code" "$out"
out=$(drift_entry web)
expect_contains "doctor says the role is scaled" "$out" "web/main: scaled to 2 with berthy scale"
expect_contains "the scaled app is ok" "$out" "ok: true"
expect_not_contains "a scale is not drift" "$out" "release says"
out=$(adm describe web 2>&1)
expect_contains "describe says the role is scaled" "$out" "web/main: scaled to 2 with berthy scale"
expect_not_contains "describe shows no drift for a scaled role" "$out" "release says"

# 6. A Compose project recreated by hand with another environment is drift: a deploy would recreate the service.
hand=$(mktemp -d)
cp "$RUN/state/compose/$project/compose.yaml" "$hand/compose.yaml"
sed -i 's/MODE: one/MODE: hand/' "$hand/compose.yaml"
DOCKER_HOST="unix://$SOCK" DOCKER_CONFIG=$E2E_ROOT/docker-config "$COMPOSE_BIN" -p "$project" -f "$hand/compose.yaml" up -d >/dev/null 2>&1; code=$?
rm -rf "$hand"
expect_code "recreate the Compose service by hand" 0 "$code"
out=$(drift_entry db)
expect_contains "doctor says the Compose service would be recreated" "$out" "db: service side would be recreated"
out=$(adm describe "$project" 2>&1)
expect_contains "describe says the Compose service would be recreated" "$out" "db: service side would be recreated"

out=$(adm deploy db --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy the Compose app again" 0 "$code" "$out"
out=$(drift_entry db)
expect_contains "the deploy clears the Compose drift" "$out" "matches release"

finish
