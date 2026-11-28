#!/bin/bash
# Seven releases with the same image still count as seven distinct revisions.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

svc="$PROJECT-$ENVN-web-main"
image="$REG/e2e/web@$WEB_V1_DIGEST"
applied_id() {
  printf 'Authorization: Bearer %s\n' "$BERTHY_ADMIN_TOKEN" |
    curl_local --fail --header @- "http://$AGENT_LISTEN/v1/projects/$PROJECT/environments/$ENVN/applied/web" |
    jq -er .id
}

revision() {
  service_spec "$svc" | jq -er '.TaskTemplate.ContainerSpec.Env[] | select(startswith("REVISION=")) | ltrimstr("REVISION=")'
}

ids=()
for rev in {1..7}; do
  write_manifest <<YML
project: $PROJECT
network: $NETWORK
apps:
  web:
    kind: image
    image: $REG/e2e/web
    deploy:
      retain: 2
    roles:
      main:
        health:
          test: ["CMD", "true"]
          interval: 1s
          timeout: 2s
          retries: 1
    env:
      plain:
        REVISION: "$rev"
YML
  out=$(adm deploy web --version "$WEB_V1_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
  expect_code "deploy revision $rev" 0 "$code" "$out"
  if ! id=$(applied_id) || [[ ! $id =~ ^[1-9][0-9]*$ ]]; then
    fail "read the applied release for revision $rev" "invalid release ID '$id'"
    finish
    exit 1
  fi
  ids+=("$id")
done

out=$(adm history web 2>&1); code=$?
expect_code "read release history" 0 "$code" "$out"
expect_eq "history lists exactly releases seven and six" "${ids[6]}"$'\n'"${ids[5]}" "$(sed -n 's/^id: //p' <<<"$out")"

out=$(adm rollback web --to "${ids[5]}" 2>&1); code=$?
expect_code "roll back to release ${ids[5]}" 0 "$code" "$out"
expect_eq "rollback restores revision six in the Swarm service" 6 "$(revision)"
before=$(applied_id)

out=$(adm rollback web --to "${ids[0]}" 2>&1); code=$?
expect_code "refuse rollback to release ${ids[0]}" 1 "$code" "$out"
expect_contains "explain the retained history boundary" "$out" "rollback release is outside retained history; choose a release from berthy history"
expect_eq "refusal preserves the applied release" "$before" "$(applied_id)"
expect_eq "refusal leaves the Swarm service on revision six" 6 "$(revision)"

out=$(adm prune 2>&1); code=$?
expect_code "prune succeeds with a retained image" 0 "$code" "$out"
out=$(d image inspect --format '{{index .RepoDigests 0}}' "$image" 2>&1); code=$?
expect_code "prune keeps the retained image" 0 "$code" "$out"

finish
