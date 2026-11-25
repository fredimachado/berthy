#!/bin/bash
# Deploy from a repository root and read the overlay and Compose file under .berthy.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

repo=$(mktemp -d "$MAN/repo.XXXXXX") || exit 1
cd "$repo" || exit 1
mkdir -p .berthy/compose || exit 1
cat > .berthy/berthy.yml <<YML
project: $PROJECT
apps:
  sample:
    kind: compose
    file: compose/sample.yml
YML
cat > ".berthy/berthy.$ENVN.yml" <<'YML'
apps:
  sample:
    env:
      plain:
        LOCATION: hidden
YML
cat > .berthy/compose/sample.yml <<YML
services:
  sample:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    environment:
      LOCATION: \${LOCATION}
YML

out=$(adm deploy sample 2>&1); code=$?
expect_code "deploy the nested Compose app from the root" 0 "$code" "$out"
out=$(adm exec sample --service sample -- printenv LOCATION 2>&1); code=$?
expect_code "read the nested Compose app's environment" 0 "$code" "$out"
expect_contains "the nested overlay reached the container" "$out" "hidden"
finish
