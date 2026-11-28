#!/bin/bash
# Install over SSH with Docker present and neither curl nor wget on the server.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
export FS_HOME="$RUN/setup-health-home" FS_DOCKER_HOST="unix://$SOCK" DOCKER_CONFIG="$E2E_ROOT/docker-config"
export HOME="$FS_HOME"
mkdir -p "$FS_HOME/bin" "$RUN/setup-empty" || exit 1
ln -sf "$DOCKER_BIN" "$FS_HOME/bin/docker" || exit 1
export PATH="$FS_HOME/bin:/usr/bin:/bin"
fixture="$REPO/test/fakeserver/fakeserver.sh"
private=$(mktemp -d /dev/shm/berthy-setup-health.XXXXXXXX) || exit 1
trap 'rm -rf "$private"' EXIT
scope=$(printf '%s' "$PROJECT/$ENVN" | sha256sum | cut -c1-8)
nonce=$(printf '%s' "$private" | sha256sum | cut -c1-16)
server="h${scope}-${nonce}"
export FS_OWNER="$nonce"

die() { echo "setup health fixture: $*" >&2; exit 1; }
existing=$(d ps -aq --no-trunc --filter "name=^/${server}\$") || exit 1
[ -z "$existing" ] || die "container name already in use"
volumes=$(d volume ls -q) || exit 1
for volume in "$server-docker" "$server-containerd"; do
  if grep -Fxq -- "$volume" <<<"$volumes"; then die "volume name already in use"; fi
done
blocked=0
unblock_health() {
  if [ "$blocked" -eq 1 ]; then
    remote iptables -w -D INPUT "${rule[@]}" || return
    blocked=0
  fi
}
cleanup() {
  unblock_health || echo "could not remove fixture health rule" >&2
  bash "$fixture" rm "$server" >/dev/null 2>&1 || echo "could not remove setup health fixture" >&2
  rm -rf "$private"
}
trap cleanup EXIT
bash "$fixture" start "$server" >/dev/null || die "could not start server"
ip=$(bash "$fixture" ip "$server") || exit 1

remote() { bash "$fixture" exec "$server" "$@"; }
ssh_server() { ssh -o BatchMode=yes -T -- "ops@$server" "$1"; }
if [ -n "${FS_IMAGE:-}" ]; then
  remote systemctl start docker || die "could not start Docker in $FS_IMAGE"
else
  bash "$fixture" install-docker "$server" >"$private/install.log" 2>&1 || die "could not install Docker"
fi
ssh_server 'sudo -n sh -c "docker version"' >/dev/null || die "Docker daemon unavailable over SSH"

remote sh -ec '
  hidden=$(mktemp -d /root/setup-health-tools.XXXXXXXX)
  for tool in curl wget; do
    while path=$(command -v "$tool"); do
      mv -- "$path" "$(mktemp "$hidden/$tool.XXXXXXXX")"
    done
  done
' || die "could not move fetch tools"
ssh_server 'if command -v curl || command -v wget; then exit 1; fi
sudo -n sh -c "! command -v curl && ! command -v wget"' || die "fetch tools still resolve or SSH failed"
pass "Docker answers over SSH and neither fetch tool resolves"

cfg="$private/cli.json"
setup=("$BIN" setup --project "$PROJECT" --host "ops@$server" \
  --listen "$ip:7710" --manifest-dir "$RUN/setup-empty" --config "$cfg" \
  --no-install-docker --binary "$BIN" --json)
run_setup() {
  local report=$1
  shift
  "${setup[@]}" "$@" </dev/null >"$private/$report.json" 2>"$private/$report.stderr"
}
operator_health() { curl --noproxy '*' --max-time 30 -sS -o /dev/null -w '%{http_code}' "http://$ip:7710/healthz"; }
run_setup initial -e "$ENVN"
code=$?
expect_code "setup without curl or wget" 0 "$code"
if [ "$code" -ne 0 ]; then
  # The report can contain an admin token. Print only status fields and the fixed failure message.
  jq -r '.steps[] | "\(.step): \(.status)"' "$private/initial.json"
  if jq -e 'any(.steps[]; .step == "service" and (.detail | contains("the agent did not come up and is left stopped")))' \
    "$private/initial.json" >/dev/null; then
    echo "  service: the agent did not come up and is left stopped"
  fi
fi
if jq -e 'any(.steps[]; .step == "service" and .status == "changed")' "$private/initial.json" >/dev/null; then
  pass "setup starts the service"
else
  fail "setup starts the service" "service step did not report a successful start"
fi
if jq -e 'any(.steps[]; .step == "verify" and .status == "ok")' "$private/initial.json" >/dev/null; then
  pass "setup verifies the agent"
else
  fail "setup verifies the agent" "verify step did not succeed"
fi
active=$(remote systemctl is-active berthy) || true
expect_eq "berthy.service is active" active "$active"
http_code=$(operator_health) || true
expect_eq "agent answers /healthz from the operator" 200 "$http_code"
if [ "$E2E_FAILS" -ne 0 ]; then finish; fi

