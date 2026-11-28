#!/bin/bash
set -euo pipefail
DIR=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export CALLS=$tmp/calls DOCKER_BIN=$tmp/docker SOCK=$tmp/docker.sock
export PATH="$tmp:$PATH"
cat > "$DOCKER_BIN" <<'DOCKER'
#!/bin/bash
printf '%s\n' "$*" >> "$CALLS"
if [ "$1" = info ]; then echo inactive; fi
if [ "$1" = image ] && [[ $* != *berthy-fakeserver:debian12* ]]; then exit 1; fi
exit 0
DOCKER
chmod +x "$DOCKER_BIN"
bash "$DIR/ci-prepare.sh" 00-smoke >/dev/null
if grep -q 'berthy-fakeserver:debian12' "$CALLS"; then
  echo 'Built the fakeserver for a shard that does not use it' >&2
  exit 1
fi
: > "$CALLS"
bash "$DIR/ci-prepare.sh" 00-smoke 43-guided-setup >/dev/null
grep -q 'berthy-fakeserver:debian12' "$CALLS"
echo 'Fakeserver build selection passed.'
