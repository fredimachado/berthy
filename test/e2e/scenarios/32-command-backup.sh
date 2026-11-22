#!/bin/bash
# 32-command-backup proves a backup that takes the standard output of a command, and a restore that feeds one. A Compose
# postgres app is backed up with pg_dump into the fake S3: the object is a gzip of the dump, listed with its size, and nothing
# but that is made. A command that exits non-zero leaves no listed backup and no object, its check goes critical with the
# command's error, and its detail keeps the end of its standard error. `restore --exec` loads the backup into a scratch
# database through pg_restore and the data matches, after the object's SHA-256 was checked; a tampered object is refused and the
# command never starts. An image app's command runs in a container of its own that Docker does not log, and one past its
# timeout leaves no container. `restore --into` refuses a command backup, and `config validate` refuses a backup with both
# `volume` and `command`, or with neither. The database service runs postgres:17-alpine, which the fixture has.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

cat > "$MAN/compose.yml" <<YML
services:
  postgres:
    image: postgres:17-alpine
    environment:
      POSTGRES_PASSWORD: \${POSTGRES_PASSWORD}
YML

manifest() {
  base_manifest
  cat <<YML
  db:
    kind: compose
    file: compose.yml
    env:
      plain:
        POSTGRES_PASSWORD: e2e-pass
destinations:
  offsite:
    type: s3
    endpoint: http://$S3_LISTEN
    bucket: $S3_BUCKET
    access_key_secret: S3_ACCESS_KEY
    secret_key_secret: S3_SECRET_KEY
backups:
  main-db:
    app: db
    service: postgres
    command: ["sh", "-c", "pg_dump -Fc --no-acl --no-owner -U postgres appdb"]
    destination: offsite
    retention:
      keep: 3
  broken:
    app: db
    service: postgres
    command: ["sh", "-c", "echo partial-dump; echo 'dump went wrong: no such table' >&2; exit 3"]
    destination: offsite
  note:
    app: web
    role: main
    command: ["sh", "-c", "echo note-from-a-one-off-container"]
    destination: offsite
  slow:
    app: web
    role: main
    command: ["sh", "-c", "echo data; sleep 12; echo late"]
    destination: offsite
  stuck:
    app: web
    role: main
    command: ["sh", "-c", "echo started; sleep 60"]
    timeout: 3s
    destination: offsite
YML
}

backup_containers() { d ps -aq --filter label=berthy.environment=$ENVN --filter label=berthy.oneoff=backup; }
psql_db() { adm exec db --service postgres -- psql -U postgres "$@" 2>&1; }
# has_line OUTPUT LINE: true when LINE is a whole line of OUTPUT. The CLI prints a command's output between the events and the
# operation's record, so an answer is found as a line and not as the last one.
has_line() { printf '%s\n' "$1" | grep -qxF -- "$2"; }

