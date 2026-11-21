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
stop_agent() { :; }
stop_fakes() { :; }
sleep() { :; }
d() {
  case "$1 $2" in
    'service ls'|'images --no-trunc') ;;
    'network ls')
      if [[ $* == *'--format {{.Name}}'* ]]; then
        if [ "${DELAYED_NETWORK:-}" = 1 ]; then
          if [ ! -e "$E2E_ROOT/delayed-removed" ]; then echo shop-delayed
          elif [ ! -e "$E2E_ROOT/delayed-seen" ]; then touch "$E2E_ROOT/delayed-seen"; echo shop-delayed
          else touch "$E2E_ROOT/delayed-gone"; fi
        else
          for name in shop-prod shop-stage shop-two-prod shop-foreign; do
            if [ ! -f "$E2E_ROOT/removed-networks" ] || ! grep -Fxq "$name" "$E2E_ROOT/removed-networks"; then echo "$name"; fi
          done
        fi
      elif [[ $* == *'label=com.docker.compose.project=shop-prod-db'* ]]; then
        if [ ! -e "$E2E_ROOT/network-gone" ]; then echo own-network; fi
      elif [[ $* != *'label=com.docker.compose.project='* ]]; then
        printf '%s\n' own-network other-network vanished-network empty-network
      fi ;;
    'network inspect')
      case "${@: -1}" in
        shop-foreign) echo '' ;;
        shop-prod|shop-stage|shop-delayed) echo "$FIXTURE_ID" ;;
        own-network) echo shop-prod-db ;;
        other-network) echo shop-prod-foreign-db ;;
        vanished-network) echo 'foreign network disappeared' >&2; return 1 ;;
        empty-network) echo '' ;;
      esac ;;
    'network rm')
      printf '%s\n' "$3" >> "$E2E_ROOT/removed-networks"
      if [ "$3" = shop-delayed ]; then touch "$E2E_ROOT/delayed-removed"; fi
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
echo 'PASS purge_fixture keeps other projects and removes owned environment volumes'
