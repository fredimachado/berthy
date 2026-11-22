#!/bin/bash
set -uo pipefail
test_root=$(mktemp -d) || exit 1
readonly test_root
trap 'rm -rf -- "$test_root"' EXIT
E2E_ROOT=$test_root
export E2E_ROOT
. "$(dirname "$0")/../lib.sh"
fixture='[{"Id":"running","ImageID":"sha256:one"},{"Id":"stopped","ImageID":"sha256:two"}]'
curl() { printf 'x' >> "$E2E_ROOT/calls"; printf '%s' "$fixture"; }
out=$(container_image_inventory); code=$?
[ "$code" -eq 0 ] && [ "$(jq -r '.[].ImageID' <<< "$out" | paste -sd, -)" = 'sha256:one,sha256:two' ] || { echo 'FAIL: inventory IDs'; exit 1; }
[ "$(cat "$E2E_ROOT/calls")" = x ] || { echo 'FAIL: more than one inventory request'; exit 1; }
fixture='[{"Id":"incomplete"}]'
if container_image_inventory >/dev/null 2>&1; then echo 'FAIL: accepted incomplete inventory'; exit 1; fi
curl() { return 1; }
if container_image_inventory >/dev/null 2>&1; then echo 'FAIL: ignored inventory error'; exit 1; fi
echo 'PASS container inventory'
