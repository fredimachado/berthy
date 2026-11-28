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
echo 'Change classification passed.'
