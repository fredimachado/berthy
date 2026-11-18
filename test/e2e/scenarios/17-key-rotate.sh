#!/bin/bash
# 17-key-rotate proves `key rotate` on the real agent. A secret reaches its container before a rotation and after it, on the
# next deploy. A dry run changes nothing. The rotation prints key IDs and the command that copies the key, never a key; it
# keeps the old key as <key>.prev, records key_rotate in the audit log and shows a state_key check in the doctor. An agent
# token cannot rotate. The agent starts again with only the new key, and it refuses to start with only the old one.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

key=$RUN/state.key
svc="$PROJECT-web-main"

# key_sum: the SHA-256 of the key file. key_hex: the key itself, which no output may contain.
key_sum() { sha256sum "$key" | cut -d' ' -f1; }
key_hex() { od -An -v -tx1 "$1" | tr -d ' \n'; }

# The web app takes DATABASE_URL from the secret WEB_DB.
cat <<YML | write_manifest
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
    env:
      secret:
        - DATABASE_URL: WEB_DB
YML

# 1. A secret is set and reaches the container of the first deploy.
out=$(printf 'postgres://before-the-rotation' | adm secret set WEB_DB 2>&1); code=$?
expect_code "secret WEB_DB is set" 0 "$code" "$out"
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1 before the rotation" 0 "$code" "$out"
expect_contains "the service runs with the value of WEB_DB" "$(service_spec "$svc")" '"DATABASE_URL=postgres://before-the-rotation"'

old_sum=$(key_sum)
old_hex=$(key_hex "$key")
cp "$key" "$RUN/old.key"

# 2. A dry run counts the sealed values and changes nothing.
out=$(adm key rotate --dry-run 2>&1); code=$?
expect_code "key rotate --dry-run" 0 "$code" "$out"
expect_contains "the dry run says it changed nothing" "$out" "nothing changed"
expect_eq "the dry run leaves the key file as it was" "$old_sum" "$(key_sum)"
if [ -e "$key.prev" ] || [ -e "$key.next" ]; then fail "the dry run leaves no key.prev or key.next" "one exists"; else pass "the dry run leaves no key.prev or key.next"; fi

# 3. An agent token cannot rotate the key, and nothing changes.
out=$(ci key rotate 2>&1); code=$?
expect_code "an agent token's key rotate is refused" 1 "$code" "$out"
expect_eq "the refused rotation leaves the key file as it was" "$old_sum" "$(key_sum)"

# 4. The rotation prints the IDs and the copy command, never the key.
out=$(adm key rotate 2>&1); code=$?
expect_code "key rotate" 0 "$code" "$out"
expect_contains "it says what it resealed" "$out" "rotated the state key: "
expect_contains "it says the previous key is kept" "$out" "the previous key is kept on the server until the next state snapshot succeeds"
expect_contains "it prints the command that copies the new key" "$out" "copy the new key off the server now: sudo cat $key > $ENVN.key"
expect_not_contains "it never prints the old key" "$out" "$old_hex"
expect_not_contains "it never prints the new key" "$out" "$(key_hex "$key")"
if [ "$(key_sum)" != "$old_sum" ]; then pass "the key file holds a new key"; else fail "the key file holds a new key" "it is unchanged"; fi
expect_eq "the old key is kept as key.prev" "$old_sum" "$(sha256sum "$key.prev" | cut -d' ' -f1)"
if [ -e "$key.next" ]; then fail "no key.next is left" "it exists"; else pass "no key.next is left"; fi
expect_eq "the key file is private" "600" "$(stat -c '%a' "$key")"

out=$(adm audit 2>&1); code=$?
expect_code "audit answers" 0 "$code" "$out"
expect_contains "the audit records key_rotate" "$out" "action: key_rotate"
out=$(adm doctor 2>&1); code=$?
expect_contains "the doctor says the previous key is kept" "$out" "previous key kept until the next state snapshot succeeds"

# 5. A deploy after the rotation still gives the container its secret.
out=$(adm deploy web --version v2 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v2 after the rotation" 0 "$code" "$out"
expect_contains "the service still runs with the value of WEB_DB" "$(service_spec "$svc")" '"DATABASE_URL=postgres://before-the-rotation"'
out=$(adm secret list 2>&1); code=$?
expect_code "secret list after the rotation" 0 "$code" "$out"
expect_contains "WEB_DB is still stored" "$out" "WEB_DB"

# 6. The agent starts again with only the new key, which opens everything.
new_sum=$(key_sum)
stop_agent
mv "$key.prev" "$RUN/prev.saved"
start_agent >/dev/null 2>&1; code=$?
expect_code "the agent starts with only the new key" 0 "$code"
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1 with only the new key" 0 "$code" "$out"
expect_contains "the secret still reaches the container" "$(service_spec "$svc")" '"DATABASE_URL=postgres://before-the-rotation"'
expect_eq "the restart leaves the key file as it was" "$new_sum" "$(key_sum)"

# 7. The old key alone does not open the state. The agent does not start and says so.
stop_agent
cp "$key" "$RUN/new.key"
cp "$RUN/old.key" "$key"
chmod 600 "$key"
start_agent >/dev/null 2>&1; code=$?
if [ "$code" -ne 0 ]; then pass "the agent does not start with the old key alone"; else fail "the agent does not start with the old key alone" "it started"; fi
expect_contains "the agent says the key does not open the state" "$(cat "$LOG/agent.log")" "the key does not open this state"
stop_agent

# 8. Put the new key back: the agent starts and the secret is still there.
cp "$RUN/new.key" "$key"
chmod 600 "$key"
start_agent >/dev/null 2>&1; code=$?
expect_code "the agent starts again with the new key" 0 "$code"
out=$(adm secret list 2>&1); code=$?
expect_code "secret list with the new key" 0 "$code" "$out"
expect_contains "WEB_DB is still stored after the restarts" "$out" "WEB_DB"

finish
