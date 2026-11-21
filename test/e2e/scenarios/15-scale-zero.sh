#!/bin/bash
# 15-scale-zero proves `scale --replicas 0` on the real Swarm. A scale to 0 stops the web role: no container of its service
# runs, ps shows 0 desired and 0 running, wait returns at once, and the release history is unchanged, since a scale is not a
# release. A negative count is refused before anything is sent and leaves the audit as it was. A deploy brings the role back
# to its manifest count, and a scale brings it back to N. On a protected environment an agent token's scale to 0 is held until
# a person approves it, and the service runs on until then.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

svc="$PROJECT-$ENVN-web-main"

# running_count: how many containers of the web role's service run now.
running_count() { d ps -q --filter label=com.docker.swarm.service.name="$svc" | wc -l; }

# 1. The web app is deployed at v1.
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1" 0 "$code" "$out"

# 2. A scale to 0 stops the role. The scale waits for the old containers to drain, so none runs once the command returns.
out=$(adm scale web --replicas 0 2>&1); code=$?
expect_code "scale to 0" 0 "$code" "$out"
spec=$(service_spec "$svc")
expect_contains "the service is scaled to 0" "$spec" '"Replicated":{"Replicas":0}'
expect_eq "no container runs right after the scale" "0" "$(running_count)"

# 3. ps shows the web app's main role at 0 desired and 0 running.
out=$(adm ps 2>&1); code=$?
expect_code "ps answers" 0 "$code" "$out"
expect_contains "ps shows the main role at 0 desired and 0 running" "$out" \
  $'ref: \n  kind: image\n  name: web\nroles: \n  desired: 0\n  name: main\n  replicas: \n  running: 0'

# 4. wait returns at once for a stopped role.
start=$SECONDS
out=$(adm wait web --timeout 30s 2>&1); code=$?
elapsed=$((SECONDS - start))
expect_code "wait for a stopped role exits 0" 0 "$code" "$out"
if [ "$elapsed" -lt 10 ]; then pass "wait for a stopped role returned in ${elapsed}s"; else fail "wait for a stopped role returned at once" "took ${elapsed}s"; fi

# 5. A scale is not a release: the history still has the one v1 release, and the audit records the successful scale.
out=$(adm history web 2>&1); code=$?
expect_code "history answers after the scale" 0 "$code" "$out"
expect_contains "history lists the v1 release" "$out" "$WEB_V1_DIGEST"
expect_eq "history has exactly one release row" "1" "$(printf '%s\n' "$out" | grep -c '^tag: ')"

out=$(adm audit 2>&1); code=$?
expect_code "audit answers after the scale" 0 "$code" "$out"
expect_eq "audit has one succeeded scale row" "1" \
  "$(printf '%s\n' "$out" | awk 'BEGIN { RS = "" } /action: scale/ && /outcome: succeeded/ { n++ } END { print n + 0 }')"

# 6. A negative count is refused before anything is sent, and the audit is the same before and after.
before=$(adm audit 2>&1)
out=$(adm scale web --replicas -1 2>&1); code=$?
expect_code "a negative replica count exits 2" 2 "$code" "$out"
expect_contains "the refusal says the count must be 0 or more" "$out" "must be 0 or more"
expect_eq "the refused scale leaves the audit unchanged" "$before" "$(adm audit 2>&1)"

# 7. A deploy brings the role back to the manifest's count of 1, and exactly one container runs.
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "a deploy after the scale to 0 succeeds" 0 "$code" "$out"
spec=$(service_spec "$svc")
expect_contains "the deploy sets the role back to 1 replica" "$spec" '"Replicated":{"Replicas":1}'
expect_eq "exactly one container runs after the deploy" "1" "$(running_count)"

# 8. A scale to 0 stops the role again, and a scale to 2 runs two containers.
out=$(adm scale web --replicas 0 2>&1); code=$?
expect_code "scale to 0 again" 0 "$code" "$out"
expect_eq "no container runs after the second scale to 0" "0" "$(running_count)"
out=$(adm scale web --replicas 2 2>&1); code=$?
expect_code "scale to 2" 0 "$code" "$out"
spec=$(service_spec "$svc")
expect_contains "the service is scaled to 2" "$spec" '"Replicated":{"Replicas":2}'
expect_eq "exactly two containers run after the scale to 2" "2" "$(running_count)"

# 9. On a protected environment an agent token's scale to 0 is held until a person approves it. The protected deploy
# first brings the role to its manifest count of 1, so the hold is checked against a running service.
{ echo "protected: true"; base_manifest; } | write_manifest
out=$(adm deploy web --version "$WEB_V1_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "protect the environment with a digest deploy" 0 "$code" "$out"
spec=$(service_spec "$svc")
expect_contains "the protected deploy sets the role to 1 replica" "$spec" '"Replicated":{"Replicas":1}'
expect_eq "one container runs before the held scale" "1" "$(running_count)"

out=$(ci scale web --replicas 0 2>&1); code=$?
expect_code "an agent's scale to 0 is held for approval" 3 "$code" "$out"
expect_contains "the hold names an approval" "$out" "held for approval: "
id=$(printf '%s\n' "$out" | sed -n 's/^held for approval: \([^ ]*\) .*/\1/p')
spec=$(service_spec "$svc")
expect_contains "the held scale leaves the role at 1 replica" "$spec" '"Replicated":{"Replicas":1}'
expect_eq "the held scale leaves one container running" "1" "$(running_count)"

out=$(adm approvals 2>&1); code=$?
expect_code "approvals lists" 0 "$code" "$out"
expect_contains "the pending scale is listed by its summary" "$out" "scale web to 0"

out=$(adm approve "$id" 2>&1); code=$?
expect_code "approve runs the held scale" 0 "$code" "$out"
spec=$(service_spec "$svc")
expect_contains "the approved scale sets the role to 0" "$spec" '"Replicated":{"Replicas":0}'
expect_eq "the approved scale leaves no container running" "0" "$(running_count)"

finish
