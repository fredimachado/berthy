# Shared settings and helpers for the Berthy e2e harness. Source it from a script; never run it.
#
# The harness runs in WSL (distro Ubuntu) against an isolated single-node Swarm daemon. It creates only what carries the
# e2e labels or names below, and it writes only under $E2E_ROOT.

export PATH=/usr/bin:/bin

GO=/var/tmp/berthy-phase1/go/bin/go
DOCKER_BIN=/var/tmp/berthy-phase1/bin/docker
SOCK=/var/tmp/berthy-phase2/docker.sock
# The standalone Compose v2 CLI of the fixture, which the agent runs as its compose_command.
COMPOSE_BIN=/var/tmp/berthy-phase1/cli-plugins/docker-compose

E2E_ROOT=${E2E_ROOT:-$HOME/berthy-e2e/harness}
FIXTURE_ID=$(printf '%s' "$(realpath -m "$E2E_ROOT")" | sha256sum | cut -d' ' -f1)
SRC=$E2E_ROOT/src            # rsynced copy of the repo (native fs)
BIN=$E2E_ROOT/bin/berthy
TG_BIN=$E2E_ROOT/bin/faketelegram
OTLP_BIN=$E2E_ROOT/bin/fakeotlp
HOOK_BIN=$E2E_ROOT/bin/fakewebhook
S3_BIN=$E2E_ROOT/bin/fakes3
TUI_BIN=$E2E_ROOT/bin/tuidriver
MCP_BIN=$E2E_ROOT/bin/mcpdriver
NAMES_BIN=$E2E_ROOT/bin/fixture-names
RUN=$E2E_ROOT/run            # agent config, tokens, state, CLI configs, pids
LOG=$E2E_ROOT/logs
MAN=$E2E_ROOT/manifest       # manifest dir scenarios deploy from
# The environment, project, registry and listen addresses default to the values below. Two runs on the same daemon at once (a
# checkout's own run and another's) set their own, with their own E2E_ROOT, so they never share a Swarm service, a registry
# container or a port. For example: ENVN=e2e9 PROJECT=e2e9 REG=127.0.0.1:15031 REG_NAME=e2e9-registry AGENT_LISTEN=127.0.0.1:19511.
ENVN=${ENVN:-e2e}
PROJECT=${PROJECT:-e2e}
NETWORK=$PROJECT-$ENVN
REG=${REG:-127.0.0.1:15021}          # throwaway unauthenticated registry
REG_NAME=${REG_NAME:-e2e-registry}   # its container name
AGENT_LISTEN=${AGENT_LISTEN:-127.0.0.1:19501}
TG_LISTEN=${TG_LISTEN:-127.0.0.1:18901}
OTLP_LISTEN=${OTLP_LISTEN:-127.0.0.1:18902}
HOOK_LISTEN=${HOOK_LISTEN:-127.0.0.1:18903}   # fake webhook for alerts and heartbeat; per slot: 1895<SLOT>
S3_LISTEN=${S3_LISTEN:-127.0.0.1:18904}       # fake S3 for backups; per slot: 1897<SLOT>
S3_BUCKET=${S3_BUCKET:-e2e-backups}           # the bucket the fake S3 starts with
# The repo is two directories above this file, so the harness works from any checkout, including /mnt/d.
REPO=${REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}

# The tokens that setup.sh wrote. Before setup has run there are none, and the CLI says so.
if [ -f "$RUN/env.sh" ]; then . "$RUN/env.sh"; fi

# d ARGS: the docker CLI against the isolated daemon. DOCKER_CONFIG is the harness's own (empty) config, so the CLI never
# asks the credential helper that the user's Windows-side config names, which does not exist in WSL.
d() {
  if [ -n "${D_TIMEOUT:-}" ]; then
    DOCKER_CONFIG=$E2E_ROOT/docker-config timeout --kill-after=1s "${D_TIMEOUT}s" "$DOCKER_BIN" --host "unix://$SOCK" "$@"
  else
    DOCKER_CONFIG=$E2E_ROOT/docker-config "$DOCKER_BIN" --host "unix://$SOCK" "$@"
  fi
}

