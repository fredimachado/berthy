#!/bin/bash
# 25-timezone proves that the manifest's timezone sets the clock every schedule is read on. A manifest with `timezone:
# Asia/Tokyo` and a job at 02:00 shows its next occurrence as 02:00 JST and, nine hours earlier, 17:00 UTC, in the table, in
# --json (`next` in UTC, `next_local` with the offset, `timezone`) and in describe. A job that runs every minute still runs
# with a zone set. `config validate` refuses an unknown zone with the file and line. A dry run names a change of zone, and
# applying it moves the occurrence to the new zone's clock.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

# zone_manifest ZONE: the base manifest with `timezone: ZONE` as its third line, and two jobs of the web app. nightly runs at
# 02:00 on the zone's clock; tick runs every minute.
zone_manifest() {
  cat <<YML
project: $PROJECT
network: $NETWORK
timezone: $1
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
jobs:
  nightly:
    app: web
    role: main
    schedule: "0 2 * * *"
    command: ["true"]
  tick:
    app: web
    role: main
    schedule: "* * * * *"
    command: ["true"]
YML
}

# 1. An unknown zone is refused by config validate, naming the file and the line, before anything is sent.
zone_manifest Mars/Base | write_manifest
out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "config validate refuses an unknown zone" 1 "$code" "$out"
expect_contains "the refusal names the file and line" "$out" "berthy.yml:3:"
expect_contains "the refusal names the zone" "$out" 'invalid timezone "Mars/Base"'
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "a deploy of an unknown zone is refused" 1 "$code" "$out"
expect_contains "the deploy names the zone too" "$out" 'invalid timezone "Mars/Base"'

# 2. With Asia/Tokyo, the job at 02:00 shows 02:00 JST and 17:00 UTC.
zone_manifest Asia/Tokyo | write_manifest
out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "config validate accepts Asia/Tokyo" 0 "$code" "$out"
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy with a timezone" 0 "$code" "$out"
jobs_table=$(adm jobs 2>&1)
nightly=$(printf '%s\n' "$jobs_table" | grep '^nightly ')
if printf '%s' "$nightly" | grep -Eq '[0-9]{4}-[0-9]{2}-[0-9]{2} 02:00 JST \(([0-9]{4}-[0-9]{2}-[0-9]{2} )?17:00Z\)'; then
  pass "jobs shows the next occurrence at 02:00 JST and 17:00 UTC"
else
  fail "jobs shows the next occurrence at 02:00 JST and 17:00 UTC" "$jobs_table"
fi
json=$(adm jobs --json 2>&1)
expect_contains "json names the zone" "$json" '"timezone": "Asia/Tokyo"'
if printf '%s\n' "$json" | grep -Eq '"next": "[0-9-]+T17:00:00Z"'; then pass "json next is the UTC instant"; else fail "json next is the UTC instant" "$json"; fi
if printf '%s\n' "$json" | grep -Eq '"next_local": "[0-9-]+T02:00:00\+09:00"'; then pass "json next_local is the same instant in the zone"; else fail "json next_local is the same instant in the zone" "$json"; fi
out=$(adm describe web 2>&1); code=$?
expect_code "describe" 0 "$code" "$out"
expect_contains "describe names the zone" "$out" "timezone: Asia/Tokyo"
if printf '%s\n' "$out" | grep -Eq 'next_local: [0-9-]+T02:00:00\+09:00' && printf '%s\n' "$out" | grep -Eq 'next: [0-9-]+T17:00:00Z'; then
  pass "describe shows the next occurrence in the zone and in UTC"
else
  fail "describe shows the next occurrence in the zone and in UTC" "$out"
fi

# 3. A job that runs every minute still runs with a zone set.
ran=""
for i in $(seq 1 25); do
  if adm jobs 2>&1 | grep '^tick ' | grep -q succeeded; then ran=yes; break; fi
  sleep 3
done
if [ -n "$ran" ]; then pass "the every-minute job ran with a zone set"; else fail "the every-minute job ran with a zone set" "$(adm jobs 2>&1)"; fi

# 4. A change of zone is named by the dry run, and an apply moves the occurrence to the new clock.
zone_manifest Europe/Lisbon | write_manifest
out=$(adm apply --dry-run --manifest-dir "$MAN" 2>&1); code=$?
expect_code "apply dry run" 0 "$code" "$out"
expect_contains "the dry run names the change of zone" "$out" "timezone: Asia/Tokyo -> Europe/Lisbon"
out=$(adm apply --manifest-dir "$MAN" 2>&1); code=$?
expect_code "apply" 0 "$code" "$out"
expect_contains "apply names the change of zone" "$out" "timezone: Asia/Tokyo -> Europe/Lisbon"
nightly=$(adm jobs 2>&1 | grep '^nightly ')
if printf '%s' "$nightly" | grep -Eq '02:00 (WET|WEST) \('; then pass "jobs shows the next occurrence at 02:00 in Lisbon"; else fail "jobs shows the next occurrence at 02:00 in Lisbon" "$nightly"; fi
out=$(adm apply --dry-run --manifest-dir "$MAN" 2>&1)
expect_eq "a repeat apply has nothing to change" "no changes" "$out"

# 5. Leaving the key out is UTC again, and the dry run says so.
zone_manifest UTC | sed '/^timezone:/d' | write_manifest
out=$(adm apply --dry-run --manifest-dir "$MAN" 2>&1)
expect_contains "the dry run names the return to UTC" "$out" "timezone: Europe/Lisbon -> UTC"

finish
