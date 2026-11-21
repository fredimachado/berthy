#!/bin/bash
# 23-config-origin proves what config show --origin names, what an overlay null removes on a real deploy, and the warning for
# an overlay list that leaves out an entry of the base list. The overlay of the environment sets LOG_LEVEL and removes
# LEGACY_MODE with null: config show --origin names the overlay's file and line for LOG_LEVEL, the base's line for the image,
# default for a value that no file sets, and it does not list the removed variable. The deploy sends the merged manifest, so the
# service runs with the overlay's LOG_LEVEL and without LEGACY_MODE. config validate warns about the dropped entry and still
# passes, and --origin is refused by config validate. setup.sh gives each scenario a fresh manifest directory.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"

overlay="$MAN/berthy.$ENVN.yml"

# place_of PATH: the place that config show --origin printed for PATH in $out: the file and line, or default.
place_of() { printf '%s\n' "$out" | grep -F "$1: " | sed 's/.*  //'; }

# 1. config show --origin: the overlay's file and line for LOG_LEVEL, the base's line for the image, default for a defaulted
# value, and no line for the variable the overlay removes.
cat <<YML | write_manifest
project: $PROJECT
network: $NETWORK
apps:
  web:
    kind: image
    image: $REG/e2e/web
    env:
      plain:
        LOG_LEVEL: info
        LEGACY_MODE: "1"
    roles:
      main:
        health:
          test: ["CMD", "true"]
          interval: 2s
          timeout: 2s
          start_period: 1s
          retries: 3
YML
cat > "$overlay" <<YML
apps:
  web:
    env:
      plain:
        LOG_LEVEL: debug
        LEGACY_MODE: null
YML
out=$("$BIN" config show -e "$ENVN" --manifest-dir "$MAN" --origin 2>&1); code=$?
expect_code "config show --origin exits 0" 0 "$code" "$out"
debug_line=$(grep -n 'LOG_LEVEL' "$overlay" | cut -d: -f1)
image_line=$(grep -n 'image:' "$MAN/berthy.yml" | cut -d: -f1)
expect_eq "LOG_LEVEL names the overlay and its line" "berthy.$ENVN.yml:$debug_line" "$(place_of apps.web.env.plain.LOG_LEVEL)"
expect_eq "the image names the base and its line" "berthy.yml:$image_line" "$(place_of apps.web.image)"
expect_eq "a value that no file sets is default" "default" "$(place_of apps.web.deploy.drain_timeout)"
expect_not_contains "a key the overlay removes is not listed" "$out" "LEGACY_MODE"

json=$("$BIN" config show -e "$ENVN" --manifest-dir "$MAN" --origin --json 2>&1); code=$?
expect_code "config show --origin --json exits 0" 0 "$code" "$json"
expect_contains "--json names the overlay file of an overlay value" "$json" "\"file\": \"berthy.$ENVN.yml\""
expect_contains "--json names the path" "$json" "\"path\": \"apps.web.env.plain.LOG_LEVEL\""

# 2. The deploy sends the merged manifest: the service runs with the overlay's LOG_LEVEL and without LEGACY_MODE.
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1 with an overlay that removes a variable" 0 "$code" "$out"
spec=$(service_spec "$PROJECT-$ENVN-web-main")
expect_contains "the service runs with the overlay's LOG_LEVEL" "$spec" "LOG_LEVEL=debug"
expect_not_contains "the service runs without LEGACY_MODE" "$spec" "LEGACY_MODE"

# 3. An overlay list that leaves out an entry of the base list is a warning, and config validate still passes.
cat <<YML | write_manifest
project: $PROJECT
network: $NETWORK
apps:
  web:
    kind: image
    image: $REG/e2e/web
    env:
      plain:
        LOG_LEVEL: info
      secret:
        - JWT_SIGNING_KEY
        - DATABASE_URL
    roles:
      main:
        health:
          test: ["CMD", "true"]
          interval: 2s
          timeout: 2s
          start_period: 1s
          retries: 3
YML
cat > "$overlay" <<YML
apps:
  web:
    env:
      secret:
        - JWT_SIGNING_KEY
YML
out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "config validate passes with a list warning" 0 "$code" "$out"
expect_contains "config validate warns about the entry the overlay drops" "$out" \
  "warning: berthy.$ENVN.yml:4:7: apps.web.env.secret: replaces the base list and drops DATABASE_URL"
expect_contains "config validate still says the manifest is valid" "$out" "manifest valid"

# 4. --origin is a flag of config show only.
out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$MAN" --origin 2>&1); code=$?
expect_code "config validate refuses --origin" 2 "$code" "$out"
expect_contains "the refusal names the flag and the command" "$out" "--origin is not a flag of config validate"

finish
