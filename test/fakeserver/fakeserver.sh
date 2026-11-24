#!/usr/bin/env bash
# Disposable "fresh Debian servers" for testing `berthy setup`.
# See README.md. Every docker call goes to the isolated daemon.
set -euo pipefail

export DOCKER_HOST="${FS_DOCKER_HOST:-unix:///var/tmp/berthy-phase2/docker.sock}"
IMAGE="${FS_IMAGE:-berthy-fakeserver:debian12}"
NETWORK="${FS_NETWORK:-berthy-fakeservers}"
SUBNET="${FS_SUBNET:-172.31.250.0/24}"
GATEWAY="${FS_GATEWAY:-172.31.250.1}"
FS_HOME="${FS_HOME:-$HOME/berthy-e2e/p5/home}"
LABEL="berthy.fakeserver=1"
OWNER="${FS_OWNER:-}"
owner_args=() owner_filter=()
if [[ -n "$OWNER" ]]; then
  owner_args=(--label "berthy.fakeserver.owner=$OWNER")
  owner_filter=(--filter "label=berthy.fakeserver.owner=$OWNER")
fi
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

die() { echo "fakeserver: $*" >&2; exit 1; }
d() { docker "$@"; }

# Only resources carrying LABEL are ours. Docker also takes a bare argument as an id
# prefix when no name matches, so no server NAME is passed to docker as it is: a lookup
# goes through cid (anchored name plus label), and the command acts on the id it returns.
cid() {  # NAME -> full id of the labelled container NAME; prints nothing if there is none
  [[ -n "${1:-}" ]] || return 0
  d ps -aq --no-trunc --filter "name=^/${1}\$" --filter "label=$LABEL" "${owner_filter[@]}"
}
need_cid() {  # NAME -> full id of the labelled container NAME, or die
  local id
  id="$(cid "$1")"
  [[ -n "$id" ]] || die "no such server: $1 (no container named $1 with label $LABEL)"
  echo "$id"
}
exists() { [[ -n "$(cid "$1")" ]]; }
running() {
  local id
  id="$(cid "$1")"
  [[ -n "$id" && "$(d container inspect -f '{{.State.Running}}' "$id" 2>/dev/null || true)" == true ]]
}
ip_of() {
  local id
  id="$(cid "$1")"
  [[ -n "$id" ]] || return 0
  d container inspect -f "{{(index .NetworkSettings.Networks \"$NETWORK\").IPAddress}}" "$id" 2>/dev/null
}

need_name() {
  local n="${1:-}"
  [[ "$n" =~ ^[a-z][a-z0-9-]{0,30}$ ]] || die "server name must match [a-z][a-z0-9-]* (got '$n')"
  # Docker would read a name of hex digits alone as an id prefix (see cid).
  if [[ "$n" =~ ^[0-9a-f]+$ ]]; then
    die "server name \"$n\" could be read as a container id; use a name with another letter, e.g. sf1"
  fi
}

ensure_network() {
  d network inspect "$NETWORK" >/dev/null 2>&1 && return 0
  d network create --driver bridge --subnet "$SUBNET" --gateway "$GATEWAY" \
    --label "$LABEL" "$NETWORK" >/dev/null
}

# Lowest free address from .10 upward, so the first server is always .10.
alloc_ip() {
  local prefix="${SUBNET%.*}" used n
  used="$(d network inspect -f '{{range .Containers}}{{.IPv4Address}} {{end}}' "$NETWORK" 2>/dev/null || true)"
  for n in $(seq 10 250); do
    [[ " $used " == *" $prefix.$n/"* ]] || { echo "$prefix.$n"; return; }
  done
  die "no free address in $SUBNET"
}

# --- fixture ssh home -------------------------------------------------------
ensure_home() {
  mkdir -p "$FS_HOME/.ssh"; chmod 700 "$FS_HOME/.ssh"
  [[ -f "$FS_HOME/.ssh/id_ed25519" ]] ||
    ssh-keygen -q -t ed25519 -N '' -C berthy-fakeserver -f "$FS_HOME/.ssh/id_ed25519"
  touch "$FS_HOME/.ssh/config" "$FS_HOME/.ssh/known_hosts"
  chmod 600 "$FS_HOME/.ssh/config"
  # OpenSSH finds ~/.ssh/config through the passwd entry, not $HOME, so $HOME alone
  # cannot redirect it. These wrappers on PATH pass -F, which is the same thing for
  # any program that runs `ssh`/`scp` (such as berthy setup).
  mkdir -p "$FS_HOME/bin"
  local t
  for t in ssh scp; do
    printf '#!/bin/sh\nexec /usr/bin/%s -F "%s" "$@"\n' "$t" "$FS_HOME/.ssh/config" > "$FS_HOME/bin/$t"
    chmod 755 "$FS_HOME/bin/$t"
  done
}

