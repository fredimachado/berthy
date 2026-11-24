#!/bin/bash
# Sourced by scenarios; credentials and registry storage belong to this harness run.
start_private_registry() {
  local nonce source target digest v tag media
  umask 077
  nonce=$(openssl rand -hex 8) || return 1
  PRIVATE_OWNER="$FIXTURE_ID.$nonce"
  PRIVATE_NAME="${PROJECT}-${ENVN}-private-$nonce"
  PRIVATE_DIR=$(mktemp -d "$RUN/private-registry.XXXXXX") || return 1
  PRIVATE_USER=fixture
  PRIVATE_PASSWORD=$(openssl rand -hex 24) || return 1
  PRIVATE_TAG_REPO="${PROJECT}/${ENVN}-tags-$nonce"
  PRIVATE_IMAGE_REPO="${PROJECT}/${ENVN}-images-$nonce"
  PRIVATE_ID=
  PRIVATE_CREATE_PENDING=
  PRIVATE_SEED_CONFIG=
  PRIVATE_LOCAL_TAG=
  PRIVATE_LOCAL_IMAGE=
  if d container inspect "$PRIVATE_NAME" >/dev/null 2>&1; then
    echo 'private registry name already exists' >&2
    return 1
  fi
  PRIVATE_PASSWORD="$PRIVATE_PASSWORD" python3 -W ignore::DeprecationWarning -c 'import crypt, os; print("fixture:" + crypt.crypt(os.environ["PRIVATE_PASSWORD"], crypt.mksalt(crypt.METHOD_BLOWFISH)))' > "$PRIVATE_DIR/htpasswd" || return 1
  PRIVATE_CREATE_PENDING=1
  PRIVATE_ID=$(d create --name "$PRIVATE_NAME" --label "berthy.e2e.fixture=$PRIVATE_OWNER" \
    --log-driver none -p 127.0.0.1::5000 --tmpfs /var/lib/registry \
    -v "$PRIVATE_DIR:/auth:ro" -e REGISTRY_AUTH=htpasswd \
    -e REGISTRY_AUTH_HTPASSWD_REALM=fixture -e REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd \
    registry:2.8.3) || return 1
  d start "$PRIVATE_ID" >/dev/null || return 1
  [ "$(d exec "$PRIVATE_ID" registry --version)" = 'registry github.com/docker/distribution 2.8.3' ] || return 1
  PRIVATE_PORT=$(d port "$PRIVATE_ID" 5000/tcp) || return 1
  PRIVATE_REG="127.0.0.1:${PRIVATE_PORT##*:}"
  PRIVATE_URL="http://$PRIVATE_REG"
  printf 'user = "%s:%s"\n' "$PRIVATE_USER" "$PRIVATE_PASSWORD" > "$PRIVATE_DIR/curl.conf" || return 1
  PRIVATE_CURL_CONFIG="$PRIVATE_DIR/curl.conf"
  wait_for 'private registry' private_curl -K "$PRIVATE_CURL_CONFIG" -f -o /dev/null "$PRIVATE_URL/v2/" || return 1
  private_seed_config || return 1
  for v in v1 v2; do
    source="127.0.0.1:15001/berthy-phase2/web:$v"
    target="$PRIVATE_REG/$PRIVATE_IMAGE_REPO:$v"
    private_tag_image "$source" "$target" || return 1
    private_seed_d push "$target" >/dev/null || return 1
    digest=$(d image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$target" | sed -n "s#^$PRIVATE_REG/$PRIVATE_IMAGE_REPO@##p" | head -n 1)
    case $digest in sha256:*) ;; *) return 1 ;; esac
    if [ "$v" = v1 ]; then PRIVATE_A=$digest; else PRIVATE_B=$digest; fi
    private_remove_tag || return 1
  done
  # Push one manifest to the second repository, then assign its bytes to the remaining tags.
  target="$PRIVATE_REG/$PRIVATE_TAG_REPO:tag001"
  private_tag_image '127.0.0.1:15001/berthy-phase2/web:v1' "$target" || return 1
  private_seed_d push "$target" >/dev/null || return 1
  private_remove_tag || return 1
  rm -rf -- "$PRIVATE_SEED_CONFIG" || return 1
  PRIVATE_SEED_CONFIG=
  private_curl -f -K "$PRIVATE_CURL_CONFIG" -D "$PRIVATE_DIR/headers" \
    -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json' \
    "$PRIVATE_URL/v2/$PRIVATE_TAG_REPO/manifests/tag001" -o "$PRIVATE_DIR/manifest" || { echo 'reading seed manifest failed' >&2; return 1; }
  media=$(sed -n 's/^[Cc]ontent-[Tt]ype: *\([^;\r]*\).*/\1/p' "$PRIVATE_DIR/headers" | tr -d '\r' | tail -n 1)
  case $media in application/vnd.*+json) ;; *) echo 'unexpected seed manifest type' >&2; return 1 ;; esac
  for v in $(seq 2 100); do
    printf -v tag 'tag%03d' "$v"
    private_curl -f -K "$PRIVATE_CURL_CONFIG" -X PUT -H "Content-Type: $media" \
      --data-binary "@$PRIVATE_DIR/manifest" "$PRIVATE_URL/v2/$PRIVATE_TAG_REPO/manifests/$tag" -o /dev/null || { echo "writing seed manifest $tag failed" >&2; return 1; }
  done
}

