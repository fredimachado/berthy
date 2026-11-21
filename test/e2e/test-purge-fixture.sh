#!/bin/bash
# Exercise fixture cleanup with fake Docker responses and no real daemon.
set -euo pipefail
E2E_ROOT=$(mktemp -d)
trap 'rm -rf "$E2E_ROOT"' EXIT
PROJECT=shop ENVN=prod
. "$(dirname "$0")/lib.sh"
mkdir -p "$RUN/environments.d"
printf '{"project":"shop","environment":"prod","state_dir":"%s"}\n' "$RUN/state" > "$RUN/environments.d/one.json"
printf '{"project":"shop","environment":"stage","state_dir":"%s"}\n' "$RUN/state-stage" > "$RUN/environments.d/two.json"
printf '{"project":"other","environment":"lab","state_dir":"%s"}\n' "$RUN/state-other" > "$RUN/environments.d/three.json"
# The Compose project was removed without --volumes, so its directory is already gone.
printf 'volume\t%s\n' shop-prod_data shop-stage_data other-lab_data > "$E2E_ROOT/fixture-objects"
printf 'compose\t%s\n' shop-prod-db >> "$E2E_ROOT/fixture-objects"
printf 'network\t%s\n' shop-prod shop-stage shop-foreign >> "$E2E_ROOT/fixture-objects"
EVENT_FIFO=$E2E_ROOT/network-events
mkfifo "$EVENT_FIFO"
stop_agent() { :; }
stop_fakes() { :; }
sleep() { :; }
start_network_events() { d events --since fake > "$2" & events_pid=$!; }
d() {
  case "$1 $2" in
    'service ls'|'images --no-trunc') ;;
    'network ls')
      if [[ $* == *'--no-trunc'* ]]; then
        local network
        for network in shop-prod shop-stage shop-two-prod shop-foreign shop-delayed shop-retry shop-vanished shop-timeout shop-early; do
          if [ "${EARLY_DESTROY:-}" = 1 ]; then
            [ "$network" = shop-early ] && [ ! -e "$E2E_ROOT/early-gone" ] || continue
          elif [ "${TIMEOUT_NETWORK:-}" = 1 ]; then
            [ "$network" = shop-timeout ] || continue
          elif [ "${RACE_NETWORK:-}" = 1 ]; then
            [ "$network" = shop-vanished ] && [ ! -e "$E2E_ROOT/vanished" ] || continue
          elif [ "${RETRY_NETWORK:-}" = 1 ]; then
            [ "$network" = shop-retry ] && [ ! -e "$E2E_ROOT/retry-success" ] || continue
          elif [ "${DELAYED_NETWORK:-}" = 1 ]; then
            [ "$network" = shop-delayed ] && [ ! -e "$E2E_ROOT/delayed-gone" ] || continue
          else
            case $network in shop-delayed|shop-retry|shop-vanished|shop-timeout|shop-early) continue ;; esac
            if [ -f "$E2E_ROOT/removed-networks" ] && grep -Fxq "$network" "$E2E_ROOT/removed-networks"; then continue; fi
          fi
          if [[ $* == *'{{.ID}} {{.Name}}'* ]]; then echo "id-$network $network"; else echo "id-$network"; fi
        done
      elif [[ $* == *'label=com.docker.compose.project=shop-prod-db'* ]]; then
        if [ ! -e "$E2E_ROOT/network-gone" ]; then echo own-network; fi
      elif [[ $* != *'label=com.docker.compose.project='* ]]; then
        printf '%s\n' own-network other-network vanished-network empty-network
      fi ;;
    'network inspect')
      case "${@: -1}" in
        id-shop-foreign) echo '' ;;
        id-shop-vanished) touch "$E2E_ROOT/vanished"; return 1 ;;
        id-shop-prod|id-shop-stage|id-shop-delayed|id-shop-retry|id-shop-timeout|id-shop-early) echo "$FIXTURE_ID" ;;
        own-network) echo shop-prod-db ;;
        other-network) echo shop-prod-foreign-db ;;
        vanished-network) echo 'foreign network disappeared' >&2; return 1 ;;
        empty-network) echo '' ;;
      esac ;;
    'events --since')
      if [ "${TIMEOUT_NETWORK:-}" = 1 ]; then echo disconnect; return; fi
      exec 3<> "$EVENT_FIFO"
      while read -r event <&3; do
        if [ "$event" = destroy ] && [ "${DELAYED_NETWORK:-}" = 1 ]; then touch "$E2E_ROOT/delayed-gone"; fi
        printf '%s\n' "$event"
      done ;;
    'network rm')
      local network=${3#id-}
      if [ "$network" = shop-early ]; then
        if [ ! -e "$E2E_ROOT/early-refused" ]; then
          touch "$E2E_ROOT/early-refused"; echo 'network is in use' >&2; printf 'destroy\n' > "$EVENT_FIFO"; return 1
        fi
        touch "$E2E_ROOT/early-gone"; return 0
      fi
      if [ "$network" = shop-timeout ]; then
        if [ ! -e "$E2E_ROOT/timeout-refused" ]; then touch "$E2E_ROOT/timeout-refused"; echo 'network is in use' >&2; return 1; fi
        return 0
      fi
      if [ "$network" = shop-retry ] && [ ! -e "$E2E_ROOT/retry-refused" ]; then
        touch "$E2E_ROOT/retry-refused"
        echo 'network has active endpoints and is in use' >&2
        printf 'disconnect\n' > "$EVENT_FIFO"
        return 1
      fi
      if [ "$network" = shop-retry ]; then touch "$E2E_ROOT/retry-success"; fi
      printf '%s\n' "$network" >> "$E2E_ROOT/removed-networks"
      if [[ $3 == id-* ]]; then printf 'destroy\n' > "$EVENT_FIFO"; fi
      if [ "${VANISH_OWN_NETWORK:-}" = 1 ]; then touch "$E2E_ROOT/network-gone"; return 1; fi ;;
    'ps -aq')
      if [[ $* == *'label=com.docker.compose.project=shop-prod-foreign-db'* ]]; then echo other-container
      elif [[ $* == *'label=com.docker.compose.project=shop-prod-db'* ]]; then echo own-container; fi ;;
    'ps -a') printf '%s\n' 'own-container shop-prod-db' 'other-container shop-two-lab-db' 'similar-container shop-prod-foreign-db' ;;
    'volume ls')
      if [ "${3:-}" = --format ] && [ "${4:-}" = '{{.Name}}' ]; then
        printf '%s\n' shop-prod_data shop-stage_data other-lab_data shop-two-lab_data shop-prod_team-lab_data
      elif [[ $* == *'label=com.docker.compose.project=shop-prod-db'* ]]; then
        echo shop-prod-db_data
      elif [[ $* == *'label=com.docker.compose.project='* ]]; then :
      elif [[ $* == *'name=^shop-prod_data$'* ]]; then echo 'shop-prod_data '
      elif [[ $* == *'name=^shop-stage_data$'* ]]; then echo 'shop-stage_data '
      elif [[ $* == *'name=^other-lab_data$'* ]]; then echo 'other-lab_data '
      elif [[ $* == *'--filter name='* ]]; then :
      else
        printf '%s\n' \
          'shop-prod_data ' 'shop-stage_data ' 'other-lab_data ' 'shop-two-lab_data ' 'shop-prod_team-lab_data ' \
          'shop-prod-db_data com.docker.compose.project=shop-prod-db' \
          'shop-two-lab-db_data com.docker.compose.project=shop-two-lab-db' \
          'shop-prod-foreign-db_data com.docker.compose.project=shop-prod-foreign-db' \
          'shop-prod_data2 com.docker.compose.project=shop-two-lab-db'
      fi ;;
    'volume rm') printf '%s\n' "$3" >> "$E2E_ROOT/removed-volumes"; [ "${FAIL_VOLUME:-}" != "$3" ] ;;
    'rm -f') printf '%s\n' "$3" >> "$E2E_ROOT/removed-containers" ;;
    'rm -fv') ;;
    *) printf 'unexpected docker command: %s\n' "$*" >&2; return 1 ;;
  esac
}
mkdir -p "$E2E_ROOT/bin"
cat > "$NAMES_BIN" <<'NAMES'
#!/bin/bash
printf 'volume\t%s\n' shop-prod_uncreated shop-prod_team-lab_data
NAMES
chmod +x "$NAMES_BIN"
if record_deploy shop prod --manifest-dir "$RUN"; then
  echo 'deployment accepted an existing unowned volume' >&2
  exit 1
