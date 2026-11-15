#!/usr/bin/env bash
set -euo pipefail
dir=$(mktemp -d)
cleanup() {
  trace=$(sed -n 's/^trace=//p' "$dir/output" 2>/dev/null || true)
  [[ $trace =~ ^/var/tmp/berthy-ingress-[0-9]+-[0-9]+$ ]] && rm -r -- "$trace"
  rm -r -- "$dir"
}
trap cleanup EXIT
cat > "$dir/docker" <<'STUB'
#!/usr/bin/env bash
if [[ ${1:-} == network && ${2:-} == create ]]; then
  [[ -z ${DOCKER_CONTEXT+x} && $DOCKER_HOST == unix:///tmp/isolated.sock ]] || exit 1
  echo 'dedicated socket selected' >&2
  exit 1
fi
exit 0
STUB
chmod +x "$dir/docker"
script=${1:-$(dirname "$0")/ingress.sh}
DOCKER_CONTEXT=nonexistent DOCKER_HOST=unix:///tmp/isolated.sock \
  DOCKER_BIN="$dir/docker" GO_BIN=/bin/true bash "$script" > "$dir/output" 2>&1 || true
grep -q '^dedicated socket selected$' "$dir/output"
echo 'Inherited context ignored'
