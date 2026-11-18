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
SRC=$E2E_ROOT/src            # rsynced copy of the repo (native fs)
BIN=$E2E_ROOT/bin/berthy
TG_BIN=$E2E_ROOT/bin/faketelegram
OTLP_BIN=$E2E_ROOT/bin/fakeotlp
TUI_BIN=$E2E_ROOT/bin/tuidriver
RUN=$E2E_ROOT/run            # agent config, tokens, state, CLI configs, pids
LOG=$E2E_ROOT/logs
MAN=$E2E_ROOT/manifest       # manifest dir scenarios deploy from
# The environment, project, registry and listen addresses default to the values below. Two runs on the same daemon at once (a
# checkout's own run and another's) set their own, with their own E2E_ROOT, so they never share a Swarm service, a registry
# container or a port. For example: ENVN=e2e9 PROJECT=e2e9 REG=127.0.0.1:15031 REG_NAME=e2e9-registry AGENT_LISTEN=127.0.0.1:19511.
ENVN=${ENVN:-e2e}
PROJECT=${PROJECT:-e2e}
NETWORK=berthy-phase2-lab
REG=${REG:-127.0.0.1:15021}          # throwaway unauthenticated registry
REG_NAME=${REG_NAME:-e2e-registry}   # its container name
AGENT_LISTEN=${AGENT_LISTEN:-127.0.0.1:19501}
TG_LISTEN=${TG_LISTEN:-127.0.0.1:18901}
OTLP_LISTEN=${OTLP_LISTEN:-127.0.0.1:18902}
# The repo is two directories above this file, so the harness works from any checkout, including /mnt/d.
REPO=${REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}

# The tokens that setup.sh wrote. Before setup has run there are none, and the CLI says so.
if [ -f "$RUN/env.sh" ]; then . "$RUN/env.sh"; fi

# d ARGS: the docker CLI against the isolated daemon. DOCKER_CONFIG is the harness's own (empty) config, so the CLI never
# asks the credential helper that the user's Windows-side config names, which does not exist in WSL.
d() { DOCKER_CONFIG=$E2E_ROOT/docker-config "$DOCKER_BIN" --host "unix://$SOCK" "$@"; }

# adm CMD [ARGS]: berthy as the admin token. Flags go right after the command name, so a "-- cmd" tail stays last.
adm() { local c=$1; shift; "$BIN" "$c" -e "$ENVN" --config "$RUN/admin.json" "$@"; }

# ci CMD [ARGS]: berthy as the agent token "ci" (scope deploy, agent: true), for approval scenarios. setup.sh makes it
# through the API.
ci() { local c=$1; shift; "$BIN" "$c" -e "$ENVN" --config "$RUN/ci.json" "$@"; }

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
  printf '{"environments":{"%s":{"url":"http://%s","token_env":"BERTHY_CIADMIN_TOKEN"}}}\n' "$ENVN" "$AGENT_LISTEN" > "$RUN/ciadmin.json" || return 1
  cia() { local c=$1; shift; "$BIN" "$c" -e "$ENVN" --config "$RUN/ciadmin.json" "$@"; }
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

