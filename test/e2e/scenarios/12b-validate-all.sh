#!/bin/bash
# 12b-validate-all proves that config validate reports every problem of a manifest in one run, each with the file and the
# line it is written at, and says which of the base manifest and the overlay it is in. The base has a role port out of range
# and an unknown key; the overlay names a destination that does not exist and has an unknown key too. The checks:
#   a. config validate exits 1 and prints the four problems, each once, with the file, line and column, and the count;
#   b. config show, which stops at the first problem, prints that one message only;
#   c. a deploy of the broken manifest is refused with the first problem, and creates nothing;
#   d. with the mistakes fixed, config validate prints manifest valid and exits 0.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

OVERLAY=berthy.$ENVN.yml

# The base is written so that the lines are known: the role's port is on line 9 and the unknown key on line 12.
cat > "$MAN/berthy.yml" <<YML
project: $PROJECT
network: $NETWORK
apps:
  web:
    kind: image
    image: $REG/e2e/web
    roles:
      main:
        port: 70000
        health:
          test: ["CMD", "true"]
wat: 1
YML
# The overlay's destination is on line 5 and its unknown key on line 6.
cat > "$MAN/$OVERLAY" <<YML
backups:
  data:
    app: web
    volume: data
    destination: nowhere
nope: 1
YML

want="$OVERLAY:6:1: nope: unknown field \"nope\"
berthy.yml:12:1: wat: unknown field \"wat\"
berthy.yml:9:9: apps.web.roles.main.port: app web role main: invalid settings
$OVERLAY:5:5: backups.data.destination: backup data: unknown destination
4 problems"

out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "config validate exits 1" 1 "$code" "$out"
expect_eq "config validate prints every problem with its file, line and path, and the count" "$want" "$out"

# config show keeps its one line: the first problem.
out=$("$BIN" config show -e "$ENVN" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "config show exits 1" 1 "$code" "$out"
expect_eq "config show prints the first problem's message only" 'unknown field "nope"' "$out"

# A deploy is refused with the first problem and creates nothing.
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy of the broken manifest exits 1" 1 "$code" "$out"
expect_contains "deploy is refused with the first problem" "$out" 'unknown field "nope"'
services=$(d service ls -q --filter "label=berthy.environment=$ENVN")
expect_eq "the refused deploy created no service" "" "$services"
out=$(adm history web 2>&1)
expect_not_contains "the refused deploy recorded no release" "$out" "$WEB_V1_DIGEST"

# Fixed, the manifest is valid.
base_manifest | write_manifest || exit 1
out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "the fixed manifest validates" 0 "$code" "$out"
expect_eq "the fixed manifest prints manifest valid" "manifest valid" "$out"

finish
