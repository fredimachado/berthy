#!/bin/bash
# Three hosted environments keep their workloads, state and retained images separate.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"
P2=$PROJECT-two
EB=${ENVN}b
add_environment "$P2" "$ENVN" || exit 1
add_environment "$PROJECT" "$EB" || exit 1

# The shared source v1 also runs outside this fixture. Make a distinct v1 so the host-wide check is meaningful.
printf 'FROM %s/e2e/web@%s\nLABEL berthy-e2e-version=v1-projects\nSTOPSIGNAL SIGTERM\n' "$REG" "$WEB_V1_DIGEST" > "$RUN/Dockerfile-v1"
d build -t "$REG/e2e/web:v1" -f "$RUN/Dockerfile-v1" "$RUN" >/dev/null || exit 1
d push "$REG/e2e/web:v1" >/dev/null || exit 1
WEB_V1_DIGEST=$(d image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$REG/e2e/web:v1" | sed -n "s#^$REG/e2e/web@##p" | head -n1)
d rmi "$REG/e2e/web:v1" >/dev/null || exit 1
# Give v3 a distinct digest, so neither current nor previous service spec holds v1.
printf 'FROM %s/e2e/web@%s\nLABEL berthy-e2e-version=v3\nSTOPSIGNAL SIGTERM\n' "$REG" "$WEB_V2_DIGEST" > "$RUN/Dockerfile-v3"
d build -t "$REG/e2e/web:v3" -f "$RUN/Dockerfile-v3" "$RUN" >/dev/null || exit 1
d push "$REG/e2e/web:v3" >/dev/null || exit 1
for pair in "$PROJECT $ENVN" "$P2 $ENVN" "$PROJECT $EB"; do
  read -r p e <<< "$pair"
  dir=$(env_dir "$p" "$e")
  cat > "$dir/berthy.yml" <<YML
project: $p
apps:
  web:
    kind: image
    image: $REG/e2e/web
    roles:
      main:
        volumes: ["data:/data"]
        health:
          test: ["CMD", "true"]
          interval: 2s
          timeout: 2s
          retries: 3
  db:
    kind: compose
    file: compose.yml
jobs:
  hold:
    app: web
    timeout: 5m
    command: ["sh", "-c", "trap 'exit 1' TERM; touch /data/ready; while [ ! -e /data/go ]; do sleep 1 & wait \$!; done"]
YML
  cat > "$dir/compose.yml" <<YML
services:
  db:
    image: $REG/e2e/web@$WEB_V2_DIGEST
    entrypoint: ["sleep", "3600"]
    volumes: ["pgdata:/data"]
volumes:
  pgdata: {}
YML
  for app in web db; do
    args=(); [ "$app" = web ] && args=(--version v1)
    out=$(env_cmd "$p" "$e" deploy "$app" "${args[@]}" 2>&1); code=$?
    expect_code "deploy $p/$e $app" 0 "$code" "$out"
  done
done
# Workload names and storage are scoped to the pair.
for pair in "$PROJECT $ENVN" "$P2 $ENVN" "$PROJECT $EB"; do
  read -r p e <<< "$pair"
  d service inspect "$p-$e-web-main" >/dev/null 2>&1; expect_code "service $p/$e" 0 "$?"
  d network inspect "$p-$e" >/dev/null 2>&1; expect_code "network $p/$e" 0 "$?"
  d volume inspect "${p}-${e}_data" >/dev/null 2>&1; expect_code "volume $p/$e" 0 "$?"
  ids=$(d ps -q --filter "label=com.docker.compose.project=$p-$e-db")
  [ -n "$ids" ] && pass "Compose $p/$e" || fail "Compose $p/$e" "no container"
  out=$(env_cmd "$p" "$e" ps 2>&1); code=$?
  expect_code "ps $p/$e" 0 "$code" "$out"
  expect_contains "ps shows web" "$out" web
  expect_contains "ps shows db" "$out" db
  out=$(env_cmd "$p" "$e" stats --json 2>&1); code=$?
  expect_code "stats $p/$e" 0 "$code" "$out"
  for other in "$PROJECT $ENVN" "$P2 $ENVN" "$PROJECT $EB"; do
    read -r op oe <<< "$other"
    [ "$p/$e" = "$op/$oe" ] && continue
    for id in $(d ps -q --filter "label=com.docker.swarm.service.name=$op-$oe-web-main"); do
      expect_not_contains "stats $p/$e excludes $op/$oe" "$out" "$id"
    done
  done
  out=$(env_cmd "$p" "$e" history web 2>&1); expect_contains "history $p/$e" "$out" "$WEB_V1_DIGEST"
done
out=$(env_cmd "$PROJECT" "$EB" logs "$P2-$ENVN-web-main" 2>&1); code=$?
expect_code 'another environment service logs are refused' 1 "$code" "$out"
out=$(env_cmd "$PROJECT" "$ENVN" run web -- sh -c 'echo x > /data/f' 2>&1); expect_code 'write first volume' 0 "$?" "$out"
out=$(env_cmd "$PROJECT" "$EB" run web -- sh -c 'test ! -e /data/f' 2>&1); expect_code 'second volume is separate' 0 "$?" "$out"
printf secret | env_cmd "$PROJECT" "$ENVN" secret set A_ONLY >/dev/null 2>&1
for pair in "$P2 $ENVN" "$PROJECT $EB"; do
  read -r p e <<< "$pair"
  out=$(env_cmd "$p" "$e" secret list 2>&1); expect_not_contains "secret isolated in $p/$e" "$out" A_ONLY
done
for pair in "$P2 $ENVN" "$PROJECT $EB"; do
  read -r p e <<< "$pair"
  status=$(curl_local -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $BERTHY_ADMIN_TOKEN" "http://$AGENT_LISTEN/v1/projects/$p/environments/$e/agent")
  expect_eq "token isolated from $p/$e" 401 "$status"
done
out=$(cd "$E2E_ROOT" && "$BIN" ps -e "$ENVN" --config "$RUN/admin.json" 2>&1); code=$?
expect_code 'ambiguous environment refused' 1 "$code" "$out"
expect_contains 'ambiguity names projects' "$out" 'exists in several projects'
out=$(cd "$E2E_ROOT" && "$BIN" ps -e "$ENVN" --project "$P2" --config "$RUN/admin.json" 2>&1); expect_code 'explicit project resolves ambiguity' 0 "$?" "$out"
out=$(env_cmd "$PROJECT" "$ENVN" deploy web --version v1 --manifest-dir "$(env_dir "$P2" "$ENVN")" 2>&1); code=$?
expect_code 'other manifest refused' 2 "$code" "$out"
expect_contains 'mismatch names project' "$out" 'is for project'
first_db=$(d ps -q --filter "label=com.docker.compose.project=$PROJECT-$ENVN-db")
# A second app in the same environment cannot reuse a Compose project either.
dir=$(env_dir "$PROJECT" "$ENVN")
cp "$dir/berthy.yml" "$dir/original.yml"
python3 - "$dir/berthy.yml" "$PROJECT-$ENVN-db" <<'PY'
import sys
p, name = sys.argv[1:]
s = open(p).read().replace('jobs:\n', '  duplicate:\n    kind: compose\n    file: compose.yml\n    project_name: '+name+'\njobs:\n')
open(p, 'w').write(s)
PY
out=$(env_cmd "$PROJECT" "$ENVN" deploy duplicate 2>&1); code=$?
expect_code 'duplicate Compose project in one environment refused' 1 "$code" "$out"
expect_contains 'duplicate refusal names both apps' "$out" "Compose project $PROJECT-$ENVN-db is shared by apps db and duplicate"
mv "$dir/original.yml" "$dir/berthy.yml"
# A claimed Compose project and a claimed volume cannot be used by another environment.
dir=$(env_dir "$P2" "$ENVN")
cp "$dir/berthy.yml" "$dir/original.yml"
sed -i "/file: compose.yml/a\\    project_name: $PROJECT-$ENVN-db" "$dir/berthy.yml"
out=$(env_cmd "$P2" "$ENVN" deploy db 2>&1); code=$?
expect_code 'claimed Compose project refused' 1 "$code" "$out"
expect_contains 'Compose refusal names owner' "$out" "name $PROJECT-$ENVN-db is taken by another project or environment on this server"
mv "$dir/original.yml" "$dir/berthy.yml"
sed -i "s/pgdata: {}/pgdata: {name: ${PROJECT}-${ENVN}-db_pgdata}/" "$dir/compose.yml"
out=$(env_cmd "$P2" "$ENVN" deploy db 2>&1); code=$?
expect_code 'claimed Compose volume refused' 1 "$code" "$out"
expect_contains 'volume refusal names owner' "$out" "name $PROJECT-$ENVN-db_pgdata is taken by another project or environment on this server"
expect_eq 'first Compose container survives refused claims' "$first_db" "$(d ps -q --filter "label=com.docker.compose.project=$PROJECT-$ENVN-db")"
# A kept Compose volume remains claimed after removal.
out=$(env_cmd "$PROJECT" "$EB" remove db 2>&1); expect_code 'remove db but keep volume' 0 "$?" "$out"
sed -i "s/${PROJECT}-${ENVN}-db_pgdata/${PROJECT}-${EB}-db_pgdata/" "$dir/compose.yml"
out=$(env_cmd "$P2" "$ENVN" deploy db 2>&1); code=$?
expect_code 'kept volume refused' 1 "$code" "$out"
expect_contains 'kept volume refusal names owner' "$out" "name $PROJECT-$EB-db_pgdata is taken by another project or environment on this server"
out=$(env_cmd "$PROJECT" "$EB" deploy db 2>&1); expect_code 'redeploy db after kept-volume check' 0 "$?" "$out"
# A graceful agent restart keeps workload containers and interrupts the job in its own environment.
ids=$(for pair in "$PROJECT $ENVN" "$P2 $ENVN" "$PROJECT $EB"; do read -r p e <<< "$pair"; d ps -q --filter "label=com.docker.swarm.service.name=$p-$e-web-main"; d ps -q --filter "label=com.docker.compose.project=$p-$e-db"; done)
env_cmd "$PROJECT" "$EB" job run hold > "$RUN/hold.out" 2>&1 & jobpid=$!
ready=1
for i in $(seq 1 120); do
  out=$(env_cmd "$PROJECT" "$EB" exec web -- test -e /data/ready 2>&1); ready=$?
  [ "$ready" -eq 0 ] && break
  sleep 0.5
done
expect_code 'job installed its TERM handler' 0 "$ready" "$out"
stop_agent
wait "$jobpid" || true
start_agent || exit 1
for id in $ids; do state=$(d inspect -f '{{.State.Running}}' "$id" 2>/dev/null); expect_eq "container $id survives restart" true "$state"; done
out=$(env_cmd "$PROJECT" "$EB" jobs 2>&1); expect_contains 'job interrupted on graceful stop' "$out" interrupted
expect_contains 'job reports shutdown' "$out" 'agent stopped during the run'
for pair in "$PROJECT $ENVN" "$P2 $ENVN"; do
  read -r p e <<< "$pair"
  out=$(env_cmd "$p" "$e" jobs 2>&1); expect_not_contains "other jobs unaffected $p/$e" "$out" interrupted
done
out=$(env_cmd "$PROJECT" "$EB" exec web -- rm -f /data/ready 2>&1); expect_code 'clear readiness marker' 0 "$?" "$out"
# Move both survivors twice before destroying the only environment that pulled v1.
for pair in "$P2 $ENVN" "$PROJECT $EB"; do
  read -r p e <<< "$pair"
  for version in v2 v3; do
    out=$(env_cmd "$p" "$e" deploy web --version "$version" 2>&1); code=$?
    expect_code "deploy $p/$e $version" 0 "$code" "$out"
  done
  spec=$(d service inspect --format '{{json .Spec}} {{json .PreviousSpec}}' "$p-$e-web-main")
  expect_not_contains "service specs $p/$e do not hold v1" "$spec" "$WEB_V1_DIGEST"
done
# A running job in the surviving environment must outlive another environment's destroy.
env_cmd "$PROJECT" "$EB" job run hold > "$RUN/hold-after.out" 2>&1 & jobpid=$!
ready=1
for i in $(seq 1 120); do
  out=$(env_cmd "$PROJECT" "$EB" exec web -- test -e /data/ready 2>&1); ready=$?
  [ "$ready" -eq 0 ] && break
  sleep 0.5
done
expect_code 'second hold job is ready' 0 "$ready" "$out"
# Stop the last v1 workload without removing its release before checking Docker references.
out=$(env_cmd "$PROJECT" "$ENVN" scale web --role main --replicas 0 2>&1); expect_code 'scale first web to zero' 0 "$?" "$out"
for id in $(d ps -q); do image=$(d inspect -f '{{.Config.Image}}' "$id"); expect_not_contains "container $id does not run v1" "$image" "$WEB_V1_DIGEST"; done
out=$(env_cmd "$PROJECT" "$ENVN" destroy --confirm "$ENVN" 2>&1); expect_code 'destroy first environment' 0 "$?" "$out"
d image inspect "$REG/e2e/web@$WEB_V1_DIGEST" >/dev/null 2>&1; expect_code 'retained v1 image remains' 0 "$?"
history=$(env_cmd "$PROJECT" "$EB" history web --json 2>&1)
release=$(printf '%s' "$history" | python3 -c 'import json,sys; x=json.load(sys.stdin); print(next(r["id"] for r in x if "'"$WEB_V1_DIGEST"'" in str(r)))')
out=$(env_cmd "$PROJECT" "$EB" rollback web --to "$release" 2>&1); expect_code 'rollback to retained v1' 0 "$?" "$out"
for pair in "$P2 $ENVN" "$PROJECT $EB"; do
  read -r p e <<< "$pair"
  out=$(env_cmd "$p" "$e" ps 2>&1); expect_code "ps survives destroy $p/$e" 0 "$?" "$out"
  out=$(env_cmd "$p" "$e" history web 2>&1); expect_code "history survives destroy $p/$e" 0 "$?" "$out"
  d network inspect "$p-$e" >/dev/null 2>&1; expect_code "network survives destroy $p/$e" 0 "$?"
  d volume inspect "${p}-${e}_data" >/dev/null 2>&1; expect_code "volume survives destroy $p/$e" 0 "$?"
done
out=$(env_cmd "$PROJECT" "$EB" run web -- touch /data/go 2>&1); expect_code 'finish surviving job' 0 "$?" "$out"
wait "$jobpid"; expect_code 'surviving job succeeds' 0 "$?" "$(cat "$RUN/hold-after.out")"
expect_contains 'job reports success' "$(cat "$RUN/hold-after.out")" 'status: succeeded'
finish
