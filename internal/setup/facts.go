package setup

import (
	"context"
	"fmt"
	"strings"
)

// Facts is what one probe of the server found. A fact the server could not give is the zero value.
type Facts struct {
	OS, Arch string
	Systemd  bool
	Addrs    []string
	Curl     bool
	Wget     bool

	Docker              bool
	DockerVersion       string // the Engine's version, from the daemon; empty when the daemon is not running
	DockerClientVersion string // the docker client's version, which needs no daemon
	DockerActive        bool
	DockerEnabled       bool

	SwarmState   string // the Swarm's local node state: inactive, active, pending, error or locked; empty when unknown
	SwarmManager bool

	NetworkFound      bool
	NetworkDriver     string
	NetworkScope      string
	NetworkAttachable bool
	NetworkError      string // why the network could not be inspected, when it could not and is not simply missing

	AgentEnv string // the environment of the agent already installed, when there is one
}

// probeScript prints the facts as key=value lines. It only reads, so a dry run can run it. Every command is silenced and
// defaulted, so a missing tool gives an empty value and never a failed probe. Docker is asked only once systemd says it is
// running: the docker command would otherwise start it through its socket.
const probeScript = `set -f
echo "os=$(uname -s 2>/dev/null)"
echo "arch=$(uname -m 2>/dev/null)"
if [ -d /run/systemd/system ]; then echo "systemd=yes"; else echo "systemd="; fi
ips=$(ip -o addr show 2>/dev/null | sed -n 's|^[0-9]*: *[^ ]* *inet6\{0,1\} \([^ /]*\)/.*|\1|p')
[ -n "$ips" ] || ips=$(hostname -I 2>/dev/null | tr ' ' '\n')
for a in $ips; do echo "addr=$a"; done
echo "curl=$(command -v curl 2>/dev/null)"
echo "wget=$(command -v wget 2>/dev/null)"
d=$(command -v docker 2>/dev/null)
echo "docker=$d"
if [ -n "$d" ]; then
  echo "docker_client_version=$(docker --version 2>/dev/null | sed -n 's/^Docker version \([^,]*\),.*/\1/p')"
  active=$(systemctl is-active docker 2>/dev/null)
  echo "docker_active=$active"
  echo "docker_enabled=$(systemctl is-enabled docker 2>/dev/null)"
  if [ "$active" = active ]; then
    t=
    command -v timeout >/dev/null 2>&1 && t="timeout 30"
    echo "docker_version=$($t docker version --format '{{.Server.Version}}' 2>/dev/null)"
    echo "swarm=$($t docker info --format '{{.Swarm.LocalNodeState}} {{.Swarm.ControlAvailable}}' 2>/dev/null)"
    out=$($t docker network inspect -f '{{.Driver}} {{.Scope}} {{.Attachable}}' %s 2>&1)
    if [ $? -eq 0 ]; then
      echo "network=$out"
    else
      case "$out" in
        *"No such network"*|*"not found"*) ;;
        *) echo "network_error=$(echo "$out" | head -n 1)" ;;
      esac
    fi
  fi
fi
f=/etc/berthy/agent.json
[ -r "$f" ] && echo "agent_env=$(sed -n 's/.*"environment"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$f" 2>/dev/null | head -n 1)"
exit 0
`

// Probe reads the facts of the server in one script. network is the shared network to look for.
func Probe(ctx context.Context, r Runner, network string) (*Facts, error) {
	res, err := r.Run(ctx, fmt.Sprintf(probeScript, shellQuote(network)), nil)
	if err != nil {
		return nil, fmt.Errorf("could not read the state of %s: %w", r.Target(), err)
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("could not read the state of %s: %s", r.Target(), failureText(res))
	}
	return ParseFacts(string(res.Stdout)), nil
}

// ParseFacts reads the key=value lines of the probe. Lines it does not know, or cannot split, are ignored.
func ParseFacts(out string) *Facts {
	f := &Facts{}
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "os":
			f.OS = value
		case "arch":
			f.Arch = value
		case "systemd":
			f.Systemd = value == "yes"
		case "addr":
			if value != "" {
				f.Addrs = append(f.Addrs, value)
			}
		case "curl":
			f.Curl = value != ""
		case "wget":
			f.Wget = value != ""
		case "docker":
			f.Docker = value != ""
		case "docker_version":
			f.DockerVersion = value
		case "docker_client_version":
			f.DockerClientVersion = value
		case "docker_active":
			f.DockerActive = value == "active"
		case "docker_enabled":
			f.DockerEnabled = value == "enabled"
		case "swarm":
			fields := strings.Fields(value)
			if len(fields) > 0 {
				f.SwarmState = fields[0]
			}
			f.SwarmManager = len(fields) > 1 && fields[1] == "true"
		case "network":
			if fields := strings.Fields(value); len(fields) >= 3 {
				f.NetworkFound = true
				f.NetworkDriver, f.NetworkScope, f.NetworkAttachable = fields[0], fields[1], fields[2] == "true"
			}
		case "network_error":
			f.NetworkError = value
		case "agent_env":
			f.AgentEnv = value
		}
	}
	return f
}