cluster_dir=/var/lib/berthy/cluster
cluster_layout() {
  remote cat /etc/berthy/agent.json | jq -e --arg dir "$cluster_dir" '
    .cluster == {state_dir: ($dir + "/state"), key_file: ($dir + "/key")}' >/dev/null
}
cluster_identity() { remote stat -c '%d:%i' "$cluster_dir/state/state.db" "$cluster_dir/key"; }
if cluster_layout; then
  pass "setup without alert flags configures the cluster paths"
  expect_eq "the cluster directory and key are private" $'700 berthy:berthy\n600 berthy:berthy' \
    "$(remote stat -c '%a %U:%G' "$cluster_dir/state" "$cluster_dir/key")"
  env_key="/var/lib/berthy/environments/$PROJECT/$ENVN/key"
  remote cmp -s "$env_key" "$cluster_dir/key"; code=$?
  expect_eq "the cluster key differs from the environment key" 1 "$code"
  cluster_before=$(cluster_identity) || die "setup did not open the cluster database"
else
  fail "setup without alert flags configures the cluster paths" "missing default cluster state and key"
  finish
fi

token=$(jq -er '.token | strings' "$private/initial.json") || die "initial setup did not return a token"
case $token in bty."$PROJECT"."$ENVN".?*) ;; *) die "unexpected token shape";; esac
token_env=$(jq -er --arg p "$PROJECT" --arg e "$ENVN" '.projects[$p].environments[$e].token_env | strings' "$cfg") ||
  die "missing token_env"
expected_env=$(printf 'BERTHY_%s_%s_TOKEN' "$PROJECT" "$ENVN" | tr '[:lower:]-' '[:upper:]_')
if [[ ! $token_env =~ ^[A-Za-z_][A-Za-z0-9_]*$ || $token_env != "$expected_env" ]]; then die "unexpected token_env"; fi
export "$token_env=$token"
unset token

operator_ip=$(ip -4 route get "$ip" | awk '{for (i=1; i<NF; i++) if ($i=="src") print $(i+1)}') || die "operator route unavailable"
iface=$(remote ip -o -4 addr show to "$ip/32" | awk '{print $2}') || die "fixture interface unavailable"
[[ $operator_ip =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ && $iface =~ ^[A-Za-z0-9_.-]+$ ]] || die "could not identify the operator bridge route"
rule=(-i "$iface" -s "$operator_ip/32" -d "$ip/32" -p tcp --dport 7710 -j REJECT --reject-with tcp-reset)
remote iptables -w -I INPUT "${rule[@]}" || die "could not block operator HTTP"
blocked=1
if operator_health >/dev/null 2>&1; then die "operator can still reach the agent"; fi
ssh_server true || die "operator cannot reach SSH"
remote /usr/local/bin/berthy agent setup-probe health --listen "$ip:7710" --timeout 3s ||
  die "the agent does not answer inside the fixture"
pass "operator HTTP is blocked while SSH and fixture-local health work"

run_setup doctor -e "$ENVN"
expect_code "setup with doctor fallback" 0 "$?"
if jq -e 'any(.steps[]; .step == "verify" and .status == "ok" and (.detail | contains("checked on the server")))' \
  "$private/doctor.json" >/dev/null; then
  pass "authenticated doctor checked on the server"
else
  fail "authenticated doctor checked on the server" "missing successful server verification"
fi

unset "$token_env"
run_setup tokenless -e "$ENVN"
expect_code "setup without an admin token" 0 "$?"
if jq -e '(.token == null) and any(.steps[]; .step == "token" and .status == "ok") and
  any(.steps[]; .step == "verify" and .status == "ok" and
  .detail == "the agent answers; run berthy doctor with an admin token to check it fully")' "$private/tokenless.json" >/dev/null; then
  pass "setup verified /healthz on the server without a token"
else
  fail "setup verified /healthz on the server without a token" "missing successful tokenless verification"
fi
unblock_health || die "could not restore operator HTTP"
if [ "$E2E_FAILS" -ne 0 ]; then finish; fi

second_env="$ENVN-next"
run_setup second-plan -e "$second_env" --dry-run --verbose
expect_code "plan setup of a second environment" 0 "$?"
if jq -e '[.steps[] | select(.step == "service") | .commands[]] |
  any(contains("$(date +%s) + 150 ))")) and any(contains("--on-active=360s "))' "$private/second-plan.json" >/dev/null; then
  pass "two environments get a 150-second wait and 360-second guard"
else
  fail "two environments get a 150-second wait and 360-second guard" "planned service scripts have the wrong budgets"
fi

run_setup second -e "$second_env"
expect_code "set up the second environment" 0 "$?"
expect_eq "adding an environment keeps the cluster database and key" "$cluster_before" "$(cluster_identity)"
check_scope() {
  local env_name=$1 setup_report=$2
  jq -er '.token | strings' "$private/$setup_report.json" |
    "$BIN" agent setup-probe doctor --listen "$ip:7710" --scope "$PROJECT/$env_name" |
    jq -e 'any(.checks[]; .name == "agent" and .ok)' >/dev/null
}
for pair in "$ENVN:initial" "$second_env:second"; do
  env_name=${pair%:*}
  if check_scope "$env_name" "${pair#*:}"; then
    pass "the agent answers for $PROJECT/$env_name"
  else
    fail "the agent answers for $PROJECT/$env_name" "scoped doctor failed"
  fi
done
finish
