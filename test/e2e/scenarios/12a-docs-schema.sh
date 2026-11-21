#!/bin/bash
# 12a-docs-schema proves the generated manifest reference and the JSON Schema, the way a person and an editor use them.
# `berthy docs` prints the reference with no agent, no manifest and no -e. `berthy config schema` prints a draft-07 schema, and
# `--overlay` one for an overlay. With python3, jsonschema (draft 7) and pyyaml, the schema accepts the manifests the harness
# and the examples use, including one that sets Compose, aliases, logging, env.secret mapping, alerts with deploys and
# telemetry, and it refuses a typo and a value outside an enum. `config validate` refuses the same two, so the schema and the
# validator agree. An editor completing keys cannot be driven here: the schema check stands in for it.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"

WORK=$RUN/12a
rm -rf "$WORK"
mkdir -p "$WORK"
cd "$WORK" || exit 1

# --- docs: local, with no agent, no manifest and no -e.
out=$("$BIN" docs 2>&1); code=$?
expect_code "docs prints the reference" 0 "$code" "$out"
for key in project network protected apps.\<name\> retention.keep deploys min_version; do
  expect_contains "docs mentions $key" "$out" "$key"
done

out=$("$BIN" docs apps 2>&1); code=$?
expect_code "docs apps prints the apps section" 0 "$code" "$out"
expect_contains "docs apps lists deploy.retain" "$out" "deploy.retain"
expect_contains "docs apps lists env.secret" "$out" "env.secret"
expect_not_contains "docs apps does not list a backups key" "$out" "retention.keep"

out=$("$BIN" docs backups 2>&1); code=$?
expect_code "docs backups prints the backups section" 0 "$code" "$out"
expect_contains "docs backups lists retention.keep" "$out" "retention.keep"
expect_not_contains "docs backups does not list an apps key" "$out" "deploy.retain"

out=$("$BIN" docs nope 2>&1); code=$?
expect_code "docs refuses an unknown section" 2 "$code" "$out"
expect_contains "the error lists the sections" "$out" "backups"

# --- the schema.
"$BIN" config schema > schema.json 2> schema.err; code=$?
expect_code "config schema needs no -e" 0 "$code" "$(cat schema.err)"
"$BIN" config schema --overlay > overlay.json 2> overlay.err; code=$?
expect_code "config schema --overlay" 0 "$code" "$(cat overlay.err)"
expect_contains "the schema is draft-07" "$(cat schema.json)" '"$schema": "http://json-schema.org/draft-07/schema#"'

# check FILE SCHEMA: exits 0 when the YAML file is valid by the schema, and 1 with the reasons when it is not.
cat > "$WORK/check.py" <<'PY'
import json, sys
import jsonschema, yaml
schema = json.load(open(sys.argv[2]))
jsonschema.Draft7Validator.check_schema(schema)
doc = yaml.safe_load(open(sys.argv[1]))
errors = sorted(jsonschema.Draft7Validator(schema).iter_errors(doc), key=lambda e: list(e.path))
for e in errors[:5]:
    print("/".join(str(p) for p in e.path) + ": " + e.message[:200])
sys.exit(1 if errors else 0)
PY
check() { python3 "$WORK/check.py" "$1" "$2" 2>&1; }

if ! python3 -c 'import jsonschema, yaml' 2>/dev/null; then
  fail "python3 with jsonschema and pyyaml" "not installed in this WSL distro"
  finish
fi

# validate_with NAME SCHEMA FILE: the schema accepts the file.
accepts() {
  local out code
  out=$(check "$3" "$2"); code=$?
  expect_code "$1" 0 "$code" "$out"
}
rejects() {
  local out code
  out=$(check "$3" "$2"); code=$?
  expect_code "$1" 1 "$code" "$out"
}

# The harness's own manifest.
base_manifest > base.yml
accepts "the harness manifest is valid by the schema" schema.json base.yml

# A manifest that sets one of everything the recent keys added.
cat > rich.yml <<YML
project: $PROJECT
network: $NETWORK
protected: true
min_version: 0.9.0
registries:
  ghcr.io:
    username: acme
    password_secret: GHCR_TOKEN