# Replace (or add) the Host block for NAME; managed blocks are fenced by markers.
ssh_config_set() {
  local name="$1" ip="$2" cfg="$FS_HOME/.ssh/config"
  ssh_config_del "$name"
  cat >>"$cfg" <<CFG
# >>> berthy-fakeserver $name
Host $name
  HostName $ip
  User ops
  IdentityFile $FS_HOME/.ssh/id_ed25519
  IdentitiesOnly yes
  UserKnownHostsFile $FS_HOME/.ssh/known_hosts
  StrictHostKeyChecking accept-new
# <<< berthy-fakeserver $name
CFG
}
ssh_config_del() {
  local name="$1" cfg="$FS_HOME/.ssh/config"
  [[ -f "$cfg" ]] || return 0
  sed -i "/^# >>> berthy-fakeserver $name\$/,/^# <<< berthy-fakeserver $name\$/d" "$cfg"
}
forget_host() {
  local name="$1" ip="${2:-}"
  [[ -f "$FS_HOME/.ssh/known_hosts" ]] || return 0
  ssh-keygen -q -R "$name" -f "$FS_HOME/.ssh/known_hosts" >/dev/null 2>&1 || true
  [[ -z "$ip" ]] || ssh-keygen -q -R "$ip" -f "$FS_HOME/.ssh/known_hosts" >/dev/null 2>&1 || true
  rm -f "$FS_HOME/.ssh/known_hosts.old"
}

install_keys() {
  local id="$1" pub; pub="$(cat "$FS_HOME/.ssh/id_ed25519.pub")"
  d exec "$id" sh -c "install -d -m 700 -o ops -g ops /home/ops/.ssh && install -d -m 700 /root/.ssh \
    && printf '%s\n' '$pub' > /home/ops/.ssh/authorized_keys && chown ops:ops /home/ops/.ssh/authorized_keys \
    && chmod 600 /home/ops/.ssh/authorized_keys && printf '%s\n' '$pub' > /root/.ssh/authorized_keys \
    && chmod 600 /root/.ssh/authorized_keys"
}

# --- commands ---------------------------------------------------------------
cmd_build() {
  d build -t "$IMAGE" "$HERE"
  d image inspect -f 'built {{.RepoTags}} {{.Id}}' "$IMAGE"
}

cmd_start() {
  local name="${1:-}" ip="${2:-}" id; need_name "$name"
  d image inspect "$IMAGE" >/dev/null 2>&1 || die "image $IMAGE missing: run '$0 build'"
  ensure_network; ensure_home
  id="$(cid "$name")"
  if [[ -n "$id" ]]; then
    running "$name" || d start "$id" >/dev/null
    ip="$(ip_of "$name")"
  else
    # Never adopt a container or a volume that is not ours, even under the same name.
    [[ -z "$(d ps -aq --no-trunc --filter "name=^/${name}\$")" ]] ||
      die "a container named $name exists without the label $LABEL; refusing to use it"
    local v
    for v in docker containerd; do
      if d volume inspect "$name-$v" >/dev/null 2>&1 && ! volume_labelled "$name-$v"; then
        die "volume $name-$v exists without the label $LABEL; refusing to use it"
      fi
    done
    [[ -n "$ip" ]] || ip="$(alloc_ip)"
    # Docker 29 keeps image layers under /var/lib/containerd; both must be off overlayfs.
    for v in docker containerd; do
      d volume create --label "$LABEL" "${owner_args[@]}" "$name-$v" >/dev/null
      volume_labelled "$name-$v" || die "volume $name-$v belongs to another fixture"
    done
    d run -d --name "$name" --hostname "$name" --label "$LABEL" "${owner_args[@]}" \
      --privileged --cgroupns=private --tmpfs /run --tmpfs /run/lock \
      --network "$NETWORK" --ip "$ip" \
      -v "$name-docker:/var/lib/docker" -v "$name-containerd:/var/lib/containerd" --label "berthy.fakeserver.name=$name" \
      "$IMAGE" >/dev/null
    id="$(cid "$name")"
    [[ -n "$id" ]] || die "$name: created, but not found by its name and label"
    forget_host "$name" "$ip"
  fi
  ssh_config_set "$name" "$ip"

  # systemd must come up before keys can be installed and sshd can answer.
  local state="" i
  for i in $(seq 1 90); do
    state="$(d exec "$id" systemctl is-system-running 2>/dev/null || true)"
    [[ "$state" == running || "$state" == degraded ]] && break
    sleep 1
  done
  [[ "$state" == running || "$state" == degraded ]] || die "$name: systemd never settled (state: ${state:-none})"
  if [[ "$state" == degraded ]]; then
    echo "$name: system is degraded; failed units:" >&2
    d exec "$id" systemctl --failed --no-legend --plain >&2 || true
  fi
  install_keys "$id"
  for i in $(seq 1 30); do
    if "$FS_HOME/bin/ssh" -o BatchMode=yes -o ConnectTimeout=3 "$name" true 2>/dev/null; then
      echo "$name up at $ip (systemd: $state)"; return 0
    fi
    sleep 1
  done
  die "$name: sshd did not answer for ops@$ip"
}

