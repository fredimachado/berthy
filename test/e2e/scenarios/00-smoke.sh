#!/bin/bash
# 00-smoke proves the harness itself. It deploys the pinned v1 image, runs a command inside it, lists the history,
# deploys v2 and rolls back to v1. Every scenario follows this shape: source lib.sh, drive the real berthy binary through
# adm or ci, assert what the user would see, and end with finish.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1" 0 "$code" "$out"
spec=$(service_spec e2e-web-main)
expect_contains "service runs the pinned v1 digest" "$spec" "$WEB_V1_DIGEST"

out=$(adm exec web -- cat /etc/os-release 2>&1); code=$?
expect_code "exec runs in the container" 0 "$code" "$out"
expect_contains "exec output comes from the container" "$out" "Alpine"

out=$(adm history web 2>&1); code=$?
expect_code "history answers" 0 "$code" "$out"
expect_contains "history lists the v1 release" "$out" "$WEB_V1_DIGEST"

out=$(adm deploy web --version v2 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v2" 0 "$code" "$out"
spec=$(service_spec e2e-web-main)
expect_contains "service runs the pinned v2 digest" "$spec" "$WEB_V2_DIGEST"

out=$(adm rollback web 2>&1); code=$?
expect_code "rollback" 0 "$code" "$out"
spec=$(service_spec e2e-web-main)
expect_contains "rollback returns to v1" "$spec" "$WEB_V1_DIGEST"
expect_not_contains "rollback leaves v2 behind" "$spec" "$WEB_V2_DIGEST"

finish
