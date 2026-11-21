#!/bin/bash
# 18-env-settings proves that the environment's settings live on their own and no app's release changes them.
#
# `berthy apply` puts a probe into the running monitor without a deploy: no release is recorded, the audit says apply, and a
# second apply prints no changes. A deploy applies the settings of its manifest too, and a rollback of the app brings back the
# app's own release but not the old settings: the second probe stays, the apply of the same manifest still has nothing to
# change, and the monitor keeps checking the probe. A manifest of another project is refused by apply and by deploy, dry run
# or not, and an apply is refused while the deploy lock is held.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

# settings_manifest [PROBE...]: the base manifest with an http probe of the fake Telegram's address for each PROBE name.
settings_manifest() {
  local project=${PROJECT_OVERRIDE:-$PROJECT} probe
  echo "project: $project"
  echo "network: $NETWORK"
  if [ $# -gt 0 ]; then
    echo "probes:"
    for probe in "$@"; do
      printf '  %s:\n    http: http://%s/_fake/messages\n    interval: 5s\n    timeout: 2s\n' "$probe" "$TG_LISTEN"
    done
  fi
  cat <<YML
apps:
  web:
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
YML
}

# has_check NAME: the monitor has a stored state for the check NAME.
has_check() { adm checks 2>&1 | grep -q "^$1 "; }

# wait_check NAME SECONDS: waits until the monitor lists the check, which it does after its next rebuild (every minute).
wait_check() {
  local i
  for i in $(seq 1 $(($2 / 3))); do
    has_check "$1" && return 0
    sleep 3
  done
  return 1
}

# release_rows APP: how many releases the history of APP lists.
release_rows() { adm history "$1" 2>&1 | grep -c '^tag: '; }

# 1. The web app is deployed with no probe.
settings_manifest | write_manifest
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1" 0 "$code" "$out"
expect_eq "one release after the deploy" "1" "$(release_rows web)"
if has_check probe:site; then fail "no probe before the apply" "probe:site is listed"; else pass "no probe before the apply"; fi

# 2. A dry run shows the probe and changes nothing. The apply puts it in, and records no release.
settings_manifest site | write_manifest
out=$(adm apply --dry-run --manifest-dir "$MAN" 2>&1); code=$?
expect_code "apply dry run" 0 "$code" "$out"
expect_contains "the dry run names the new probe" "$out" "probes: + site"
expect_contains "the dry run says it would change" "$out" "would change the environment settings"
out=$(adm apply --manifest-dir "$MAN" 2>&1); code=$?
expect_code "apply" 0 "$code" "$out"
expect_contains "apply names the new probe" "$out" "probes: + site"
expect_eq "apply recorded no release" "1" "$(release_rows web)"
out=$(adm apply --manifest-dir "$MAN" 2>&1); code=$?
expect_code "a repeat apply" 0 "$code" "$out"
expect_eq "a repeat apply has nothing to change" "no changes" "$out"
out=$(adm audit 2>&1)
expect_eq "the audit has two succeeded apply rows" "2" \
  "$(printf '%s\n' "$out" | awk 'BEGIN { RS = "" } /action: apply/ && /outcome: succeeded/ { n++ } END { print n + 0 }')"
if wait_check probe:site 120; then pass "the monitor runs the applied probe"; else fail "the monitor runs the applied probe" "$(adm checks 2>&1)"; fi

# 3. A deploy applies the settings of its manifest too: v2 comes with a second probe.
settings_manifest site site2 | write_manifest
out=$(adm deploy web --version v2 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v2 with a second probe" 0 "$code" "$out"
out=$(adm apply --dry-run --manifest-dir "$MAN" 2>&1)
expect_eq "the deploy applied the second probe" "no changes" "$out"
if wait_check probe:site2 120; then pass "the monitor runs the second probe"; else fail "the monitor runs the second probe" "$(adm checks 2>&1)"; fi

# 4. A rollback returns the app to v1 and leaves the settings alone.
out=$(adm rollback web 2>&1); code=$?
expect_code "rollback" 0 "$code" "$out"
spec=$(service_spec "$PROJECT-$ENVN-web-main")
expect_contains "rollback returns to v1" "$spec" "$WEB_V1_DIGEST"
out=$(adm apply --dry-run --manifest-dir "$MAN" 2>&1)
expect_eq "the rollback kept the second probe" "no changes" "$out"
# The monitor rebuilds its checks every minute, so a probe the rollback had dropped would be gone after this wait.
sleep 65
if has_check probe:site2; then pass "the monitor still runs the second probe after the rollback"; else fail "the monitor still runs the second probe after the rollback" "$(adm checks 2>&1)"; fi

# 5. A manifest of another project is refused before anything changes. The CLI takes the project from the manifest directory
# and its configuration has no such project, so it refuses before it contacts the agent; the agent's own comparison of the
# project is reached with --project, which comes with the CLI's project selection.
want="no environment $ENVN for project $PROJECT-other in the CLI configuration"
PROJECT_OVERRIDE=$PROJECT-other settings_manifest site site2 | write_manifest
before=$(adm audit 2>&1)
out=$(adm apply --manifest-dir "$MAN" 2>&1); code=$?
expect_code "apply of another project is refused" 1 "$code" "$out"
expect_contains "apply names the project and the configuration" "$out" "$want"
out=$(adm apply --dry-run --manifest-dir "$MAN" 2>&1); code=$?
expect_code "a dry run of another project is refused" 1 "$code" "$out"
expect_contains "the dry run names the project and the configuration" "$out" "$want"
out=$(adm deploy web --version v1 --dry-run --manifest-dir "$MAN" 2>&1); code=$?
expect_code "a deploy dry run of another project is refused" 1 "$code" "$out"
expect_contains "the deploy dry run names the project and the configuration" "$out" "$want"
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "a deploy of another project is refused" 1 "$code" "$out"
expect_contains "the deploy names the project and the configuration" "$out" "$want"
expect_eq "the refusals changed no audit row" "$before" "$(adm audit 2>&1)"
expect_eq "the refused deploy recorded no release" "3" "$(release_rows web)"

# 6. An apply is refused while the deploy lock is held, and goes through once it is released.
settings_manifest site | write_manifest
out=$(adm lock acquire -m "e2e apply" 2>&1); code=$?
expect_code "lock acquire" 0 "$code" "$out"
out=$(adm apply --manifest-dir "$MAN" 2>&1); code=$?
expect_code "apply under the lock is refused" 1 "$code" "$out"
expect_contains "the refusal says conflict" "$out" "conflict"
out=$(adm lock release 2>&1); code=$?
expect_code "lock release" 0 "$code" "$out"
out=$(adm apply --manifest-dir "$MAN" 2>&1); code=$?
expect_code "apply after the release" 0 "$code" "$out"
expect_contains "apply drops the second probe" "$out" "probes: - site2"

finish
