#!/bin/bash
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$(dirname "$0")/../deploy-ux-fixture.sh" || exit 1
trap 'stop_private_registry trap' EXIT
start_private_registry || { fail "start private registry" "fixture failed"; exit 1; }

expect_eq "anonymous registry access is refused" 401 "$(curl_local -o /dev/null -w '%{http_code}' "$PRIVATE_URL/v2/")"
expect_eq "wrong password is refused" 401 "$(curl_local -u "${PRIVATE_USER}:wrong" -o /dev/null -w '%{http_code}' "$PRIVATE_URL/v2/")"
expect_eq "fixture credential works" 200 "$(curl_local -K "$PRIVATE_CURL_CONFIG" -o /dev/null -w '%{http_code}' "$PRIVATE_URL/v2/")"
if [ "$PRIVATE_A" != "$PRIVATE_B" ]; then pass "fixture images have distinct digests"; else fail "fixture images have distinct digests" "digests match"; fi
for query in 'n=100' 'n=100&last=tag100'; do
  response=$(curl_local -K "$PRIVATE_CURL_CONFIG" -D "$RUN/private-headers" "$PRIVATE_URL/v2/$PRIVATE_TAG_REPO/tags/list?$query")
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
