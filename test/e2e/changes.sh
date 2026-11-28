#!/bin/bash
# Classify repository-relative paths read from standard input.
set -euo pipefail
go=false e2e=false harness=false actions=false
while IFS= read -r path; do
  case "$path" in
    '') continue ;;
    .github/workflows/*) go=true e2e=true harness=true actions=true ;;
    .github/*) actions=true ;;
    docs/user-guide.md|docs/spec-v1.md) go=true ;;
    test/*/testdata/*) go=true e2e=true harness=true ;;
    cmd/*|internal/*|third_party/*|go.mod|go.sum|testdata/*|*/testdata/*|examples/todo/*)
      go=true e2e=true ;;
    deploy/systemd/berthy.service) go=true e2e=true ;;
    deploy/*) e2e=true ;;
    test/e2e/*|test/fake*/*|test/tuidriver/*|test/mcpdriver/*)
      e2e=true harness=true
      [[ $path != *.go ]] || go=true ;;
    test/*) harness=true; [[ $path != *.go ]] || go=true ;;
    *.md|*.MD) ;;
    *) go=true e2e=true harness=true actions=true ;;
  esac
done
printf 'go=%s e2e=%s harness=%s actions=%s\n' "$go" "$e2e" "$harness" "$actions"