fi
if grep -Fxq $'volume\tshop-prod_uncreated' "$E2E_ROOT/fixture-objects"; then
  echo 'a refused deploy claimed an uncreated volume' >&2
  exit 1
fi
cat > "$NAMES_BIN" <<'NAMES'
#!/bin/bash
printf 'compose\t%s\n' shop-prod-foreign-db
NAMES
if record_deploy shop prod --manifest-dir "$RUN"; then
  echo 'deployment accepted an existing unowned Compose project' >&2
  exit 1
fi
cat > "$NAMES_BIN" <<'NAMES'
#!/bin/bash
printf 'volume\t%s\n' shop-prod_new
printf 'compose\t%s\n' shop-prod-newdb
NAMES
record_deploy shop prod --manifest-dir "$RUN"
for entry in $'volume\tshop-prod_new' $'compose\tshop-prod-newdb'; do
  grep -Fxq "$entry" "$E2E_ROOT/fixture-objects" || { echo "missing intended name: $entry" >&2; exit 1; }
done
for entry in $'volume\tshop-prod_team-lab_data' $'compose\tshop-prod-foreign-db'; do
  if grep -Fxq "$entry" "$E2E_ROOT/fixture-objects"; then echo "claimed existing foreign name: $entry" >&2; exit 1; fi
