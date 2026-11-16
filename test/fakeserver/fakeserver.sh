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
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

die() { echo "fakeserver: $*" >&2; exit 1; }
d() { docker "$@"; }

need_name() {
  [[ "${1:-}" =~ ^[a-z][a-z0-9-]{0,30}$ ]] || die "server name must match [a-z][a-z0-9-]* (got '${1:-}')"
}
exists() { d container inspect "$1" >/dev/null 2>&1; }
running() { [[ "$(d container inspect -f '{{.State.Running}}' "$1" 2>/dev/null || true)" == true ]]; }
ip_of() {
  d container inspect -f "{{(index .NetworkSettings.Networks \"$NETWORK\").IPAddress}}" "$1" 2>/dev/null
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
  local name="$1" pub; pub="$(cat "$FS_HOME/.ssh/id_ed25519.pub")"
  d exec "$name" sh -c "install -d -m 700 -o ops -g ops /home/ops/.ssh && install -d -m 700 /root/.ssh \
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
  local name="${1:-}" ip="${2:-}"; need_name "$name"
  d image inspect "$IMAGE" >/dev/null 2>&1 || die "image $IMAGE missing: run '$0 build'"
  ensure_network; ensure_home
  if exists "$name"; then
    running "$name" || d start "$name" >/dev/null
    ip="$(ip_of "$name")"
  else
    [[ -n "$ip" ]] || ip="$(alloc_ip)"
    # Docker 29 keeps image layers under /var/lib/containerd; both must be off overlayfs.
    local v; for v in docker containerd; do d volume create --label "$LABEL" "$name-$v" >/dev/null; done
    d run -d --name "$name" --hostname "$name" --label "$LABEL" \
      --privileged --cgroupns=private --tmpfs /run --tmpfs /run/lock \
      --network "$NETWORK" --ip "$ip" \
      -v "$name-docker:/var/lib/docker" -v "$name-containerd:/var/lib/containerd" --label "berthy.fakeserver.name=$name" \
      "$IMAGE" >/dev/null
    forget_host "$name" "$ip"
  fi
  ssh_config_set "$name" "$ip"

  # systemd must come up before keys can be installed and sshd can answer.
  local state="" i
  for i in $(seq 1 90); do
    state="$(d exec "$name" systemctl is-system-running 2>/dev/null || true)"
    [[ "$state" == running || "$state" == degraded ]] && break
    sleep 1
  done
  [[ "$state" == running || "$state" == degraded ]] || die "$name: systemd never settled (state: ${state:-none})"
  if [[ "$state" == degraded ]]; then
    echo "$name: system is degraded; failed units:" >&2
    d exec "$name" systemctl --failed --no-legend --plain >&2 || true
  fi
  install_keys "$name"
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
  local name="${1:-}"; need_name "$name"; shift
  [[ $# -gt 0 ]] || die "exec NAME CMD..."
  if [[ -t 0 && -t 1 ]]; then d exec -it "$name" "$@"; else d exec -i "$name" "$@"; fi
}

cmd_install_docker() {
  local name="${1:-}"; need_name "$name"; running "$name" || die "$name is not running"
  d exec "$name" sh -c 'curl -fsSL https://get.docker.com | sh'
  d exec "$name" systemctl enable --now docker
}

cmd_stop() { need_name "${1:-}"; d stop -t 20 "$1" >/dev/null; }

cmd_rm() {
  local name="${1:-}"; need_name "$name"
  local ip=""; exists "$name" && ip="$(ip_of "$name" || true)"
  d rm -f -v "$name" >/dev/null 2>&1 || true
  d volume rm -f "$name-docker" "$name-containerd" >/dev/null 2>&1 || true
  [[ -d "$FS_HOME/.ssh" ]] && { ssh_config_del "$name"; forget_host "$name" "$ip"; }
  true
}

cmd_rm_all() {
  local c n
  for c in $(d ps -aq --filter "label=$LABEL"); do
    n="$(d container inspect -f '{{.Name}}' "$c")"; cmd_rm "${n#/}"
  done
  # volumes (labelled at creation) whose container is already gone
  for n in $(d volume ls -q --filter "label=$LABEL"); do d volume rm -f "$n" >/dev/null 2>&1 || true; done
  d network rm "$NETWORK" >/dev/null 2>&1 || true
}

usage() {
  sed -n '2,3p' "$0" | sed 's/^# \{0,1\}//'
  cat <<U
usage: $0 build | start NAME [IP] | ip NAME | ssh NAME CMD... | exec NAME CMD...
          | install-docker NAME | stop NAME | rm NAME | rm-all
U
  exit 2
}

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
