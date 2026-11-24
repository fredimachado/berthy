#!/bin/bash
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$(dirname "$0")/../deploy-ux-fixture.sh" || exit 1
# Exercise failure paths without a Docker daemon or the operator's curl configuration.
fixture_self_test() (
  local tmp failed=0 check
  tmp=$(mktemp -d) || exit 1
  trap 'rm -rf -- "$tmp"' EXIT
  check_curl() (
    export HOME="$tmp" CURL_HOME="$tmp"
    printf 'trace-ascii = "%s/trace"\n' "$tmp" > "$tmp/.curlrc"
    printf 'probe' > "$tmp/input"
    curl --noproxy '*' -sS "file://$tmp/input" >/dev/null || exit 1
    [ -f "$tmp/trace" ] || exit 1
    rm "$tmp/trace"
    private_curl "file://$tmp/input" >/dev/null || exit 1
    [ ! -e "$tmp/trace" ]
  )
  check_seed_config() (
    PRIVATE_DIR="$tmp/config-test" PRIVATE_REG=localhost:1234 PRIVATE_USER=user PRIVATE_PASSWORD=password
    mkdir "$PRIVATE_DIR"
    private_seed_config || exit 1
    jq -e --arg host "$PRIVATE_REG" 'keys == ["auths"] and (.auths | keys) == [$host] and (.auths[$host] | keys) == ["auth"]' "$PRIVATE_SEED_CONFIG/config.json" >/dev/null
  )
  check_inventory() (
    PRIVATE_LOCAL_TAG= PRIVATE_LOCAL_IMAGE=
    d() { case "$1 $2" in "image inspect") [ "${3:-}" != target ] || return 1; echo source-id ;; "tag source") touch "$tmp/tagged" ;; *) return 1 ;; esac; }
    private_image_inventory() { return 1; }
    if private_tag_image source target; then exit 1; fi
    [ ! -e "$tmp/tagged" ] || exit 1
    private_image_inventory() { printf '%s\n' '[{"Id":"sha256:abc","RepoTags":["target"]}]'; }
    if private_tag_image source target; then exit 1; fi
    [ ! -e "$tmp/tagged" ] || exit 1
    private_image_inventory() { printf '%s\n' '{"message":"incomplete"}'; }
    if private_tag_image source target; then exit 1; fi
    [ ! -e "$tmp/tagged" ] || exit 1
    private_image_inventory() { printf '%s\n' '[{"Id":"sha256:abc"}]'; }
    if private_tag_image source target; then exit 1; fi
    [ ! -e "$tmp/tagged" ] || exit 1
    private_image_inventory() { printf '%s\n' '[]' '[]'; }
    if private_tag_image source target; then exit 1; fi
    [ ! -e "$tmp/tagged" ] || exit 1
    private_image_inventory() { printf '%s\n' '[]'; }
    d() { case "$1 $2" in "image inspect") echo source-id ;; "tag source") return 1 ;; *) return 1 ;; esac; }
    if private_tag_image source target; then exit 1; fi
    [ "$PRIVATE_LOCAL_TAG" = target ] && [ "$PRIVATE_LOCAL_IMAGE" = source-id ]
  )
  check_cleanup() (
    PRIVATE_ID= PRIVATE_CREATE_PENDING= PRIVATE_OWNER=owner PRIVATE_LOCAL_TAG=target PRIVATE_LOCAL_IMAGE=expected
    PRIVATE_DIR="$tmp/secret-dir"; mkdir "$PRIVATE_DIR"; touch "$PRIVATE_DIR/curl.conf"
    d() { return 1; }
    if stop_private_registry trap; then exit 1; fi
    [ ! -e "$tmp/secret-dir" ]
  )
  check_uncertain_create() (
    PRIVATE_LOCAL_TAG= PRIVATE_DIR= PRIVATE_ID= PRIVATE_CREATE_PENDING=1 PRIVATE_NAME=fixture PRIVATE_OWNER=owner
    d() {
      case $1 in
        container) printf '%s\n' 'actual-id owner' ;;
        rm) [ "$3" = actual-id ] || return 1; touch "$tmp/removed" ;;
        *) return 1 ;;
      esac
    }
    stop_private_registry trap || exit 1
    [ -e "$tmp/removed" ] || exit 1
    rm "$tmp/removed"
    PRIVATE_ID= PRIVATE_CREATE_PENDING=1
    d() { case $1 in container) printf '%s\n' 'actual-id foreign' ;; rm) touch "$tmp/removed" ;; *) return 1 ;; esac; }
    if stop_private_registry trap; then exit 1; fi
    [ ! -e "$tmp/removed" ]
  )
  for check in check_curl check_seed_config check_inventory check_cleanup check_uncertain_create; do
    if "$check"; then echo "  ok   $check"; else echo "  FAIL $check"; failed=1; fi
  done
  return "$failed"
)
if [ "${1:-}" = --fixture-self-test ]; then fixture_self_test; exit $?; fi
if fixture_self_test; then pass 'fixture security failure paths'; else fail 'fixture security failure paths' 'self-test failed'; fi
trap 'stop_private_registry trap' EXIT
start_private_registry || { fail "start private registry" "fixture failed"; exit 1; }
if [ -z "$PRIVATE_SEED_CONFIG" ] && [ ! -e "$PRIVATE_DIR/docker-config" ]; then
  pass 'removed the seeding Docker configuration'
else
  fail 'removed the seeding Docker configuration' 'configuration remains'
fi

expect_eq "anonymous registry access is refused" 401 "$(private_curl -o /dev/null -w '%{http_code}' "$PRIVATE_URL/v2/")"
expect_eq "wrong password is refused" 401 "$(private_curl -u "${PRIVATE_USER}:wrong" -o /dev/null -w '%{http_code}' "$PRIVATE_URL/v2/")"
expect_eq "fixture credential works" 200 "$(private_curl -K "$PRIVATE_CURL_CONFIG" -o /dev/null -w '%{http_code}' "$PRIVATE_URL/v2/")"
if [ "$PRIVATE_A" != "$PRIVATE_B" ]; then pass "fixture images have distinct digests"; else fail "fixture images have distinct digests" "digests match"; fi
for query in 'n=100' 'n=100&last=tag100'; do
  response=$(private_curl -K "$PRIVATE_CURL_CONFIG" -D "$RUN/private-headers" "$PRIVATE_URL/v2/$PRIVATE_TAG_REPO/tags/list?$query")
  if jq -e --arg repo "$PRIVATE_TAG_REPO" '.name == $repo and (.tags | sort) == ([range(1;101) | "tag" + (tostring | if length == 1 then "00" + . elif length == 2 then "0" + . else . end)] | sort)' <<< "$response" >/dev/null; then
    pass "pagination query $query returns all 100 tags"
  else
    fail "pagination query $query returns all 100 tags" "unexpected response"
  fi
  if grep -qi '^link:' "$RUN/private-headers"; then fail "pagination query $query has no continuation" "unexpected Link"; else pass "pagination query $query has no continuation"; fi
done

owner=$PRIVATE_OWNER
PRIVATE_OWNER=foreign
if stop_private_registry; then fail "foreign ownership refuses cleanup" "cleanup succeeded"; else pass "foreign ownership refuses cleanup"; fi
PRIVATE_OWNER=$owner
if stop_private_registry; then pass "removed the owned private registry"; else fail "removed the owned private registry" "cleanup failed"; fi
finish