# container_image_inventory reads container IDs and image IDs from one Docker snapshot, including stopped containers.
container_image_inventory() {
  local inventory
  inventory=$(curl --noproxy '*' --unix-socket "$SOCK" --fail --silent --show-error --max-time 30 'http://localhost/containers/json?all=1') || return 1
  jq -e 'type == "array" and all(.[]; (.Id | type == "string" and length > 0) and (.ImageID | type == "string" and length > 0))' <<< "$inventory" >/dev/null || return 1
  printf '%s\n' "$inventory"
}

# record_deploy PROJECT ENV [ARGS]: journal names before a deploy can create them, including on interruption.
record_deploy() (
  local project=$1 env=$2 dir=. app= arg kind name tmp pending combined lock existing
  shift 2
  for arg in "$@"; do [ "$arg" = --dry-run ] && return 0; done
  while [ $# -gt 0 ]; do
    case $1 in
      --manifest-dir) shift; [ $# -gt 0 ] || return 0; dir=$1 ;;
      --manifest-dir=*) dir=${1#*=} ;;
      --version|--commit|--request-id|--project) shift; [ $# -gt 0 ] || return 0 ;;
      --*) ;;
      *) [ -n "$app" ] || app=$1 ;;
    esac
    shift
  done
  [ -x "$NAMES_BIN" ] || { echo 'fixture-names is unavailable' >&2; return 1; }
  tmp=$(mktemp "$E2E_ROOT/fixture-names.XXXXXX") || return 1
  pending=$(mktemp "$E2E_ROOT/fixture-pending.XXXXXX") || { rm -f "$tmp"; return 1; }
  combined=$(mktemp "$E2E_ROOT/fixture-combined.XXXXXX") || { rm -f "$tmp" "$pending"; return 1; }
  trap 'rm -f "$tmp" "$pending" "$combined"' EXIT
  if ! "$NAMES_BIN" "$dir" "$project" "$env" "$app" > "$tmp"; then rm -f "$tmp"; return 0; fi
  exec {lock}>>"$E2E_ROOT/fixture-objects.lock" || return 1
  flock -x "$lock" || return 1
  touch "$E2E_ROOT/fixture-objects" || return 1
  while IFS=$'\t' read -r kind name; do
    [ -n "$name" ] || continue
    grep -Fxq -- "$kind"$'\t'"$name" "$E2E_ROOT/fixture-objects" && continue
    grep -Fxq -- "$kind"$'\t'"$name" "$pending" && continue
    case $kind in
      volume)
        existing=$(d volume ls --format '{{.Name}}') || { rm -f "$tmp"; return 1; }
        if printf '%s\n' "$existing" | grep -Fxq -- "$name"; then existing=yes; else existing=; fi
        ;;
      compose)
        existing=$(d ps -aq --filter "label=com.docker.compose.project=$name") || { rm -f "$tmp"; return 1; }
        existing+=$(d network ls -q --filter "label=com.docker.compose.project=$name") || { rm -f "$tmp"; return 1; }
        existing+=$(d volume ls -q --filter "label=com.docker.compose.project=$name") || { rm -f "$tmp"; return 1; }
        ;;
      *) rm -f "$tmp"; return 1 ;;
    esac
    if [ -n "$existing" ]; then
      echo "fixture refuses existing unrecorded $kind $name" >&2
      return 1
    fi
    printf '%s\t%s\n' "$kind" "$name" >> "$pending" || return 1
  done < "$tmp"
  cat "$E2E_ROOT/fixture-objects" "$pending" > "$combined" || return 1
  mv -f "$combined" "$E2E_ROOT/fixture-objects" || return 1
  flock -u "$lock"
  exec {lock}>&-
)

# adm CMD [ARGS]: berthy as the admin token. Flags go right after the command name, so a "-- cmd" tail stays last. It names only
# the environment, so it is right while one project has $ENVN; with a second project that has it, use env_cmd.
adm() { local c=$1; shift; [ "$c" != deploy ] || record_deploy "$PROJECT" "$ENVN" "$@" || return 1; "$BIN" "$c" -e "$ENVN" --config "$RUN/admin.json" "$@"; }

# ci CMD [ARGS]: berthy as the agent token "ci" (scope deploy, agent: true), for approval scenarios. setup.sh makes it
# through the API.
ci() { local c=$1; shift; [ "$c" != deploy ] || record_deploy "$PROJECT" "$ENVN" "$@" || return 1; "$BIN" "$c" -e "$ENVN" --config "$RUN/ci.json" "$@"; }

