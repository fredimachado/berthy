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
stop_agent() { :; }
stop_fakes() { :; }
d() {
  case "$1 $2" in
    'service ls'|'images --no-trunc') ;;
    'network ls')
      if [[ $* == *'label=com.docker.compose.project='* ]]; then :; else printf '%s\n' own-network other-network empty-network; fi ;;
    'network inspect')
      case "${@: -1}" in
        own-network) echo shop-prod-db ;;
        other-network) echo shop-prod-foreign-db ;;
        empty-network) echo '' ;;
      esac ;;
    'network rm') printf '%s\n' "$3" >> "$E2E_ROOT/removed-networks" ;;
    'ps -aq') [[ $* != *'label=com.docker.compose.project=shop-prod-foreign-db'* ]] || echo other-container ;;
    'ps -a') printf '%s\n' 'own-container shop-prod-db' 'other-container shop-two-lab-db' 'similar-container shop-prod-foreign-db' ;;
    'volume ls')
      if [ "${3:-}" = --format ] && [ "${4:-}" = '{{.Name}}' ]; then
        printf '%s\n' shop-prod_data shop-stage_data other-lab_data shop-two-lab_data shop-prod_team-lab_data
      elif [[ $* == *'label=com.docker.compose.project='* ]]; then
        :
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
printf 'volume\t%s\n' shop-prod_team-lab_data shop-prod_new
printf 'compose\t%s\n' shop-prod-foreign-db shop-prod-newdb
NAMES
chmod +x "$NAMES_BIN"
record_deploy shop prod --manifest-dir "$RUN"
for entry in $'volume\tshop-prod_new' $'compose\tshop-prod-newdb'; do
  grep -Fxq "$entry" "$E2E_ROOT/fixture-objects" || { echo "missing intended name: $entry" >&2; exit 1; }
done
for entry in $'volume\tshop-prod_team-lab_data' $'compose\tshop-prod-foreign-db'; do
  if grep -Fxq "$entry" "$E2E_ROOT/fixture-objects"; then echo "claimed existing foreign name: $entry" >&2; exit 1; fi
done
purge_fixture
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
echo 'PASS purge_fixture keeps other projects and removes owned environment volumes'
