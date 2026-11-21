#!/bin/bash
# 12b-validate-all proves that config validate reports every problem of a manifest in one run, each with the file and the
# line it is written at, and says which of the base manifest and the overlay it is in. The base has a role port out of range
# and an unknown key; the overlay names a destination that does not exist and has an unknown key too. The checks:
#   a. config validate exits 1 and prints the four problems, each once, with the file, line and column, and the count;
#   b. config show, which stops at the first problem, prints that one message only;
#   c. a deploy of the broken manifest is refused with the first problem, and creates nothing;
#   d. with the mistakes fixed, config validate prints manifest valid and exits 0;
#   e. config validate -e prod_1 against the scenario's manifest directory is refused by the CLI: its exact message, exit 2;
#   f. an overlay that sets project: other is refused by config validate, with the overlay's file and the line of the key.
#   g. offline validation refuses a service name over 63 characters, with the role's file and line.
#   h. offline validation refuses an oversized managed ingress connector and a scoped role volume the engine cannot address.
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

# An environment name with _ is refused by the CLI before anything is read, with its own message and exit code 2.
out=$("$BIN" config validate -e prod_1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "config validate refuses environment prod_1" 2 "$code" "$out"
expect_eq "the refusal of prod_1 is the CLI's message" "valid -e environment required" "$out"

# An overlay cannot change the project: the problem is placed at the overlay's file and at the line of the key.
cat > "$MAN/$OVERLAY" <<YML
network: $NETWORK
project: other
YML
out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "config validate refuses an overlay with another project" 1 "$code" "$out"
expect_eq "the refusal names the overlay, the line and the rule" "$OVERLAY:2:1: project is set in berthy.yml only: an environment cannot have another project
1 problem" "$out"
rm -f "$MAN/$OVERLAY"

# The project and environment together are 55 characters; adding -web-main makes the service name 64.
stop_agent
printf -v long_env '%*s' "$((54 - ${#PROJECT}))" ''
long_env=${long_env// /a}
out=$("$BIN" config validate -e "$long_env" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "offline validation refuses a long service name" 1 "$code" "$out"
expect_contains "the refusal names the service limit" "$out" "is longer than 63 characters; shorten the project, environment, app or role"
expect_contains "the refusal names the role's file and line" "$out" "berthy.yml:8:"

# The generated connector is 64 characters, while the ordinary web service still fits.
connector_suffix=-ingress-connector
printf -v connector_env '%*s' "$((64 - ${#PROJECT} - ${#connector_suffix}))" ''
connector_env=${connector_env// /a}
cat > "$MAN/berthy.yml" <<YML
project: $PROJECT
network: $NETWORK
ingress:
  cloudflare:
    account_id: acct
    api_token_secret: CF_TOKEN
    image: cloudflare/cloudflared:2026.11.0
apps:
  web:
    kind: image
    image: $REG/e2e/web
    roles:
      main: {}
YML
out=$("$BIN" config validate -e "$connector_env" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "offline validation refuses a long ingress connector" 1 "$code" "$out"
expect_contains "the connector refusal gives the service limit" "$out" "is longer than 63 characters; shorten the project, environment, app or role"
expect_contains "the connector refusal names its file and line" "$out" "berthy.yml:4:"

# The scoped role volume is 129 characters, while the service name still fits.
volume_prefix="$PROJECT-${ENVN}_"
printf -v volume '%*s' "$((129 - ${#volume_prefix}))" ''
volume=${volume// /v}
cat > "$MAN/berthy.yml" <<YML
project: $PROJECT
network: $NETWORK
apps:
  web:
    kind: image
    image: $REG/e2e/web
    roles:
      main:
        volumes:
          - $volume:/data
YML
out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "offline validation refuses a long scoped volume" 1 "$code" "$out"
expect_contains "the volume refusal says what to shorten" "$out" "shorten the volume, project or environment"
expect_contains "the volume refusal names its file and line" "$out" "berthy.yml:10:"

finish