# api_token NAME [FLAGS]: makes a token through the API as the admin token ("token create"), and prints its value. Nothing
# else is printed, so the caller can capture it.
api_token() {
  local out
  out=$(adm token create "$@" --json) || return 1
  printf '%s\n' "$out" | sed -n 's/^ *"token": "\(bty\.[^"]*\)".*/\1/p'
}

# make_ciadmin: makes the agent admin token "ci-admin" through the API, writes its CLI configuration ciadmin.json and its
# environment variable, and defines cia: berthy as that token. It needs no restart of the agent.
make_ciadmin() {
  local t
  t=$(api_token ci-admin --scope admin --agent) || return 1
  case $t in bty.$ENVN.*) ;; *) return 1 ;; esac
  printf "export BERTHY_CIADMIN_TOKEN='%s'\n" "$t" >> "$RUN/env.sh" || return 1
  export BERTHY_CIADMIN_TOKEN=$t
  printf '{"projects":{"%s":{"environments":{"%s":{"url":"http://%s","token_env":"BERTHY_CIADMIN_TOKEN"}}}}}\n' "$PROJECT" "$ENVN" "$AGENT_LISTEN" > "$RUN/ciadmin.json" || return 1
  cia() { local c=$1; shift; [ "$c" != deploy ] || record_deploy "$PROJECT" "$ENVN" "$@" || return 1; "$BIN" "$c" -e "$ENVN" --config "$RUN/ciadmin.json" "$@"; }
}

# curl_local ARGS: curl to the loopback fakes and the registry, never through a proxy.
curl_local() { curl --noproxy '*' -sS "$@"; }

# owned PIDFILE BINARY: true when PIDFILE holds the pid of a live process that runs BINARY. A stale pid file can name a
# reused pid, so the command line is checked too. A zombie has an empty command line and counts as gone.
owned() {
  local pid cmd
  [ -f "$1" ] || return 1
  pid=$(cat "$1") || return 1
  [ -n "$pid" ] && [ -r "/proc/$pid/cmdline" ] || return 1
  # The process may exit between the test and the read; the redirect is silenced so that race prints nothing.
  cmd=$(tr '\0' ' ' 2>/dev/null < "/proc/$pid/cmdline") || return 1
  case $cmd in
    *"$2"*) return 0 ;;
  esac
  return 1
}

# stop_proc PIDFILE BINARY: asks the process to exit (SIGTERM), waits up to 10 s, then kills it.
stop_proc() {
  local pid i
  if ! owned "$1" "$2"; then
    rm -f "$1"
    return 0
  fi
  pid=$(cat "$1")
  kill -TERM "$pid" 2>/dev/null
  for i in $(seq 1 50); do
    owned "$1" "$2" || break
    sleep 0.2
  done
  if owned "$1" "$2"; then
    kill -KILL "$pid" 2>/dev/null
  fi
  rm -f "$1"
}

# spawn PIDFILE LOGFILE COMMAND [ARGS]: starts COMMAND detached in its own session, appends its output to LOGFILE and
# writes its pid to PIDFILE. The shell execs the command, so the recorded pid is the command's own.
spawn() {
  local pidfile=$1 logfile=$2
  shift 2
  setsid sh -c 'echo $$ > "$1"; shift; exec "$@"' sh "$pidfile" "$@" >>"$logfile" 2>&1 </dev/null &
}

# wait_for WHAT COMMAND [ARGS]: runs COMMAND every 0.25 s for up to 15 s until it succeeds.
wait_for() {
  local what=$1 i
  shift
  for i in $(seq 1 60); do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.25
  done
  echo "$what did not come up (logs in $LOG)" >&2
  return 1
}

