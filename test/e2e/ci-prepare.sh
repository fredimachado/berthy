#!/bin/bash
# Prepare a fresh GitHub-hosted runner's Docker daemon for the e2e harness.
set -euo pipefail
DIR=$(cd "$(dirname "$0")" && pwd)
: "${DOCKER_BIN:?set DOCKER_BIN}" "${SOCK:?set SOCK}"
[ "$#" -gt 0 ] || { echo 'Pass the scenarios assigned to this shard' >&2; exit 2; }
needs_fakeserver=false
for name in "$@"; do
  file="$DIR/scenarios/$name.sh"
  [ -f "$file" ] || { echo "Unknown scenario: $name" >&2; exit 2; }
  # shellcheck disable=SC2016 # Match the literal variable reference in the scenario.
  if grep -Eq 'fakeserver|FS_IMAGE|\$BIN"?[[:space:]]+setup([[:space:]]|$)' "$file"; then
    needs_fakeserver=true
  fi
done
export DOCKER_HOST="unix://$SOCK"
if [ "$("$DOCKER_BIN" info --format '{{.Swarm.LocalNodeState}}')" != inactive ]; then
  echo 'Refusing to initialize a daemon already in a Swarm' >&2
  exit 1
fi
for version in v1 v2; do
  image="127.0.0.1:15001/berthy-phase2/web:$version"
  if "$DOCKER_BIN" image inspect "$image" >/dev/null 2>&1; then
    echo "Refusing to replace existing image $image" >&2
    exit 1
  fi
done
"$DOCKER_BIN" swarm init
for version in v1 v2; do
  "$DOCKER_BIN" build --tag "127.0.0.1:15001/berthy-phase2/web:$version" - <<EOF
FROM nginx:alpine
LABEL berthy.test.version=$version
EOF
done
if "$needs_fakeserver"; then
  FS_DOCKER_HOST="$DOCKER_HOST" bash "$DIR/../fakeserver/fakeserver.sh" build
fi
