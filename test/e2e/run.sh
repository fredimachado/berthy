#!/bin/bash
# run.sh is the entry point of the Berthy e2e harness.
#
#   run.sh [--no-build] [--keep] [scenario ...]
#
# It builds the binaries (unless --no-build). Then, for each scenario (default: every scenarios/*.sh in name order), it
# builds a fresh fixture with setup.sh and runs the scenario, so scenarios are independent. A name such as 00-smoke, or the
# file name 00-smoke.sh, selects scenarios/00-smoke.sh. Afterwards it stops the fixture with stop.sh unless --keep is
# given, prints a summary, and exits 1 when any scenario failed.
#
# The logs of each scenario are kept under $E2E_ROOT/runs/<scenario>, because setup clears $E2E_ROOT/logs for the next one.
set -uo pipefail
DIR=$(cd "$(dirname "$0")" && pwd)
. "$DIR/lib.sh"

usage() {
  echo "usage: run.sh [--no-build] [--keep] [scenario ...]" >&2
  exit 2
}

build=1
keep=0
names=()
for arg in "$@"; do
  case $arg in
    --no-build) build=0 ;;
    --keep) keep=1 ;;
    -h | --help) usage ;;
    -*) usage ;;
    *) names+=("$(basename "$arg" .sh)") ;;
  esac
done

if [ ${#names[@]} -eq 0 ]; then
  for file in "$DIR"/scenarios/*.sh; do
    [ -f "$file" ] && names+=("$(basename "$file" .sh)")
  done
fi
if [ ${#names[@]} -eq 0 ]; then
  echo "no scenarios in $DIR/scenarios" >&2
  exit 2
fi
for name in "${names[@]}"; do
  if [ ! -f "$DIR/scenarios/$name.sh" ]; then
    echo "no scenario named $name in $DIR/scenarios" >&2
    exit 2
  fi
done

for file in "$DIR"/scenarios/*.sh; do
  if grep -Eq '\$BIN"?[[:space:]]+setup([[:space:]]|$)' "$file" && ! grep -Fxq 'export HOME="$FS_HOME"' "$file"; then
    echo "setup scenario lacks isolated HOME: $file" >&2
    exit 2
  fi
done

if [ "$build" -eq 1 ]; then
  if ! bash "$DIR/build.sh"; then
    echo "build failed; no scenario ran" >&2
    exit 1
  fi
fi

results=()
failed=0
for name in "${names[@]}"; do
  echo "== $name"
  if bash "$DIR/setup.sh"; then
    bash "$DIR/scenarios/$name.sh"
    rc=$?
  else
    echo "  setup failed; the scenario did not run"
    rc=1
  fi
  rm -rf "$E2E_ROOT/runs/$name"
  mkdir -p "$E2E_ROOT/runs/$name"
  cp -a "$LOG/." "$E2E_ROOT/runs/$name/" 2>/dev/null
  if [ "$rc" -eq 0 ]; then
    results+=("PASS  $name")
  else
    results+=("FAIL  $name")
    failed=$((failed + 1))
  fi
  echo
done

if [ "$keep" -eq 1 ]; then
  echo "--keep: the fixture is still running; stop it with stop.sh"
else
  bash "$DIR/stop.sh" || echo "stop.sh reported a problem" >&2
fi

echo "summary"
for line in "${results[@]}"; do
  echo "  $line"
done

if [ "$failed" -eq 0 ]; then
  exit 0
fi
echo "$failed scenario(s) failed"
exit 1
