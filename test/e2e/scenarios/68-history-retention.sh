#!/bin/bash
# Seven releases with the same image still count as seven distinct revisions.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

svc="$PROJECT-$ENVN-web-main"
image="$REG/e2e/web@$WEB_V1_DIGEST"
fixture_value=history-retention-probe-value
fixture_secret=HISTORY_FIXTURE_SECRET
set_fixture_secret() { printf '%s' "$fixture_value" | adm secret set "$fixture_secret" >/dev/null 2>&1; }

applied_id() {
  printf 'Authorization: Bearer %s\n' "$BERTHY_ADMIN_TOKEN" |
    curl_local --fail --header @- "http://$AGENT_LISTEN/v1/projects/$PROJECT/environments/$ENVN/applied/web" |
    jq -er .id
}

revision() {
  service_spec "$svc" | jq -er '.TaskTemplate.ContainerSpec.Env[] | select(startswith("REVISION=")) | ltrimstr("REVISION=")'
}

hook_ids() {
  d ps -aq --no-trunc --filter "label=berthy.project=$PROJECT" --filter "label=berthy.environment=$ENVN" \
    --filter 'label=berthy.oneoff=pre_hook'
}

check_historical_evidence() {
  local cid out code empty hooks
  # Managed logs select active Swarm tasks. Feed the canary to the current task on stdin so the fixture value stays out of argv.
  cid=$(d ps -q --filter "label=com.docker.swarm.service.name=$svc")
  printf 'history-probe %s\n' "$fixture_value" | d exec -i "$cid" sh -c 'cat > /proc/1/fd/1' >/dev/null 2>&1
  expect_code "write a log record using the retired mapping's fixture value" 0 "$?"
  out=$(adm logs image:web --grep '^history-probe ' 2>&1); code=$?
  expect_code "read the managed logs after revision three" 0 "$code"
  if [[ $out == *'history-probe [REDACTED]'* && $out != *"$fixture_value"* ]]; then
    pass "managed logs redact the value from the first release's mapping"
  else
    fail "managed logs redact the value from the first release's mapping" "redacted probe missing or fixture value exposed"
  fi

  adm secret rm "$fixture_secret" >/dev/null 2>&1
  expect_code "remove the fixture secret" 0 "$?"
  check_fixture_secret_absent
  empty=$(mktemp -d "$RUN/empty-manifest.XXXXXX")
  out=$(adm secret generate "$fixture_secret" --manifest-dir "$empty" 2>&1); code=$?
  rmdir "$empty"
  expect_code "refuse named generation from an empty manifest directory" 1 "$code"
  if [[ $out == *"app web was deployed here and may hold the value of $fixture_secret"* ]]; then
    pass "generation names the historical reader"
  else
    fail "generation names the historical reader" "historical-reader diagnostic missing"
  fi
  check_fixture_secret_absent
  set_fixture_secret
  expect_code "restore the fixture secret for the rollback checks" 0 "$?"

  adm prune >/dev/null 2>&1
  expect_code "prune the journaled pre-deploy hook" 0 "$?"
  if hooks=$(hook_ids) && [[ -n $first_hook && -z $hooks ]]; then
    pass "the journaled pre-deploy hook is gone"
  else
    fail "the journaled pre-deploy hook is gone" "missing initial hook identity or a hook is still present"
  fi
}

check_fixture_secret_absent() {
  local listed
  if listed=$(adm secret list 2>&1) && [[ $listed != *"$fixture_secret"* ]]; then
    pass "the fixture secret is absent"
  else
    fail "the fixture secret is absent" "secret list failed or still names the fixture"
  fi
}

set_fixture_secret
expect_code "store a fixture secret before the first deploy" 0 "$?"
ids=()
first_hook=""
for rev in {1..7}; do
  {
    cat <<YML
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
    if [[ $rev == 1 ]]; then
      cat <<YML
      secret:
        - HISTORY_OLD_VAR: $fixture_secret
    hooks:
      pre_deploy:
        - run: ["true"]
YML
    fi
  } | write_manifest
  out=$(adm deploy web --version "$WEB_V1_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
  expect_code "deploy revision $rev" 0 "$code" "${out//"$fixture_value"/[REDACTED]}"
  if ! id=$(applied_id) || [[ ! $id =~ ^[1-9][0-9]*$ ]]; then
    fail "read the applied release for revision $rev" "invalid release ID '$id'"
    finish
    exit 1
  fi
  ids+=("$id")
  if [[ $rev == 1 ]]; then
    if first_hook=$(hook_ids) && [[ $first_hook =~ ^[0-9a-f]{64}$ ]]; then
      pass "the first release left a journaled hook container"
    else
      fail "the first release left a journaled hook container" "expected exactly one hook ID"
    fi
  elif [[ $rev == 3 ]]; then
    check_historical_evidence
  fi
done

out=$(adm history web 2>&1); code=$?
expect_code "read release history" 0 "$code" "$out"
expect_eq "history lists exactly releases seven and six" "${ids[6]}"$'\n'"${ids[5]}" "$(sed -n 's/^id: //p' <<<"$out")"

out=$(python3 - "$RUN/state/state.db" "$RUN/history-check.db" "$ENVN" <<'PY'
from contextlib import closing
import sqlite3
import sys

source, snapshot, environment = sys.argv[1:]
with closing(sqlite3.connect('file:' + source + '?mode=ro', uri=True)) as db:
    with closing(sqlite3.connect(snapshot)) as copy:
        db.backup(copy)
with closing(sqlite3.connect('file:' + snapshot + '?mode=ro', uri=True)) as copy:
    print(*[row[0] for row in copy.execute(
        'SELECT id FROM releases WHERE environment=? AND app=? ORDER BY id DESC', (environment, 'web'))], sep='\n')
    print(copy.execute('SELECT count(*) FROM operations WHERE environment=? AND app=?', (environment, 'web')).fetchone()[0])
    print(len(copy.execute('PRAGMA foreign_key_check').fetchall()))
PY
); code=$?
expect_code "read a closed copy of the fixture database" 0 "$code"
expect_eq "database keeps two release rows and two app operations with valid foreign keys" "${ids[6]}"$'\n'"${ids[5]}"$'\n2\n0' "$out"
rm -f -- "$RUN/history-check.db"

out=$(adm rollback web --to "${ids[5]}" 2>&1); code=$?
expect_code "roll back to release ${ids[5]}" 0 "$code" "$out"
expect_eq "rollback restores revision six in the Swarm service" 6 "$(revision)"
before=$(applied_id)

out=$(adm rollback web --to "${ids[0]}" 2>&1); code=$?
out=${out//"$fixture_value"/[REDACTED]}
expect_code "refuse rollback to release ${ids[0]}" 1 "$code" "$out"
expect_contains "explain the retained history boundary" "$out" "rollback release is outside retained history; choose a release from berthy history"
expect_eq "refusal preserves the applied release" "$before" "$(applied_id)"
expect_eq "refusal leaves the Swarm service on revision six" 6 "$(revision)"

out=$(adm prune 2>&1); code=$?
expect_code "prune succeeds with a retained image" 0 "$code" "$out"
out=$(d image inspect --format '{{index .RepoDigests 0}}' "$image" 2>&1); code=$?
expect_code "prune keeps the retained image" 0 "$code" "$out"

finish
