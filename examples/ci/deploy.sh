#!/bin/sh
# Run after checking out the commit and publishing its image.
set -eu
: "${BERTHY_ENV:?set the target environment}"
: "${BERTHY_CONFIG:?set the path to the CLI configuration}"
: "${IMAGE_DIGEST:?set the pushed image sha256 digest}"
: "${BERTHY_TOKEN:?set the scoped deploy token in the runner secret store}"
BERTHY_BIN=${BERTHY_BIN:-berthy}
APP=${APP:-api}
if ! printf '%s\n' "$IMAGE_DIGEST" | grep -Eq '^sha256:[0-9a-f]{64}$'; then
  printf '%s\n' 'IMAGE_DIGEST must be a sha256 digest' >&2
  exit 2
fi
commit=$(git rev-parse --verify HEAD)
"$BERTHY_BIN" config validate -e "$BERTHY_ENV" --manifest-dir "$PWD"
"$BERTHY_BIN" deploy "$APP" -e "$BERTHY_ENV" --config "$BERTHY_CONFIG" \
  --manifest-dir "$PWD" --version "$IMAGE_DIGEST" --commit "$commit"