done
if ! purge_fixture; then
  echo 'cleanup failed when a foreign network disappeared' >&2
  exit 1
fi
if grep -qx shop-foreign "$E2E_ROOT/removed-networks"; then echo "cleanup removed foreign network with a journaled name" >&2; exit 1; fi
if ! grep -qx shop-prod "$E2E_ROOT/removed-networks" || ! grep -qx shop-stage "$E2E_ROOT/removed-networks" || grep -qx shop-two-prod "$E2E_ROOT/removed-networks"; then
  echo 'cleanup missed an owned network or removed shop-two network' >&2
  exit 1
fi
sort "$E2E_ROOT/removed-volumes" > "$E2E_ROOT/actual-volumes"
printf '%s\n' other-lab_data shop-prod-db_data shop-prod_data shop-stage_data | sort > "$E2E_ROOT/want-volumes"
if ! diff -u "$E2E_ROOT/want-volumes" "$E2E_ROOT/actual-volumes"; then
  echo 'purge removed another fixture volume or missed an owned volume' >&2
  exit 1
fi
if ! grep -qx own-container "$E2E_ROOT/removed-containers" || grep -qx other-container "$E2E_ROOT/removed-containers" || grep -qx similar-container "$E2E_ROOT/removed-containers"; then
  echo 'purge removed another fixture Compose container or missed an owned container' >&2
  exit 1
fi
if ! grep -qx own-network "$E2E_ROOT/removed-networks" || grep -qx other-network "$E2E_ROOT/removed-networks" || grep -qx empty-network "$E2E_ROOT/removed-networks"; then
  echo 'purge removed another fixture network or missed an owned network' >&2
  exit 1
fi
if [ -e "$E2E_ROOT/fixture-objects" ]; then
  echo 'successful cleanup did not clear the ledger' >&2
  exit 1
fi
printf 'volume\tshop-prod_data\n' > "$E2E_ROOT/fixture-objects"
FAIL_VOLUME=shop-prod_data
if purge_fixture; then
  echo 'failed volume removal was reported as successful' >&2
  exit 1
fi
if ! grep -Fxq $'volume\tshop-prod_data' "$E2E_ROOT/fixture-objects"; then
  echo 'failed cleanup lost its volume record' >&2
  exit 1
fi
unset FAIL_VOLUME
purge_fixture
if [ -e "$E2E_ROOT/fixture-objects" ]; then
  echo 'successful retry did not clear the ledger' >&2
  exit 1
fi
printf 'compose\tshop-prod-db\n' > "$E2E_ROOT/fixture-objects"
VANISH_OWN_NETWORK=1
purge_fixture || { echo 'an already removed network failed cleanup' >&2; exit 1; }
if [ -e "$E2E_ROOT/fixture-objects" ]; then
  echo 'cleanup after a vanished network did not clear the ledger' >&2
  exit 1
fi
unset VANISH_OWN_NETWORK
printf 'network\tshop-delayed\n' > "$E2E_ROOT/fixture-objects"
DELAYED_NETWORK=1
purge_fixture || { echo 'delayed network removal failed' >&2; exit 1; }
if [ ! -e "$E2E_ROOT/delayed-gone" ]; then
  echo 'cleanup returned before the network disappeared' >&2
  exit 1
