#!/bin/bash
# build.sh copies the repo into WSL's native filesystem and builds the binaries the harness runs: berthy, the fake Telegram,
# the fake OTLP receiver, the fake webhook, the fake S3, the terminal driver and the MCP driver. Each binary is built to a .new
# file and moved into place, so a failed build leaves nothing half-written and keeps the previous binary.
set -uo pipefail
. "$(dirname "$0")/lib.sh"

# build_to OUTPUT PACKAGE: builds PACKAGE to OUTPUT, or fails and removes the partial file.
build_to() {
  if "$GO" build -buildvcs=false -o "$1.new" "$2"; then
    mv -f "$1.new" "$1"
  else
    rm -f "$1.new"
    echo "build of $2 failed" >&2
    return 1
  fi
}

mkdir -p "$SRC" "$E2E_ROOT/bin" || exit 1
rsync -rt --delete --exclude .git --exclude .claude --exclude '*.exe' "$REPO/" "$SRC/" || {
  echo "copy from $REPO failed" >&2
  exit 1
}
# Go accepts CRLF, but gofmt does not, and the Windows checkout may hold CRLF. The copy is ours, so strip it here.
find "$SRC" -type f \( -name '*.go' -o -name go.mod -o -name go.sum \) -exec sed -i 's/\r$//' {} + || exit 1

cd "$SRC" || exit 1
# The documented build is static (CGO_ENABLED=0). The overlay helper is made from the agent's own binary and needs it static.
CGO_ENABLED=0 build_to "$BIN" ./cmd/berthy || exit 1
build_to "$TG_BIN" ./test/faketelegram || exit 1
build_to "$OTLP_BIN" ./test/fakeotlp || exit 1
build_to "$HOOK_BIN" ./test/fakewebhook || exit 1
build_to "$S3_BIN" ./test/fakes3 || exit 1
build_to "$TUI_BIN" ./test/tuidriver || exit 1
build_to "$MCP_BIN" ./test/mcpdriver || exit 1
build_to "$NAMES_BIN" ./test/e2e/fixture-names || exit 1
"$BIN" version
