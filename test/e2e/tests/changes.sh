#!/bin/bash
set -euo pipefail
DIR=$(cd "$(dirname "$0")/.." && pwd)
check() {
  local expected=$1 input=$2 actual
  actual=$(printf '%s\n' "$input" | bash "$DIR/changes.sh")
  if [ "$actual" != "$expected" ]; then
    printf 'for %s: expected %s, got %s\n' "$input" "$expected" "$actual" >&2
    exit 1
  fi
}
check 'go=false e2e=false harness=false actions=false' README.md
check 'go=true e2e=false harness=false actions=false' docs/user-guide.md
check 'go=true e2e=true harness=false actions=false' internal/x.go
check 'go=false e2e=true harness=true actions=false' test/e2e/scenarios/x.sh
check 'go=true e2e=true harness=true actions=true' .github/workflows/ci.yml
check 'go=true e2e=true harness=true actions=true' unknown.txt
check 'go=true e2e=true harness=false actions=false' examples/todo/berthy.yml
check 'go=true e2e=true harness=false actions=false' deploy/systemd/berthy.service
check 'go=true e2e=true harness=true actions=false' test/example/testdata/sample.txt
check 'go=true e2e=false harness=true actions=false' $'README.md\ndocs/spec-v1.md\ntest/phase1/a.sh'

# Run the workflow's diff command on a real rename across classifier groups.
repo=$(mktemp -d)
trap 'rm -rf "$repo"' EXIT
git -C "$repo" init -q
git -C "$repo" config user.name 'CI test'
git -C "$repo" config user.email 'ci-test@example.invalid'
git -C "$repo" config diff.renames true
mkdir "$repo/docs"
printf 'guide content\n' > "$repo/docs/user-guide.md"
git -C "$repo" add .
git -C "$repo" -c core.hooksPath=/dev/null commit -qm base
base=$(git -C "$repo" rev-parse HEAD)
git -C "$repo" mv docs/user-guide.md docs/guide.md
git -C "$repo" -c core.hooksPath=/dev/null commit -qm rename
head=$(git -C "$repo" rev-parse HEAD)
# shellcheck disable=SC2016 # Match the literal variable reference in the workflow.
diff_command=$(tr -d '\r' < "$DIR/../../.github/workflows/ci.yml" | sed -n 's/^[[:space:]]*\(git diff .*\) > "\$RUNNER_TEMP\/changed-files"$/\1/p')
[ -n "$diff_command" ] || { echo 'Workflow diff command not found' >&2; exit 1; }
paths=$(cd "$repo" && BASE=$base HEAD=$head bash -c "$diff_command")
check 'go=true e2e=false harness=false actions=false' "$paths"
echo 'Change classification passed.'