fi
unset DELAYED_NETWORK
printf 'network\tshop-retry\n' > "$E2E_ROOT/fixture-objects"
RETRY_NETWORK=1
purge_fixture || { echo 'cleanup did not retry an in-use network' >&2; exit 1; }
[ -e "$E2E_ROOT/retry-success" ] || { echo 'cleanup missed the retry' >&2; exit 1; }
unset RETRY_NETWORK
printf 'network\tshop-vanished\n' > "$E2E_ROOT/fixture-objects"
RACE_NETWORK=1
purge_fixture || { echo 'cleanup failed after the exact network vanished' >&2; exit 1; }
unset RACE_NETWORK
printf 'network\tshop-timeout\n' > "$E2E_ROOT/fixture-objects"
TIMEOUT_NETWORK=1
if out=$(remove_environment_network id-shop-timeout 1 2>&1); then echo 'missing destroy event was accepted' >&2; exit 1; fi
if [[ $out != *'network is in use'* ]]; then echo "cleanup lost the last Docker error: $out" >&2; exit 1; fi
unset TIMEOUT_NETWORK
printf 'network\tshop-early\n' > "$E2E_ROOT/fixture-objects"
EARLY_DESTROY=1
purge_fixture || { echo 'cleanup failed after an early local destroy event' >&2; exit 1; }
if [ ! -e "$E2E_ROOT/early-gone" ]; then echo 'cleanup cleared the journal while the network ID remained' >&2; exit 1; fi
unset EARLY_DESTROY
# A Docker call may hang even while its event watcher stays open.
mkfifo "$E2E_ROOT/watch-ready" "$E2E_ROOT/stall"
cat > "$E2E_ROOT/bin/stalled-docker" <<'DOCKER'
#!/bin/bash
shift 2
case "$1 $2" in
  'events --since') echo $$ > "$E2E_ROOT/watcher-pid"; printf x > "$E2E_ROOT/watch-ready"; exec cat "$E2E_ROOT/stall" ;;
  'network rm')
    read -r -n 1 ready < "$E2E_ROOT/watch-ready"
    if [ "${STALL_ACTION:-rm}" = ls ]; then exit 0; fi
    echo 'network is in use' >&2; exec cat "$E2E_ROOT/stall" ;;
  'network ls') echo 'network listing stalled' >&2; exec cat "$E2E_ROOT/stall" ;;
  *) exit 1 ;;
esac
DOCKER
chmod +x "$E2E_ROOT/bin/stalled-docker"
export E2E_ROOT
set +e
timeout 60s bash -c '. "$1"; DOCKER_BIN="$2"; remove_environment_network id-stalled 10' _ "$(dirname "$0")/lib.sh" "$E2E_ROOT/bin/stalled-docker" > "$E2E_ROOT/stall-output" 2>&1
status=$?
set -e
watcher=$(cat "$E2E_ROOT/watcher-pid")
if [ "$status" -eq 124 ]; then kill "$watcher" 2>/dev/null || :; echo 'a stalled Docker call exceeded the cleanup bound' >&2; exit 1; fi
if [ "$status" -eq 0 ] || ! grep -q 'network is in use' "$E2E_ROOT/stall-output"; then echo 'the stalled call did not report its error' >&2; exit 1; fi
if kill -0 "$watcher" 2>/dev/null; then kill "$watcher" 2>/dev/null || :; echo 'the event watcher outlived cleanup' >&2; exit 1; fi
export STALL_ACTION=ls
set +e
timeout 60s bash -c '. "$1"; DOCKER_BIN="$2"; remove_environment_network id-stalled 10' _ "$(dirname "$0")/lib.sh" "$E2E_ROOT/bin/stalled-docker" > "$E2E_ROOT/stall-output" 2>&1
status=$?
set -e
watcher=$(cat "$E2E_ROOT/watcher-pid")
if [ "$status" -eq 124 ]; then kill "$watcher" 2>/dev/null || :; echo 'a stalled Docker listing exceeded the cleanup bound' >&2; exit 1; fi
if [ "$status" -eq 0 ] || ! grep -q 'timed out: network listing stalled' "$E2E_ROOT/stall-output"; then echo "the stalled listing did not report its timeout: $(cat "$E2E_ROOT/stall-output")" >&2; exit 1; fi
if kill -0 "$watcher" 2>/dev/null; then kill "$watcher" 2>/dev/null || :; echo 'the event watcher outlived listing cleanup' >&2; exit 1; fi
unset STALL_ACTION
echo 'PASS purge_fixture keeps other projects and removes owned environment volumes'