manifest | write_manifest
printf 'AKIAE2ETESTACCESS' | adm secret set S3_ACCESS_KEY >/dev/null 2>&1
printf 'e2e-test-secret-key-0123456789' | adm secret set S3_SECRET_KEY >/dev/null 2>&1
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy the image app" 0 "$code" "$out"
out=$(adm deploy db --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy the Compose database" 0 "$code" "$out"

ready=1
for _ in $(seq 1 60); do
  out=$(adm exec db --service postgres -- pg_isready -U postgres 2>&1); code=$?
  if [ "$code" -eq 0 ]; then ready=0; break; fi
  sleep 1
done
expect_code "postgres accepts connections" 0 "$ready" "$out"

out=$(psql_db -c "create database appdb"); code=$?
expect_code "create the database" 0 "$code" "$out"
out=$(psql_db -d appdb -c "create table t(v text); insert into t values ('first-row')"); code=$?
expect_code "insert a row" 0 "$code" "$out"

# 1. A backup of the dump command is listed with its size, as a gzip with its sidecar, and is one object and its sidecar only.
out=$(adm backup run main-db 2>&1); code=$?
expect_code "backup run of the dump command" 0 "$code" "$out"
expect_contains "the run succeeded" "$out" "status: succeeded"
key=$(sed -n 's/^key: //p' <<<"$out")
case $key in
  $PROJECT/$ENVN/db/command/main-db/*.gz) pass "the key is under the app's command directory: $key" ;;
  *) fail "the key is under the app's command directory" "got '$key'" ;;
esac
out=$(adm backup list --backup main-db 2>&1); code=$?
expect_code "backup list" 0 "$code" "$out"
expect_contains "the backup is listed" "$out" "$key"
expect_contains "it has no volume" "$out" "command"
expect_not_contains "its size is not zero" "$out" " 0 B "
objects=$(s3_objects "$PROJECT/$ENVN/db/command/main-db/")
expect_contains "the sidecar is stored" "$objects" "${key%.gz}.json"
expect_eq "the object and its sidecar are all that is stored" "2" "$(grep -o '"key"' <<<"$objects" | wc -l)"
# The stored object is a gzip of the dump (pg_dump -Fc starts with PGDMP).
raw=$(curl_local "http://$S3_LISTEN/$S3_BUCKET/$key" | gzip -dc | head -c 5)
expect_eq "the object is the gzip of a custom-format dump" "PGDMP" "$raw"

# 2. A command that exits non-zero leaves no listed backup and no object, and its check shows the error.
out=$(adm backup run broken 2>&1); code=$?
expect_code "a command that exits non-zero fails the backup" 1 "$code" "$out"
expect_contains "the run says the command exited 3" "$out" "exited with code 3"
expect_contains "the run keeps the end of its standard error" "$out" "dump went wrong: no such table"
out=$(adm backup list --backup broken 2>&1)
expect_contains "nothing is listed for the failed backup" "$out" "no backups"
expect_eq "no object is left for the failed backup" "0" "$(grep -o '"key"' <<<"$(s3_objects "$PROJECT/$ENVN/db/command/broken/")" | wc -l)"
checked=""
for _ in $(seq 1 40); do
  out=$(adm checks 2>&1)
  if grep -F "backup:broken" <<<"$out" | grep -qF "dump went wrong"; then checked=yes; break; fi
  sleep 3
done
if [ -n "$checked" ]; then pass "the check of the failed backup shows the command's error"; else fail "the check of the failed backup shows the command's error" "not seen in checks"; detail_block "$out"; fi
expect_contains "the check is critical" "$(grep -F 'backup:broken' <<<"$out")" "critical"

# 3. restore --exec loads the backup into a scratch database, and the data matches. A row added after the backup is not in it.
out=$(psql_db -d appdb -c "insert into t values ('second-row')"); code=$?
expect_code "insert a row after the backup" 0 "$code" "$out"
out=$(psql_db -c "create database scratch"); code=$?
expect_code "create the scratch database" 0 "$code" "$out"
out=$(adm restore "$key" --exec db --service postgres --dry-run -- pg_restore -U postgres --clean --if-exists --no-owner -d scratch 2>&1); code=$?
expect_code "restore --exec --dry-run" 0 "$code" "$out"
expect_contains "the plan says where it runs" "$out" "db service postgres"
expect_contains "the plan shows the command" "$out" "pg_restore -U postgres --clean --if-exists --no-owner -d scratch"
out=$(psql_db -d scratch -tAc "select count(*) from pg_tables where tablename = 't'")
has_line "$out" 0; expect_code "a dry run restored nothing" 0 "$?" "$out"
out=$(adm restore "$key" --exec db --service postgres -- pg_restore -U postgres --clean --if-exists --no-owner -d scratch 2>&1); code=$?
expect_code "restore --exec into the scratch database" 0 "$code" "$out"
expect_contains "the operation succeeded" "$out" "succeeded"
out=$(psql_db -d scratch -tAc "select v from t order by v")
has_line "$out" first-row; expect_code "the scratch database has the data of the backup" 0 "$?" "$out"
has_line "$out" second-row; expect_code "and not the row added after it" 1 "$?" "$out"

# 4. restore --into refuses a command backup, and a restore --exec needs a command.
out=$(adm restore "$key" --into some-volume 2>&1); code=$?
expect_code "restore --into of a command backup is refused" 1 "$code" "$out"
expect_contains "the refusal says to use --exec" "$out" "command backup"
out=$(adm restore "$key" --exec db --service postgres 2>&1); code=$?
expect_code "restore --exec without a command is a usage error" 2 "$code" "$out"
out=$(adm restore "$key" --exec db --role main -- true 2>&1); code=$?
expect_code "a role of a Compose app is refused" 1 "$code" "$out"
expect_contains "the refusal names the service" "$out" "use service for a Compose app"
d volume ls -q | grep -qx some-volume && fail "restore --into made no volume" "some-volume exists" || pass "restore --into made no volume"

# 5. A restore command that fails shows its exit code and its last lines.
out=$(adm restore "$key" --exec db --service postgres -- sh -c 'cat >/dev/null; echo restore-complaint >&2; exit 4' 2>&1); code=$?
expect_code "a failing restore command fails the restore" 1 "$code" "$out"
expect_contains "the failure says what happened" "$out" "restore command failed"
expect_contains "the last lines of the command are shown" "$out" "restore-complaint"
expect_contains "the exit code is shown" "$out" "exited with code 4"

# 6. A tampered object is refused before the command starts: the scratch database is not touched.
curl_local -X PUT --data-binary 'this is not the backup that was stored' "http://$S3_LISTEN/$S3_BUCKET/$key" >/dev/null
out=$(psql_db -c "create database scratch2"); code=$?
expect_code "create the second scratch database" 0 "$code" "$out"
out=$(adm restore "$key" --exec db --service postgres -- sh -c 'cat > /tmp/e2e-restore-fed; pg_restore -U postgres --no-owner -d scratch2 /tmp/e2e-restore-fed' 2>&1); code=$?
expect_code "a tampered backup fails the restore" 1 "$code" "$out"
expect_contains "the failure is the checksum" "$out" "backup checksum mismatch"
out=$(adm exec db --service postgres -- sh -c 'test -e /tmp/e2e-restore-fed && echo yes || echo no' 2>&1)
has_line "$out" no; expect_code "the command never ran: its input file does not exist" 0 "$?" "$out"
out=$(psql_db -d scratch2 -tAc "select count(*) from pg_tables where schemaname = 'public'")
has_line "$out" 0; expect_code "the scratch database has no table" 0 "$?" "$out"

# 7. A command of an image app runs in a container of its own, which is gone at the end and which Docker does not log.
out=$(adm backup run note 2>&1); code=$?
expect_code "backup run of an image app's command" 0 "$code" "$out"
expect_contains "it succeeded" "$out" "status: succeeded"
note=$(sed -n 's/^key: //p' <<<"$out")
case $note in
  $PROJECT/$ENVN/web/command/note/*.gz) pass "the key of an image app's command backup: $note" ;;
  *) fail "the key of an image app's command backup" "got '$note'" ;;
esac
expect_eq "the output of the command is what is stored" "note-from-a-one-off-container" "$(curl_local "http://$S3_LISTEN/$S3_BUCKET/$note" | gzip -dc)"
expect_eq "no container of the backup is left" "" "$(backup_containers)"

adm backup run slow >"$RUN/slow.out" 2>&1 &
slowpid=$!
cid=""
for _ in $(seq 1 30); do
  cid=$(backup_containers | head -n 1)
  [ -n "$cid" ] && break
  sleep 0.5
done
if [ -n "$cid" ]; then pass "the command runs in a container of its own"; else fail "the command runs in a container of its own" "no container"; fi
expect_eq "Docker keeps no log of the output" "none" "$(d inspect --format '{{.HostConfig.LogConfig.Type}}' "$cid" 2>/dev/null)"
expect_eq "the container carries the backup's label" "slow" "$(d inspect --format '{{index .Config.Labels "berthy.backup"}}' "$cid" 2>/dev/null)"
wait "$slowpid"; code=$?
expect_code "the slow backup finished" 0 "$code" "$(cat "$RUN/slow.out")"
expect_eq "its container is removed" "" "$(backup_containers)"

# 8. An image app's command past its timeout is stopped: nothing is left, and no backup is listed.
out=$(adm backup run stuck 2>&1); code=$?
expect_code "a command past its timeout fails the backup" 1 "$code" "$out"
expect_contains "the failure says it timed out" "$out" "timed out after 3s"
expect_eq "no container of the stuck command is left" "" "$(backup_containers)"
out=$(adm backup list --backup stuck 2>&1)
expect_contains "nothing is listed for it" "$out" "no backups"

# 9. config validate refuses a backup with both a volume and a command, or with neither, and says where.
bad=$(mktemp -d)
manifest | sed 's|^  broken:|  broken:\n    volume: pgdata|' > "$bad/berthy.yml"
cp "$MAN/compose.yml" "$bad/"
out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$bad" 2>&1); code=$?
expect_code "both volume and command is refused" 1 "$code" "$out"
expect_contains "the message says exactly one" "$out" "name exactly one of volume and command"
expect_contains "it names the file" "$out" "berthy.yml"
manifest | sed '/^    command: \["sh", "-c", "echo note-from-a-one-off-container"\]/d' > "$bad/berthy.yml"
out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$bad" 2>&1); code=$?
expect_code "neither volume nor command is refused" 1 "$code" "$out"
expect_contains "the message says exactly one" "$out" "name exactly one of volume and command"
rm -rf "$bad"

finish
