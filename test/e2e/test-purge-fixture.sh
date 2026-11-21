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
mkdir -p "$RUN/state/compose/shop-prod-db"
stop_agent() { :; }
stop_fakes() { :; }
d() {
  case "$1 $2" in
    'service ls'|'images --no-trunc') ;;
    'network ls') printf '%s\n' own-network other-network empty-network ;;
    'network inspect')
      case "${@: -1}" in
        own-network) echo shop-prod-db ;;
        other-network) echo shop-prod-foreign-db ;;
        empty-network) echo '' ;;
      esac ;;
    'network rm') printf '%s\n' "$3" >> "$E2E_ROOT/removed-networks" ;;
    'ps -aq') ;;
    'ps -a') printf '%s\n' 'own-container shop-prod-db' 'other-container shop-two-lab-db' 'similar-container shop-prod-foreign-db' ;;
    'volume ls') printf '%s\n' \
      'shop-prod_data ' 'shop-stage_data ' 'other-lab_data ' 'shop-two-lab_data ' \
      'shop-prod-db_data com.docker.compose.project=shop-prod-db' \
      'shop-two-lab-db_data com.docker.compose.project=shop-two-lab-db' \
      'shop-prod-foreign-db_data com.docker.compose.project=shop-prod-foreign-db' \
      'shop-prod_data2 com.docker.compose.project=shop-two-lab-db' ;;
    'volume rm') printf '%s\n' "$3" >> "$E2E_ROOT/removed-volumes" ;;
    'rm -f') printf '%s\n' "$3" >> "$E2E_ROOT/removed-containers" ;;
    'rm -fv') ;;
    *) printf 'unexpected docker command: %s\n' "$*" >&2; return 1 ;;
  esac
}
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
echo 'PASS purge_fixture keeps other projects and removes owned environment volumes'