# start_agent: starts the agent and waits until it answers agent-info. It gives up early when the process has died.
start_agent() {
  local i
  spawn "$RUN/agent.pid" "$LOG/agent.log" "$BIN" agent --config "$RUN/agent.json"
  for i in $(seq 1 120); do
    if adm agent-info >/dev/null 2>&1; then
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

# start_fakes: starts the fake Telegram and the fake OTLP receiver, and waits until both answer.
start_fakes() {
  spawn "$RUN/tg.pid" "$LOG/telegram.log" "$TG_BIN" -listen "$TG_LISTEN" -token-file "$RUN/tg-token" -messages "$RUN/tg-messages.json" || return 1
  spawn "$RUN/otlp.pid" "$LOG/otlp.log" "$OTLP_BIN" -listen "$OTLP_LISTEN" -summary "$RUN/otlp-summary.json" -expect-authorization-file "$RUN/otlp-auth" || return 1
  wait_for "faketelegram" curl_local -f -o /dev/null "http://$TG_LISTEN/_fake/messages" || return 1
  wait_for "fakeotlp" curl_local -o /dev/null "http://$OTLP_LISTEN/" || return 1
}

stop_fakes() {
  stop_proc "$RUN/tg.pid" "$TG_BIN"
  stop_proc "$RUN/otlp.pid" "$OTLP_BIN"
}

# tg_messages: the messages the fake Telegram recorded, as JSON.
tg_messages() { curl_local "http://$TG_LISTEN/_fake/messages"; }

# tg_reset: clears the fake Telegram's record.
tg_reset() { curl_local -X POST "http://$TG_LISTEN/_fake/reset" >/dev/null; }

# service_spec NAME: the Swarm service spec as JSON, for example service_spec e2e-web-main.
service_spec() { d service inspect --format '{{json .Spec}}' "$1"; }

# write_manifest: writes $MAN/berthy.yml from stdin and removes stale overlays (berthy.<env>.yml).
write_manifest() {
  mkdir -p "$MAN"
  rm -f "$MAN"/berthy.*.yml
  cat > "$MAN/berthy.yml"
}

# base_manifest: prints the default manifest. Project e2e, network berthy-phase2-lab, one image app "web" from the
# e2e registry, and one role "main" with a health check that always passes.
base_manifest() {
  cat <<YML
project: $PROJECT
network: $NETWORK
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
# containers and volumes labelled berthy.environment=$ENVN, the $REG_NAME container, and volumes named ${PROJECT}_* or in a
# compose project $PROJECT or $PROJECT-*. Nothing else on the daemon matches these filters.
purge_fixture() {
  local i v
  local -a ids
  stop_agent
  stop_fakes

  mapfile -t ids < <(d service ls -q --filter label=berthy.environment=$ENVN)
  if [ ${#ids[@]} -gt 0 ]; then
    d service rm "${ids[@]}" >/dev/null
  fi
  for i in $(seq 1 60); do
    mapfile -t ids < <(d service ls -q --filter label=berthy.environment=$ENVN)
    [ ${#ids[@]} -eq 0 ] && break
    sleep 0.5
  done

  for i in $(seq 1 60); do
    mapfile -t ids < <(d ps -aq --filter label=berthy.environment=$ENVN)
    [ ${#ids[@]} -eq 0 ] && break
    d rm -f "${ids[@]}" >/dev/null 2>&1
    sleep 0.5
  done

  d rm -fv "$REG_NAME" >/dev/null 2>&1

  # References under the e2e registry: tags left by an interrupted setup, and the digest references that stay once a tag is
  # removed. They are removed by name, never by image id, so an image that a source tag also names keeps its source tag.
  d images --no-trunc --digests --format '{{.Repository}} {{.Tag}} {{.Digest}}' |
    awk -v p="$REG/e2e/" 'index($1, p) == 1 { if ($2 == "<none>") print $1 "@" $3; else print $1 ":" $2 }' |
    while read -r ref; do d rmi "$ref" >/dev/null 2>&1; done

  # Compose projects of the harness are named $PROJECT-* (app <name>). Berthy labels nothing on them, so their containers and
  # networks are found by the Compose project label, and only the $PROJECT-* prefix is removed.
  d ps -a --filter label=com.docker.compose.project --format '{{.ID}} {{.Label "com.docker.compose.project"}}' |
    awk -v p="$PROJECT" 'index($2, p "-") == 1 { print $1 }' |
    while read -r c; do d rm -f "$c" >/dev/null 2>&1; done
  d network ls --filter label=com.docker.compose.project -q |
    while read -r n; do
      case $(d network inspect --format '{{index .Labels "com.docker.compose.project"}}' "$n" 2>/dev/null) in
        "$PROJECT"-*) d network rm "$n" >/dev/null 2>&1 || echo "network $n is still in use and was kept" >&2 ;;
      esac
    done

  d volume ls --format '{{.Name}} {{.Labels}}' |
    awk -v p="$PROJECT" '
      { hit = index($1, p "_") == 1
        n = split($2, labels, ",")
        for (i = 1; i <= n; i++) {
          if (labels[i] == "com.docker.compose.project=" p || index(labels[i], "com.docker.compose.project=" p "-") == 1) hit = 1
        }
        if (hit) print $1 }' |
    while read -r v; do
      d volume rm "$v" >/dev/null 2>&1 || echo "volume $v is still in use and was kept" >&2
    done
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