apps:
  web:
    kind: image
    image: $REG/e2e/web
    depends_on: [db]
    roles:
      main:
        service: $PROJECT-$ENVN-web-main
        command: ["serve"]
        replicas: 2
        port: 8080
        health:
          test: ["CMD", "true"]
          interval: 10s
          timeout: 5s
          start_period: 1s
          start_interval: 1s
          retries: 3
        volumes: ["uploads:/data:ro"]
        resources:
          cpu: "0.5"
          memory: 512M
        hostnames: [web.example.com]
        logging:
          driver: json-file
          options:
            max-size: 10m
            max-file: "3"
    env:
      plain:
        LOG_LEVEL: info
      secret:
        - API_KEY
        - DB_URL: DATABASE_URL
    deploy:
      deploy_timeout: 120s
      drain_timeout: 30s
      start_attempts: 3
      retain: 5
    hooks:
      pre_deploy:
        - backup: main
        - run: ["true"]
          timeout: 1m
          retries: 1
      post_deploy:
        - run: ["true"]
    migrations:
      version: ["migrate", "version"]
      apply: ["migrate", "up"]
      revert: ["migrate", "down-to", "{target}"]
      timeout: 5m
      retries: 1
  db:
    kind: compose
    file: deploy/db.yml
    project_name: $PROJECT-db
    stateful: true
    env:
      secret:
        - DB_PASSWORD
jobs:
  cleanup:
    app: web
    role: main
    schedule: "30 2 * * *"
    timeout: 15m
    command: ["clean"]
  dump:
    app: db
    service: postgres
    command: ["dump"]
backups:
  main:
    app: db
    volume: data
    schedule: "0 1 * * *"
    destination: offsite
    prefix: db/data
    before: dump
    retention:
      keep: 14
  weekly:
    app: db
    volume: data
    destination: offsite
    retention:
      keep: all
destinations:
  offsite:
    type: s3
    endpoint: https://s3.example.com
    region: eu-central-1
    bucket: acme-backups
    access_key_secret: S3_KEY
    secret_key_secret: S3_SECRET
state_backup:
  destination: offsite
  prefix: berthy-state/prod
  schedule: "17 3 * * *"
  keep: 14
telemetry:
  otlp_endpoint: http://10.0.0.6:4318
  headers_secret: OTLP_HEADERS
probes:
  site:
    http: https://web.example.com/health
    expect_status: [200, 204]
    interval: 30s
    timeout: 5s
    failures: 3
  database:
    tcp: 10.0.0.5:5432
  inner:
    service:
      app: web
      role: main
      port: 8080
      path: /health
peers:
  dev: http://10.0.0.4:7710
alerts:
  reminder: 4h
  deploys: all
  telegram:
    bot_token_secret: TG_TOKEN
    chat_id: "-1001"
  webhook:
    url_secret: HOOK_URL
    headers_secret: HOOK_HEADERS
  heartbeat:
    url_secret: BEAT_URL
    interval: 1m
ingress:
  cloudflare:
    account_id: abc123
    api_token_secret: CF_TOKEN
    image: cloudflare/cloudflared:2026.11.0
    replicas: 1
    tunnel: $PROJECT-prod
    dns: true
aliases:
  weblogs: logs web --follow
  shell: exec web -- sh
YML
accepts "a manifest with Compose, aliases, logging, secret mapping, alerts and telemetry is valid by the schema" schema.json rich.yml
mkdir -p rich
cp rich.yml rich/berthy.yml
out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$WORK/rich" 2>&1); code=$?
expect_code "config validate accepts the same manifest" 0 "$code" "$out"

# The examples. The overlays are partial manifests: they are valid by the overlay schema, and not by the manifest schema.
EX=$REPO/examples/todo
accepts "examples/todo/berthy.yml is valid by the schema" schema.json "$EX/berthy.yml"
for overlay in "$EX"/berthy.*.yml; do
  accepts "$(basename "$overlay") is valid by the overlay schema" overlay.json "$overlay"
  rejects "$(basename "$overlay") is not a whole manifest" schema.json "$overlay"
done
for env in dev prod; do
  out=$("$BIN" config validate -e "$env" --manifest-dir "$EX" 2>&1); code=$?
  expect_code "config validate accepts the example for $env" 0 "$code" "$out"
done

# A typo of a key: apps.web.rolez.
mkdir -p typo
sed 's/^    roles:/    rolez:/' base.yml > typo/berthy.yml
rejects "the schema refuses apps.web.rolez" schema.json typo/berthy.yml
out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$WORK/typo" 2>&1); code=$?
expect_code "config validate refuses apps.web.rolez" 1 "$code" "$out"

# A value outside an enum: alerts.deploys.
mkdir -p enum
{
  cat base.yml
  cat <<YML
alerts:
  deploys: sometimes
  telegram:
    bot_token_secret: TG_TOKEN
    chat_id: "1"
YML
} > enum/berthy.yml
rejects "the schema refuses alerts.deploys: sometimes" schema.json enum/berthy.yml
out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$WORK/enum" 2>&1); code=$?
expect_code "config validate refuses alerts.deploys: sometimes" 1 "$code" "$out"
sed -i 's/sometimes/all/' enum/berthy.yml
accepts "the schema accepts alerts.deploys: all" schema.json enum/berthy.yml
out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$WORK/enum" 2>&1); code=$?
expect_code "config validate accepts alerts.deploys: all" 0 "$code" "$out"

finish
