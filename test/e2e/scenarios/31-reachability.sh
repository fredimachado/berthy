#!/bin/bash
# 31-reachability proves the checks at the end of a deploy, on the real Swarm. A role whose port answers on the shared
# network deploys with one line saying so, and the overlay helper that made the check is running afterwards. A role whose
# port is not the one the app listens on still deploys, exits 0 and prints the warning that names the port and asks whether
# the app listens on 127.0.0.1. deploy.verify: false prints nothing about the checks, and --json carries the lines as
# "checks". A helper that cannot run is not exercised here; a unit test covers it.
# The web image listens on port 80 on all interfaces, so the wrong-port case (8081) stands for an app that listens on
# 127.0.0.1 only: both end in a refused connection to the service address.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

svc="$PROJECT-$ENVN-web-main"

# manifest PORT [VERIFY]: the web app with its main role on port PORT. VERIFY, when it is not empty, is written as the app's
# deploy.verify.
manifest() {
  local deploy=""
  if [ -n "${2:-}" ]; then deploy=$(printf '    deploy:\n      verify: %s' "$2")$'\n'; fi
  cat <<YML
project: $PROJECT
apps:
  reader:
    kind: image
    image: $REG/e2e/web
    roles:
      main:
        health:
          test: ["CMD", "true"]
          interval: 2s
          timeout: 2s
          retries: 3
  web:
    kind: image
    image: $REG/e2e/web
${deploy}    roles:
      main:
        port: $1
        health:
          test: ["CMD", "true"]
          interval: 2s
          timeout: 2s
          start_period: 1s
          retries: 3
YML
}

# 1. The right port answers on the network.
manifest 80 | write_manifest
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1 with the port the app listens on" 0 "$code" "$out"
expect_contains "the deploy says the port answers on the network" "$out" "role main: port 80 answers on the network"
expect_not_contains "no warning for a port that answers" "$out" "does not answer"
helpers=$(d ps -q --filter label=berthy.helper=probe --filter label=berthy.environment="$ENVN" | wc -l)
expect_eq "the overlay helper that made the check is running" "1" "$helpers"

out=$(adm deploy reader --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy another app on the environment network" 0 "$code" "$out"
out=$(adm exec reader -- wget -qO- http://web-main/ 2>&1); code=$?
expect_code "another app reaches web by its app-role alias" 0 "$code" "$out"
expect_contains "the alias returns the web page" "$out" "nginx"

# 2. A wrong port warns, and the deploy still succeeds with the new version serving.
manifest 8081 | write_manifest
out=$(adm deploy web --version v2 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v2 with a port nothing listens on exits 0" 0 "$code" "$out"
expect_contains "the deploy warns that the port does not answer" "$out" \
  "role main: healthy, but port 8081 does not answer on the network; is the app listening on 127.0.0.1?"
expect_contains "the deploy still succeeded" "$out" "succeeded"
spec=$(service_spec "$svc")
expect_contains "the service runs the v2 digest" "$spec" "$WEB_V2_DIGEST"

# 3. deploy.verify: false turns the checks off.
manifest 8081 false | write_manifest
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1 with deploy.verify false" 0 "$code" "$out"
expect_not_contains "no line about the network with verify off" "$out" "on the network"

# 4. --json keeps one document and carries the lines as checks.
manifest 80 | write_manifest
json=$(adm deploy web --version v2 --manifest-dir "$MAN" --json 2>/dev/null); code=$?
expect_code "deploy v2 --json exits 0" 0 "$code" "$json"
expect_contains "--json carries the check line" "$json" "role main: port 80 answers on the network"
expect_contains "--json names the checks key" "$json" '"checks"'
expect_not_contains "--json prints no event names around the document" "$json" "accepted"

finish