cmd_ip() { need_name "${1:-}"; exists "$1" || die "no such server: $1"; ip_of "$1"; }

cmd_ssh() {
  local name="${1:-}"; need_name "$name"; shift
  [[ $# -gt 0 ]] || set -- true
  exec "$FS_HOME/bin/ssh" -o BatchMode=yes "$name" -- "$@"
}

cmd_exec() {
  local name="${1:-}" id; need_name "$name"; shift
  [[ $# -gt 0 ]] || die "exec NAME CMD..."
  id="$(need_cid "$name")"
  if [[ -t 0 && -t 1 ]]; then d exec -it "$id" "$@"; else d exec -i "$id" "$@"; fi
}

cmd_install_docker() {
  local name="${1:-}" id; need_name "$name"; id="$(need_cid "$name")"
  running "$name" || die "$name is not running"
  d exec "$id" sh -c 'curl -fsSL https://get.docker.com | sh'
  d exec "$id" systemctl enable --now docker
}

cmd_stop() { local name="${1:-}" id; need_name "$name"; id="$(need_cid "$name")"; d stop -t 20 "$id" >/dev/null; }

volume_labelled() {  # NAME -> success when volume NAME exists and carries LABEL
  [[ "$(d volume inspect -f '{{index .Labels "berthy.fakeserver"}}' "$1" 2>/dev/null || true)" == 1 ]] || return 1
  [[ -z "$OWNER" || "$(d volume inspect -f '{{index .Labels "berthy.fakeserver.owner"}}' "$1" 2>/dev/null || true)" == "$OWNER" ]]
}
# A named volume is removed only if it carries LABEL; one that does not is left in place.
rm_volume() {
  local v="$1"
  d volume inspect "$v" >/dev/null 2>&1 || return 0
  if volume_labelled "$v"; then
    d volume rm -f "$v" >/dev/null
  else
    echo "fakeserver: warning: volume $v is not labelled $LABEL; left in place" >&2
  fi
}
# Removes server NAME: its container (by ID, when it has one), its own labelled volumes
# and its ssh entries.
remove_server() {
  local name="$1" id="${2:-}" ip="" owned=0
  [[ -n "$id" ]] && owned=1
  if [[ -n "$OWNER" ]] && { volume_labelled "$name-docker" || volume_labelled "$name-containerd"; }; then owned=1; fi
  if [[ -n "$id" ]]; then
    ip="$(d container inspect -f "{{(index .NetworkSettings.Networks \"$NETWORK\").IPAddress}}" "$id" 2>/dev/null || true)"
    d rm -f -v "$id" >/dev/null
  fi
  rm_volume "$name-docker"
  rm_volume "$name-containerd"
  if [[ ( -z "$OWNER" || "$owned" == 1 ) && -d "$FS_HOME/.ssh" ]]; then ssh_config_del "$name"; forget_host "$name" "$ip"; fi
}

cmd_rm() {
  local name="${1:-}" id; need_name "$name"
  id="$(cid "$name")"
  remove_server "$name" "$id"
}

cmd_rm_all() {
  [[ -z "$OWNER" ]] || die "rm-all is unavailable with an owner"
  local c n
  for c in $(d ps -aq --no-trunc --filter "label=$LABEL"); do
    n="$(d container inspect -f '{{.Name}}' "$c")"
    remove_server "${n#/}" "$c"
  done
  # labelled volumes whose containers are gone now
  for n in $(d volume ls -q --filter "label=$LABEL"); do d volume rm -f "$n" >/dev/null 2>&1 || true; done
  # the network, only if it carries the label
  for n in $(d network ls -q --no-trunc --filter "name=^${NETWORK}\$" --filter "label=$LABEL"); do
    d network rm "$n" >/dev/null 2>&1 || true
  done
}

usage() {
  sed -n '2,3p' "$0" | sed 's/^# \{0,1\}//'
  cat <<U
usage: $0 build | start NAME [IP] | ip NAME | ssh NAME CMD... | exec NAME CMD...
          | install-docker NAME | stop NAME | rm NAME | rm-all
U
  exit 2
}

# Run, the file dispatches the command. Sourced (the checks do this), it only defines
# the functions.
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  sub="${1:-}"; [[ $# -gt 0 ]] && shift || true
  case "$sub" in
    build) cmd_build "$@" ;;
    start) cmd_start "$@" ;;
    ip) cmd_ip "$@" ;;
    ssh) cmd_ssh "$@" ;;
    exec) cmd_exec "$@" ;;
    install-docker) cmd_install_docker "$@" ;;
    stop) cmd_stop "$@" ;;
    rm) cmd_rm "$@" ;;
    rm-all) cmd_rm_all "$@" ;;
    *) usage ;;
  esac
fi
