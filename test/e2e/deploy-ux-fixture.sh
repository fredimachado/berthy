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
  PRIVATE_TAG_REPO="${PROJECT}/${ENVN}-tags"
  PRIVATE_IMAGE_REPO="${PROJECT}/${ENVN}-images"
  PRIVATE_ID=
  PRIVATE_LOCAL_TAG=
  PRIVATE_LOCAL_IMAGE=
  if d container inspect "$PRIVATE_NAME" >/dev/null 2>&1; then
    echo 'private registry name already exists' >&2
    return 1
  fi
  PRIVATE_PASSWORD="$PRIVATE_PASSWORD" python3 -W ignore::DeprecationWarning -c 'import crypt, os; print("fixture:" + crypt.crypt(os.environ["PRIVATE_PASSWORD"], crypt.mksalt(crypt.METHOD_BLOWFISH)))' > "$PRIVATE_DIR/htpasswd" || return 1
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
  wait_for 'private registry' curl_local -K "$PRIVATE_CURL_CONFIG" -f -o /dev/null "$PRIVATE_URL/v2/" || return 1
  printf '%s' "$PRIVATE_PASSWORD" | d login "$PRIVATE_REG" --username "$PRIVATE_USER" --password-stdin >/dev/null 2>&1 || return 1
  for v in v1 v2; do
    source="127.0.0.1:15001/berthy-phase2/web:$v"
    target="$PRIVATE_REG/$PRIVATE_IMAGE_REPO:$v"
    private_tag_image "$source" "$target" || return 1
    d push "$target" >/dev/null || return 1
    digest=$(d image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$target" | sed -n "s#^$PRIVATE_REG/$PRIVATE_IMAGE_REPO@##p" | head -n 1)
    case $digest in sha256:*) ;; *) return 1 ;; esac
    if [ "$v" = v1 ]; then PRIVATE_A=$digest; else PRIVATE_B=$digest; fi
    private_remove_tag || return 1
  done
  # Push one manifest to the second repository, then assign its bytes to the remaining tags.
  target="$PRIVATE_REG/$PRIVATE_TAG_REPO:tag001"
  private_tag_image '127.0.0.1:15001/berthy-phase2/web:v1' "$target" || return 1
  d push "$target" >/dev/null || return 1
  private_remove_tag || return 1
  d logout "$PRIVATE_REG" >/dev/null 2>&1 || return 1
  curl_local -f -K "$PRIVATE_CURL_CONFIG" -D "$PRIVATE_DIR/headers" \
    -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json' \
    "$PRIVATE_URL/v2/$PRIVATE_TAG_REPO/manifests/tag001" -o "$PRIVATE_DIR/manifest" || { echo 'reading seed manifest failed' >&2; return 1; }
  media=$(sed -n 's/^[Cc]ontent-[Tt]ype: *\([^;\r]*\).*/\1/p' "$PRIVATE_DIR/headers" | tr -d '\r' | tail -n 1)
  case $media in application/vnd.*+json) ;; *) echo 'unexpected seed manifest type' >&2; return 1 ;; esac
  for v in $(seq 2 100); do
    printf -v tag 'tag%03d' "$v"
    curl_local -f -K "$PRIVATE_CURL_CONFIG" -X PUT -H "Content-Type: $media" \
      --data-binary "@$PRIVATE_DIR/manifest" "$PRIVATE_URL/v2/$PRIVATE_TAG_REPO/manifests/$tag" -o /dev/null || { echo "writing seed manifest $tag failed" >&2; return 1; }
  done
}

private_tag_image() {
  if d image inspect "$2" >/dev/null 2>&1; then return 1; fi
  PRIVATE_LOCAL_IMAGE=$(d image inspect -f '{{.Id}}' "$1") || return 1
  d tag "$1" "$2" || return 1
  PRIVATE_LOCAL_TAG=$2
}

private_remove_tag() {
  [ -n "${PRIVATE_LOCAL_TAG:-}" ] || return 0
  [ "$(d image inspect -f '{{.Id}}' "$PRIVATE_LOCAL_TAG" 2>/dev/null)" = "$PRIVATE_LOCAL_IMAGE" ] || return 1
  d rmi "$PRIVATE_LOCAL_TAG" >/dev/null || return 1
  PRIVATE_LOCAL_TAG=
}

stop_private_registry() {
  local mode=${1:-} label
  if [ -n "${PRIVATE_ID:-}" ]; then
    label=$(d container inspect -f '{{index .Config.Labels "berthy.e2e.fixture"}}' "$PRIVATE_ID" 2>/dev/null) || return 1
    if [ "$label" != "$PRIVATE_OWNER" ]; then
      [ "$mode" = trap ] || echo 'private registry ownership mismatch; left untouched' >&2
      return 1
    fi
    d rm -f "$PRIVATE_ID" >/dev/null || return 1
    PRIVATE_ID=
  fi
  private_remove_tag || return 1
  if [ -n "${PRIVATE_REG:-}" ]; then d logout "$PRIVATE_REG" >/dev/null 2>&1 || return 1; fi
  if [ -n "${PRIVATE_DIR:-}" ]; then rm -rf -- "$PRIVATE_DIR" || return 1; PRIVATE_DIR=; fi
}