private_curl() { curl -q --noproxy '*' -sS "$@"; }

private_seed_config() {
  PRIVATE_SEED_CONFIG="$PRIVATE_DIR/docker-config"
  mkdir "$PRIVATE_SEED_CONFIG" || return 1
  printf '%s:%s' "$PRIVATE_USER" "$PRIVATE_PASSWORD" | base64 -w 0 | \
    jq -R --arg host "$PRIVATE_REG" '{auths: {($host): {auth: .}}}' > "$PRIVATE_SEED_CONFIG/config.json"
}

private_seed_d() {
  if [ -n "${D_TIMEOUT:-}" ]; then
    DOCKER_CONFIG="$PRIVATE_SEED_CONFIG" timeout --kill-after=1s "${D_TIMEOUT}s" "$DOCKER_BIN" --host "unix://$SOCK" "$@"
  else
    DOCKER_CONFIG="$PRIVATE_SEED_CONFIG" "$DOCKER_BIN" --host "unix://$SOCK" "$@"
  fi
}

private_image_inventory() {
  private_curl --unix-socket "$SOCK" --fail --max-time 30 'http://localhost/images/json?all=1'
}

private_tag_image() {
  local inventory absent
  inventory=$(private_image_inventory) || return 1
  printf '%s' "$inventory" | jq -e 'type == "array" and all(.[]; (.Id | type == "string" and length > 0) and ((.RepoTags == null) or (.RepoTags | type == "array" and all(.[]; type == "string"))))' >/dev/null || return 1
  absent=$(printf '%s' "$inventory" | jq -r --arg tag "$2" 'if any(.[]; (.RepoTags // []) | index($tag) != null) then "present" else "absent" end') || return 1
  [ "$absent" = absent ] || return 1
  PRIVATE_LOCAL_IMAGE=$(d image inspect -f '{{.Id}}' "$1") || return 1
  [ -n "$PRIVATE_LOCAL_IMAGE" ] || return 1
  PRIVATE_LOCAL_TAG=$2
  d tag "$1" "$2" || return 1
}

private_remove_tag() {
  [ -n "${PRIVATE_LOCAL_TAG:-}" ] || return 0
  [ "$(d image inspect -f '{{.Id}}' "$PRIVATE_LOCAL_TAG" 2>/dev/null)" = "$PRIVATE_LOCAL_IMAGE" ] || return 1
  d rmi "$PRIVATE_LOCAL_TAG" >/dev/null || return 1
  PRIVATE_LOCAL_TAG=
}

stop_private_registry() {
  local mode=${1:-} target info id label failed=0
  target=${PRIVATE_ID:-}
  if [ -z "$target" ] && [ -n "${PRIVATE_CREATE_PENDING:-}" ]; then target=$PRIVATE_NAME; fi
  if [ -n "$target" ]; then
    info=$(d container inspect -f '{{.Id}} {{index .Config.Labels "berthy.e2e.fixture"}}' "$target" 2>/dev/null) || { info=; failed=1; }
    read -r id label <<< "$info"
    if [ -z "$id" ] || [ "$label" != "$PRIVATE_OWNER" ]; then
      [ "$mode" = trap ] || echo 'private registry ownership mismatch; left untouched' >&2
      failed=1
    elif d rm -f "$id" >/dev/null; then
      PRIVATE_ID= PRIVATE_CREATE_PENDING=
    else
      failed=1
    fi
  fi
  private_remove_tag || failed=1
  if [ -n "${PRIVATE_DIR:-}" ]; then
    if rm -rf -- "$PRIVATE_DIR"; then PRIVATE_DIR= PRIVATE_SEED_CONFIG=; else failed=1; fi
  fi
  return "$failed"
}
