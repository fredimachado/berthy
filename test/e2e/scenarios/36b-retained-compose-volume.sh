#!/bin/bash
# A Compose volume kept by remove must be cleaned before the next fixture starts.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"
project="$PROJECT-$ENVN-db"
volume="${project}_data"

if d volume inspect "$volume" >/dev/null 2>&1; then
  fail "the fixture starts without the prior volume" "$volume exists"
else
  pass "the fixture starts without the prior volume"
fi
write_manifest <<YML
project: $PROJECT
network: $NETWORK
apps:
  db:
    kind: compose
    file: compose.yml
YML
cat > "$MAN/compose.yml" <<YML
services:
  db:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sleep", "3600"]
    volumes: ["data:/data"]
volumes:
  data: {}
YML
out=$(adm deploy db --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy the Compose app with a volume" 0 "$code" "$out"
out=$(adm exec db -- sh -c 'test ! -e /data/marker && echo old-data > /data/marker' 2>&1); code=$?
expect_code "the volume is empty and receives a marker" 0 "$code" "$out"
out=$(adm remove db 2>&1); code=$?
expect_code "remove the Compose app without --volumes" 0 "$code" "$out"
if d volume inspect "$volume" >/dev/null 2>&1; then pass "remove keeps the volume"; else fail "remove keeps the volume" "$volume is missing"; fi
if [ ! -d "$RUN/state/compose/$project" ]; then pass "remove deleted the Compose directory"; else fail "remove deleted the Compose directory" "directory remains"; fi
if purge_fixture; then pass "fixture cleanup succeeds"; else fail "fixture cleanup succeeds" "cleanup failed"; fi
if d volume inspect "$volume" >/dev/null 2>&1; then fail "cleanup removed the retained volume" "$volume remains"; else pass "cleanup removed the retained volume"; fi
finish
