#!/bin/bash
# Compose rollback uses the saved file even after its producer leaves the ordinary operation window.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

proj="$PROJECT-$ENVN-db"
write_manifest <<YML
project: $PROJECT
network: $NETWORK
apps:
  db:
    kind: compose
    file: compose.yml
    deploy:
      retain: 2
YML

write_compose() {
  cat > "$MAN/compose.yml" <<YML
services:
  database:
    image: $REG/e2e/web@$WEB_V1_DIGEST
    entrypoint: ["sleep", "3600"]
    environment:
      MODE: "$1"
YML
}

mode() {
  local id
  id=$(d ps -q --filter "label=com.docker.compose.project=$proj" --filter 'label=com.docker.compose.service=database') &&
    d inspect --format '{{json .Config.Env}}' "$id" | jq -er '.[] | select(startswith("MODE=")) | ltrimstr("MODE=")'
}

ids=()
for rev in {1..3}; do
  write_compose "$rev"
  out=$(adm deploy db --manifest-dir "$MAN" 2>&1); code=$?
  expect_code "deploy Compose revision $rev" 0 "$code" "$out"
  out=$(adm history db 2>&1); code=$?
  expect_code "read Compose release history" 0 "$code" "$out"
  ids+=("$(sed -n 's/^id: //p' <<<"$out" | sed -n '1p')")
done

for n in {1..2}; do
  out=$(adm restart db 2>&1); code=$?
  expect_code "restart Compose so its deploy leaves the operation window ($n of 2)" 0 "$code" "$out"
done
expect_eq "the Compose container has revision three" 3 "$(mode)"
out=$(adm history db 2>&1); code=$?
expect_code "read the retained Compose history" 0 "$code" "$out"
expect_eq "Compose history lists only releases three and two" "${ids[2]}"$'\n'"${ids[1]}" "$(sed -n 's/^id: //p' <<<"$out")"

write_compose 99
out=$(adm rollback db --to "${ids[1]}" 2>&1); code=$?
expect_code "roll back to the oldest retained Compose release" 0 "$code" "$out"
expect_eq "the Compose container runs the saved file from revision two" 2 "$(mode)"

finish