# write_agent_config: writes $RUN/agent.json from the fragments in $RUN/environments.d/*.json, one hosted environment each, in
# name order. The agent's own settings are the fixture's. The file is private, as the agent requires.
write_agent_config() {
  local f sep=
  {
    printf '{"listen":"%s","docker_socket":"%s","telegram_api_url":"http://%s","compose_command":["%s"],"environments":[' \
      "$AGENT_LISTEN" "$SOCK" "$TG_LISTEN" "$COMPOSE_BIN"
    for f in "$RUN"/environments.d/*.json; do
      [ -f "$f" ] || continue
      printf '%s' "$sep"
      tr -d '\n' < "$f"
      sep=,
    done
    printf ']}\n'
  } > "$RUN/agent.json.new" && chmod 600 "$RUN/agent.json.new" && mv -f "$RUN/agent.json.new" "$RUN/agent.json"
}

# write_fragment PROJECT ENV STATE_DIR KEY_FILE TOKEN_ENTRY: writes the fragment of one hosted environment of agent.json.
write_fragment() {
  mkdir -p "$RUN/environments.d" || return 1
  printf '{"project":"%s","environment":"%s","state_dir":"%s","key_file":"%s","tokens":[%s]}\n' "$1" "$2" "$3" "$4" "$5" \
    > "$RUN/environments.d/$1-$2.json"
}

# create_environment_network records the exact network before creating it, so interrupted setup can remove it.
create_environment_network() (
  local name=$1 lock listing
  mkdir -p "$E2E_ROOT" || return 1
  exec {lock}>>"$E2E_ROOT/fixture-objects.lock" || return 1
  flock -x "$lock" || return 1
  listing=$(d network ls --format '{{.Name}}') || return 1
  if printf '%s\n' "$listing" | grep -Fxq -- "$name"; then
    echo "fixture refuses existing unrecorded network $name" >&2
    return 1
  fi
  printf 'network\t%s\n' "$name" >> "$E2E_ROOT/fixture-objects" || return 1
  d network create --driver overlay --attachable --label "berthy-e2e.fixture=$FIXTURE_ID" "$name" >/dev/null || return 1
)

# network_call bounds each Docker request by the same cleanup deadline.
network_call() {
  local deadline=$1 remaining
  shift
  remaining=$((deadline - SECONDS))
  [ "$remaining" -gt 0 ] || return 124
  D_TIMEOUT=$remaining d "$@"
}

# start_network_events gives cleanup a child it can always stop and reap.
start_network_events() {
  DOCKER_CONFIG=$E2E_ROOT/docker-config "$DOCKER_BIN" --host "unix://$SOCK" events --since "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --filter type=network --filter "network=$1" --filter event=disconnect --filter event=destroy --format '{{.Action}}' > "$2" 2>> "$3" &
  events_pid=$!
}

# remove_environment_network waits on Docker events so an attached endpoint can detach before a retry.
remove_environment_network() (
  local id=$1 limit=${2:-120} work events_fd events_pid event remaining deadline listing removed last_error= attempt_error status
  work=$(mktemp -d "$E2E_ROOT/network-wait.XXXXXX") || return 1
  mkfifo "$work/events" || { rmdir "$work"; return 1; }
  trap '[ -z "${events_pid:-}" ] || { kill -KILL "$events_pid" 2>/dev/null || :; wait "$events_pid" 2>/dev/null || :; }; rm -f "$work/events" "$work/error"; rmdir "$work"' EXIT
  deadline=${3:-$((SECONDS + limit))}
  exec {events_fd}<>"$work/events"
  start_network_events "$id" "$work/events" "$work/error"
  list_networks() {
    if listing=$(network_call "$deadline" network ls --no-trunc --format '{{.ID}}' 2>"$work/error"); then return 0; else status=$?; fi
    local list_error
    list_error=$(cat "$work/error")
    [ -z "$list_error" ] || last_error=$list_error
    if [ "$status" -eq 124 ] || [ "$status" -eq 137 ]; then
      echo "network $id cleanup timed out: $last_error" >&2
    else
      echo "network $id cleanup could not list networks: $last_error" >&2
    fi
    return 1
  }
  while :; do
    : > "$work/error"
    removed=0
    if network_call "$deadline" network rm "$id" > /dev/null 2>"$work/error"; then
      removed=1
    else
      status=$?
      attempt_error=$(cat "$work/error")
      [ -z "$attempt_error" ] || last_error=$attempt_error
      if [ "$status" -eq 124 ] || [ "$status" -eq 137 ]; then
        echo "network $id cleanup timed out: $last_error" >&2
        return 1
      fi
    fi
    list_networks || return 1
    if ! printf '%s\n' "$listing" | grep -Fxq -- "$id"; then return 0; fi
    if [ "$removed" = 0 ] && ! printf '%s\n' "$last_error" | grep -Eqi 'in use|active endpoints|network( [[:alnum:]_.-]+)? not found|no such network'; then
      echo "network $id removal failed: $last_error" >&2
      return 1
    fi
    while :; do
      remaining=$((deadline - SECONDS))
      if [ "$remaining" -le 0 ] || ! IFS= read -r -t "$remaining" event <&"$events_fd"; then
        echo "network $id cleanup timed out: $last_error" >&2
        return 1
      fi
      if [ "$event" = destroy ] || [ "$event" = disconnect ]; then
        list_networks || return 1
        if ! printf '%s\n' "$listing" | grep -Fxq -- "$id"; then return 0; fi
        break
      fi
    done
  done
)

# env_dir PROJECT ENV: the manifest directory of a hosted environment of the fixture: $MAN for the first, $MAN-PROJECT-ENV for
# one that add_environment made.
env_dir() {
  if [ "$1" = "$PROJECT" ] && [ "$2" = "$ENVN" ]; then
    printf '%s\n' "$MAN"
  else
    printf '%s-%s-%s\n' "$MAN" "$1" "$2"
  fi
}

# env_var PROJECT ENV: the name of the variable that holds the admin token of a hosted environment that add_environment made.
env_var() { printf 'BERTHY_ADMIN_TOKEN_%s' "$(printf '%s_%s' "$1" "$2" | tr 'a-z-' 'A-Z_')"; }

# add_environment PROJECT ENV: adds a hosted environment to the agent and restarts it. It makes a key, a state directory
# $RUN/state-PROJECT-ENV, an admin token (as setup.sh does for the first), the fragment of agent.json, a manifest directory
# $MAN-PROJECT-ENV with the base manifest of that project, and an entry of the CLI configuration $RUN/admin.json. The token is in
# the variable env_var names, and in env.sh.
add_environment() {
  local project=$1 env=$2 var out token entry state
  var=$(env_var "$project" "$env")
  state=$RUN/state-$project-$env
  out=$("$BIN" agent token -e "$env" --name admin --scope admin) || return 1
  token=$(printf '%s\n' "$out" | sed -n 's/^token: //p')
  entry=$(printf '%s\n' "$out" | sed -n '3p')
  case $token in bty.$env.*) ;; *) return 1 ;; esac
  case $entry in \{*) ;; *) return 1 ;; esac
  printf "export %s='%s'\n" "$var" "$token" >> "$RUN/env.sh" || return 1
  export "$var=$token"
  (umask 077 && mkdir -p "$state" && head -c 32 /dev/urandom > "$RUN/key-$project-$env") || return 1
  create_environment_network "$project-$env" || return 1
  write_fragment "$project" "$env" "$state" "$RUN/key-$project-$env" "$entry" || return 1
  mkdir -p "$(env_dir "$project" "$env")" && PROJECT=$project base_manifest > "$(env_dir "$project" "$env")/berthy.yml" || return 1
  "$BIN" context add --project "$project" -e "$env" --url "http://$AGENT_LISTEN" --token-env "$var" --config "$RUN/admin.json" >/dev/null || return 1
  stop_agent
  write_agent_config || return 1
  start_agent
}

# env_cmd PROJECT ENV CMD [ARGS]: berthy as the admin of that hosted environment, run from its manifest directory so the project
# is the manifest's and the command is never ambiguous. Flags go right after the command name, as adm does.
env_cmd() {
  local project=$1 env=$2 c=$3
  shift 3
  (cd "$(env_dir "$project" "$env")" && { [ "$c" != deploy ] || record_deploy "$project" "$env" "$@"; } && "$BIN" "$c" -e "$env" --project "$project" --config "$RUN/admin.json" "$@")
}

# start_agent: starts the agent and waits until /healthz answers, which it does only once every hosted environment has started.
# /healthz needs no token and names no environment. It gives up early when the process has died.
start_agent() {
  local i
  spawn "$RUN/agent.pid" "$LOG/agent.log" "$BIN" agent --config "$RUN/agent.json"
  for i in $(seq 1 120); do
    if curl_local -f -o /dev/null "http://$AGENT_LISTEN/healthz" 2>/dev/null; then
      return 0
    fi
    if [ -s "$RUN/agent.pid" ] && ! owned "$RUN/agent.pid" "$BIN"; then
      break
    fi
    sleep 0.25
  done
  echo "agent did not answer (logs in $LOG/agent.log)" >&2
  return 1
}

stop_agent() { stop_proc "$RUN/agent.pid" "$BIN"; }

# start_hook: starts the fake webhook and waits until it answers. It records to $RUN/hook-requests.jsonl.
start_hook() {
  spawn "$RUN/hook.pid" "$LOG/webhook.log" "$HOOK_BIN" -listen "$HOOK_LISTEN" -requests "$RUN/hook-requests.jsonl" || return 1
  wait_for "fakewebhook" curl_local -f -o /dev/null "http://$HOOK_LISTEN/_fake/requests"
}

stop_hook() { stop_proc "$RUN/hook.pid" "$HOOK_BIN"; }

# start_s3: starts the fake S3 server with the bucket $S3_BUCKET and waits until it answers. Its data is in memory, so each run
# of the fixture starts empty.
start_s3() {
  spawn "$RUN/s3.pid" "$LOG/fakes3.log" "$S3_BIN" -listen "$S3_LISTEN" -bucket "$S3_BUCKET" || return 1
  wait_for "fakes3" curl_local -f -o /dev/null "http://$S3_LISTEN/_fake/objects"
}

stop_s3() { stop_proc "$RUN/s3.pid" "$S3_BIN"; }

# s3_objects PREFIX: the objects of the fake S3 under PREFIX, as JSON.
s3_objects() { curl_local "http://$S3_LISTEN/_fake/objects?prefix=$1"; }

# start_fakes: starts the fake Telegram, the fake OTLP receiver, the fake webhook and the fake S3, and waits until all answer.
start_fakes() {
  spawn "$RUN/tg.pid" "$LOG/telegram.log" "$TG_BIN" -listen "$TG_LISTEN" -token-file "$RUN/tg-token" -messages "$RUN/tg-messages.json" || return 1
  spawn "$RUN/otlp.pid" "$LOG/otlp.log" "$OTLP_BIN" -listen "$OTLP_LISTEN" -summary "$RUN/otlp-summary.json" -expect-authorization-file "$RUN/otlp-auth" || return 1
  wait_for "faketelegram" curl_local -f -o /dev/null "http://$TG_LISTEN/_fake/messages" || return 1
  wait_for "fakeotlp" curl_local -o /dev/null "http://$OTLP_LISTEN/" || return 1
  start_hook || return 1
  start_s3 || return 1
}

stop_fakes() {
  stop_proc "$RUN/tg.pid" "$TG_BIN"
  stop_proc "$RUN/otlp.pid" "$OTLP_BIN"
  stop_hook
  stop_s3
}

# hook_requests: the requests the fake webhook recorded, as one JSON array.
hook_requests() { curl_local "http://$HOOK_LISTEN/_fake/requests"; }

# hook_reset: clears the fake webhook's record.
hook_reset() { curl_local -X POST "http://$HOOK_LISTEN/_fake/reset" >/dev/null; }

# tg_messages: the messages the fake Telegram recorded, as JSON.
tg_messages() { curl_local "http://$TG_LISTEN/_fake/messages"; }

# tg_reset: clears the fake Telegram's record.
tg_reset() { curl_local -X POST "http://$TG_LISTEN/_fake/reset" >/dev/null; }

# service_spec NAME: the Swarm service spec as JSON, for example service_spec "$PROJECT-$ENVN-web-main".
service_spec() { d service inspect --format '{{json .Spec}}' "$1"; }

# write_manifest: writes $MAN/berthy.yml from stdin and removes stale overlays (berthy.<env>.yml).
write_manifest() {
  mkdir -p "$MAN"
  rm -f "$MAN"/berthy.*.yml
  cat > "$MAN/berthy.yml"
}

# base_manifest: prints the default manifest with one image app "web" from the
# e2e registry, and one role "main" with a health check that always passes.
base_manifest() {
  cat <<YML
project: $PROJECT
apps:
  web:
    kind: image
    image: $REG/e2e/web
    roles:
      main:
        health:
          test: ["CMD", "true"]
          interval: 2s
          timeout: 2s
          start_period: 1s
          retries: 3
YML
}

# purge_fixture: stops the agent and the fakes, then removes what this harness made on the daemon: Swarm services,
# containers owned by the hosted environments, the $REG_NAME container, and objects recorded in the fixture's ledger.
purge_fixture() {
  local i v f project env state kind name lock tmp owner listing labels escaped id network_name deadline status lookup_error failed=0
  local -a ids owners
  local volumes="" projects="" networks=""
  stop_agent
  stop_fakes
  owners=("$PROJECT"$'\t'"$ENVN")
  for f in "$RUN"/environments.d/*.json; do
    [ -f "$f" ] || continue
    IFS=$'\t' read -r project env state < <(jq -r 'select((.project | type) == "string" and (.environment | type) == "string" and (.state_dir | type) == "string") | [.project, .environment, .state_dir] | @tsv' "$f" 2>/dev/null) || continue
    if [[ $project =~ ^[a-z0-9][a-z0-9_-]*$ && $env =~ ^[a-z0-9][a-z0-9-]*$ && $state == "$RUN"/* ]]; then
      owners+=("$project"$'\t'"$env")
    fi
  done
  mkdir -p "$E2E_ROOT" || return 1
  exec {lock}>>"$E2E_ROOT/fixture-objects.lock" || return 1
  flock -x "$lock" || return 1
  if [ -f "$E2E_ROOT/fixture-objects" ]; then
    while IFS=$'\t' read -r kind name; do
      case $kind in
        volume) volumes+=$'\n'"$name" ;;
        compose) projects+=$'\n'"$name" ;;
        network) networks+=$'\n'"$name" ;;
        *) failed=1 ;;
      esac
    done < "$E2E_ROOT/fixture-objects"
  fi
  for owner in "${owners[@]}"; do
    IFS=$'\t' read -r project env <<< "$owner"
    listing=$(d service ls -q --filter "label=berthy.project=$project" --filter "label=berthy.environment=$env") || { failed=1; continue; }
    ids=(); [ -z "$listing" ] || mapfile -t ids <<< "$listing"
    if [ ${#ids[@]} -gt 0 ]; then
      d service rm "${ids[@]}" >/dev/null || failed=1
    fi
    for i in $(seq 1 60); do
      listing=$(d service ls -q --filter "label=berthy.project=$project" --filter "label=berthy.environment=$env") || { failed=1; break; }
      ids=(); [ -z "$listing" ] || mapfile -t ids <<< "$listing"
      [ ${#ids[@]} -eq 0 ] && break
      sleep 0.5
    done
    [ ${#ids[@]} -eq 0 ] || failed=1
    for i in $(seq 1 60); do
      listing=$(d ps -aq --filter "label=berthy.project=$project" --filter "label=berthy.environment=$env") || { failed=1; break; }
      ids=(); [ -z "$listing" ] || mapfile -t ids <<< "$listing"
      [ ${#ids[@]} -eq 0 ] && break
      d rm -f "${ids[@]}" >/dev/null 2>&1 || failed=1
      sleep 0.5
    done
    [ ${#ids[@]} -eq 0 ] || failed=1
  done

  d rm -fv "$REG_NAME" >/dev/null 2>&1

  # References under the e2e registry: tags left by an interrupted setup, and the digest references that stay once a tag is
  # removed. They are removed by name, never by image id, so an image that a source tag also names keeps its source tag.
  d images --no-trunc --digests --format '{{.Repository}} {{.Tag}} {{.Digest}}' |
    awk -v p="$REG/e2e/" 'index($1, p) == 1 { if ($2 == "<none>") print $1 "@" $3; else print $1 ":" $2 }' |
    while read -r ref; do d rmi "$ref" >/dev/null 2>&1; done

  # The ledger survives a removed Compose working directory and failed cleanup.
  tmp=$(mktemp "$E2E_ROOT/fixture-cleanup.XXXXXX") || return 1
  while IFS= read -r project; do
    [ -n "$project" ] || continue
    if ! d ps -aq --filter "label=com.docker.compose.project=$project" > "$tmp"; then
      failed=1
    else
      while read -r v; do d rm -f "$v" >/dev/null 2>&1 || failed=1; done < "$tmp"
    fi
    if ! d network ls -q --filter "label=com.docker.compose.project=$project" > "$tmp"; then
      failed=1
    else
      while read -r v; do
        if ! d network rm "$v" >/dev/null 2>&1; then
          listing=$(d network ls -q --filter "label=com.docker.compose.project=$project") || { failed=1; continue; }
          if printf '%s\n' "$listing" | grep -Fxq -- "$v"; then failed=1; fi
        fi
      done < "$tmp"
    fi
    if ! d volume ls -q --filter "label=com.docker.compose.project=$project" > "$tmp"; then
      failed=1
    else
      while read -r v; do
        d volume rm "$v" >/dev/null 2>&1 || { echo "volume $v is still in use and was kept" >&2; failed=1; }
      done < "$tmp"
    fi
  done <<< "$projects"
  while IFS= read -r name; do
    [ -n "$name" ] || continue
    escaped=${name//./\\.}
    if ! d volume ls --filter "name=^${escaped}$" --format '{{.Name}} {{.Labels}}' > "$tmp"; then
      failed=1
    else
      while read -r v labels; do
        [ "$v" = "$name" ] || continue
        project=$(printf '%s\n' "$labels" | tr ',' '\n' | sed -n 's/^com.docker.compose.project=//p')
        if [ -z "$project" ]; then
          d volume rm "$v" >/dev/null 2>&1 || { echo "volume $v is still in use and was kept" >&2; failed=1; }
        fi
      done < "$tmp"
    fi
  done <<< "$volumes"
  while IFS= read -r name; do
    [ -n "$name" ] || continue
    deadline=$((SECONDS + ${1:-120}))
    if listing=$(network_call "$deadline" network ls --no-trunc --format '{{.ID}} {{.Name}}' 2>"$tmp"); then :; else
      echo "network $name lookup failed: $(cat "$tmp")" >&2; failed=1; continue
    fi
    while read -r id network_name; do
      [ "$network_name" = "$name" ] || continue
      if labels=$(network_call "$deadline" network inspect --format '{{index .Labels "berthy-e2e.fixture"}}' "$id" 2>"$tmp"); then :; else
        status=$?
        lookup_error=$(cat "$tmp")
        if [ "$status" -eq 124 ] || [ "$status" -eq 137 ]; then
          echo "network $id lookup timed out: $lookup_error" >&2; failed=1; continue
        fi
        if listing=$(network_call "$deadline" network ls --no-trunc --format '{{.ID}}' 2>"$tmp"); then :; else
          echo "network $id lookup failed: $(cat "$tmp")" >&2; failed=1; continue
        fi
        if printf '%s\n' "$listing" | grep -Fxq -- "$id"; then
          echo "network $id inspection failed: $lookup_error" >&2; failed=1
        fi
        continue
      fi
      [ "$labels" = "$FIXTURE_ID" ] || continue
      remove_environment_network "$id" "${1:-120}" "$deadline" || failed=1
    done <<< "$listing"
  done <<< "$networks"
  rm -f "$tmp"
  [ "$failed" -ne 0 ] || rm -f "$E2E_ROOT/fixture-objects"
  flock -u "$lock"
  exec {lock}>&-
  [ "$failed" -eq 0 ]
}

# Assertions. Each prints "  ok   NAME" or "  FAIL NAME: DETAIL" and counts the failures in E2E_FAILS.
E2E_FAILS=0

pass() { printf '  ok   %s\n' "$1"; }

fail() {
  printf '  FAIL %s: %s\n' "$1" "$2"
  E2E_FAILS=$((E2E_FAILS + 1))
}

# detail_block TEXT: shows the start of TEXT under a failure, so the FAIL line stays on one line.
detail_block() { printf '%s\n' "$1" | head -n 20 | cut -c1-300 | sed 's/^/       | /'; }

# expect_eq NAME WANT GOT
expect_eq() {
  if [ "$2" = "$3" ]; then
    pass "$1"
  else
    fail "$1" "want '$2', got '$3'"
  fi
}

# expect_contains NAME HAYSTACK NEEDLE
expect_contains() {
  if [[ $2 == *"$3"* ]]; then
    pass "$1"
  else
    fail "$1" "does not contain '$3'"
    detail_block "$2"
  fi
}

# expect_not_contains NAME HAYSTACK NEEDLE
expect_not_contains() {
  if [[ $2 != *"$3"* ]]; then
    pass "$1"
  else
    fail "$1" "unexpectedly contains '$3'"
    detail_block "$2"
  fi
}

# expect_code NAME WANT_CODE GOT_CODE [OUTPUT]: OUTPUT, when given, is shown under a failure.
expect_code() {
  if [ "$2" = "$3" ]; then
    pass "$1"
  else
    fail "$1" "exit $3, want $2"
    if [ -n "${4:-}" ]; then
      detail_block "$4"
    fi
  fi
}

# finish: the last line of every scenario. Exits 1 when any assertion failed, else 0.
finish() {
  if [ "$E2E_FAILS" -eq 0 ]; then
    echo "PASS $(basename "$0")"
    exit 0
  fi
  echo "FAIL $(basename "$0"): $E2E_FAILS assertion(s) failed"
  exit 1
}
