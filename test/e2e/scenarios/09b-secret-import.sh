#!/bin/bash
# 09b-secret-import proves `secret import` and `secret list --manifest-dir` on the real Swarm. A file of twenty NAME=value lines
# is imported in one command, and a dry run lists the names and no value. The import is all or nothing: a bad line, a value of
# 65 537 bytes and a registry password name each refuse the whole file and store nothing. `secret list --manifest-dir` names the
# secret a manifest needs that has no value. On a protected environment an agent token's import is held, its plan names the
# secrets and no value, and approving it stores them.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

# manifest_web [protected] [secret lines]: one image app, web, with a health check that passes. protected adds protected: true.
# The secret lines, when given, go under web's env.secret.
manifest_web() {
  [ "${1:-}" = protected ] && echo "protected: true"
  cat <<YML
project: $PROJECT
network: $NETWORK
apps:
  web:
    kind: image
    image: $REG/e2e/web
    roles:
      main:
        health:
          test: ["CMD", "true"]
          interval: 2s
          timeout: 2s
          start_period: 1s
          retries: 3
YML
  if [ -n "${2:-}" ]; then
    cat <<YML
    env:
      secret:
$2
YML
  fi
}

# names FROM TO: the names S01..S20 style, joined the way the CLI prints them: "S01, S02, S03".
names() { seq -f "$1%02.0f" "$2" "$3" | paste -sd, - | sed 's/,/, /g'; }

# list_names: the names the agent holds, one per line, as secret list prints them.
list_names() { adm secret list 2>&1 | sed -n 's/^name: //p'; }

ALL=$(names S 1 20)

# The twenty secrets, with a comment and a blank line. Line 7 is the seventh NAME=value line of the file, S05.
import_file=$RUN/import20.env
{
  echo "# twenty secrets for the e2e"
  echo
  for i in $(seq -w 1 20); do echo "S$i=value-$i"; done
} > "$import_file"

# A dry run lists the twenty names as would create, and no value. Nothing is stored by it.
out=$(adm secret import --dry-run < "$import_file" 2>&1); code=$?
expect_code "a dry run of the twenty-line file exits 0" 0 "$code" "$out"
expect_contains "the dry run lists the twenty names as would create" "$out" "would create: $ALL"
expect_not_contains "the dry run prints no value" "$out" "value-"
expect_eq "a dry run stores nothing" "" "$(list_names)"

# The real import stores all twenty in one command.
out=$(adm secret import < "$import_file" 2>&1); code=$?
expect_code "the twenty-line file is imported in one command" 0 "$code" "$out"
expect_contains "the import says it created the twenty" "$out" "created: $ALL"
expect_not_contains "the import prints no value" "$out" "value-"
expect_eq "secret list shows S01 to S20" "$ALL" "$(list_names | paste -sd, - | sed 's/,/, /g')"

# A dry run of the same file now lists the twenty under would replace.
out=$(adm secret import --dry-run < "$import_file" 2>&1); code=$?
expect_code "a dry run of stored secrets exits 0" 0 "$code" "$out"
expect_contains "the dry run lists the twenty as would replace" "$out" "would replace: $ALL"
expect_not_contains "the second dry run prints no value" "$out" "value-"

# A bad line refuses the whole file, and names the line.
before=$(list_names)
sed '7s/.*/NOT A PAIR/' "$import_file" > "$RUN/bad-line.env"
out=$(adm secret import < "$RUN/bad-line.env" 2>&1); code=$?
expect_code "a file with a line that is not NAME=value is refused" 1 "$code" "$out"
expect_contains "the refusal names line 7" "$out" "line 7: expected NAME=value"
expect_eq "a refused file leaves secret list unchanged" "$before" "$(list_names)"

# A value of 65 537 bytes refuses the whole file, even with a good line before it.
{
  echo "S21=value-21"
  printf 'BIG_VALUE='
  head -c 65537 /dev/zero | tr '\0' x
  echo
} > "$RUN/big.env"
out=$(adm secret import < "$RUN/big.env" 2>&1); code=$?
expect_code "a value of 65 537 bytes is refused" 1 "$code" "$out"
expect_contains "the refusal names the value's secret, not the value" "$out" "BIG_VALUE: the value is larger than 65536 bytes"
expect_eq "a refused file with a good line before it stores nothing" "$before" "$(list_names)"

# A name stored as a registry password is refused by the agent, and the other entries are not stored.
out=$(printf 'registry-token-e2e' | adm secret set REG_TOKEN --registry ghcr.io 2>&1); code=$?
expect_code "a registry password is set for the refusal" 0 "$code" "$out"
{
  echo "S22=value-22"
  echo "REG_TOKEN=plain-value-22"
} > "$RUN/registry-name.env"
out=$(adm secret import < "$RUN/registry-name.env" 2>&1); code=$?
expect_code "an import that names a registry password is refused" 1 "$code" "$out"
expect_contains "the refusal says to use secret set --registry" "$out" "REG_TOKEN is a registry password: use secret set --registry"
expect_not_contains "the refusal does not store the other entry" "$(list_names)" "S22"
expect_not_contains "the refusal prints no value" "$out" "value-22"

# secret list --manifest-dir names the secret the manifest needs that has no value, and not the stored ones.
manifest_web "" "        - DATABASE_URL: MISSING_DB
        - S01" | write_manifest
out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "the manifest that maps a secret with no value validates" 0 "$code" "$out"
out=$(adm secret list --manifest-dir "$MAN" 2>&1); code=$?
expect_code "secret list --manifest-dir exits 0" 0 "$code" "$out"
expect_contains "it names the missing secret and what needs it" "$out" "missing: MISSING_DB (needed by app web)"
expect_not_contains "it does not name the stored S01 as missing" "$out" "missing: S01"

# On a protected environment an agent token's import is held. A deploy makes the environment protected, as 08b-run does.
make_ciadmin || fail "make an agent admin token" "token create failed"

manifest_web protected | write_manifest
out=$(adm deploy web --version "$WEB_V1_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "protect the environment with a digest deploy" 0 "$code" "$out"

held_file=$RUN/held.env
printf 'H01=value-h01\nH02=value-h02\n' > "$held_file"
before=$(list_names)
out=$(cia secret import < "$held_file" 2>&1); code=$?
expect_code "an agent's import is held for approval" 3 "$code" "$out"
expect_contains "the held plan names the first secret" "$out" "H01"
expect_contains "the held plan names the second secret" "$out" "H02"
expect_not_contains "the held plan holds no value" "$out" "value-h"
expect_eq "nothing is stored while the import is held" "$before" "$(list_names)"
id=$(printf '%s\n' "$out" | sed -n 's/^held for approval: \([^ ]*\) .*/\1/p')

out=$(adm approvals 2>&1); code=$?
expect_code "approvals lists the held import" 0 "$code" "$out"
expect_contains "the list summarises it as secret import of two secrets" "$out" "secret import 2 secrets"
expect_not_contains "the list holds no value" "$out" "value-h"

out=$(adm approve "$id" 2>&1); code=$?
expect_code "approve stores the held import" 0 "$code" "$out"
expect_contains "the approved import is in secret list" "$(list_names)" "H02"
expect_contains "the approved import keeps the first secret too" "$(list_names)" "H01"

finish
