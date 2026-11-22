package setup

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strconv"
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

	// What the agent needs on the server. Hashes are SHA-256 in hex; an empty one means the file is not there.
	BinSHA          string // /usr/local/bin/berthy
	UserExists      bool   // the berthy system user
	UserGroups      []string
	UnitSHA         string // /etc/systemd/system/berthy.service
	ServiceActive   bool
	ServiceEnabled  bool
	DataDirStat     string // /var/lib/berthy: "OWNER:GROUP MODE"; empty when it does not exist
	AgentConfig     bool   // /etc/berthy/agent.json exists
	AgentConfigStat string // "OWNER:GROUP MODE"
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
if [ -f "$f" ]; then
  echo "agent_config=yes"
  echo "agent_config_stat=$(stat -c '%%U:%%G %%a' "$f" 2>/dev/null)"
fi
b=/usr/local/bin/berthy
[ -f "$b" ] && echo "bin_sha=$(sha256sum "$b" 2>/dev/null | cut -d' ' -f1)"
echo "user=$(id -u berthy 2>/dev/null)"
echo "user_groups=$(id -nG berthy 2>/dev/null)"
u=/etc/systemd/system/berthy.service
[ -f "$u" ] && echo "unit_sha=$(sha256sum "$u" 2>/dev/null | cut -d' ' -f1)"
echo "service_active=$(systemctl is-active berthy 2>/dev/null)"
echo "service_enabled=$(systemctl is-enabled berthy 2>/dev/null)"
d=/var/lib/berthy
[ -d "$d" ] && echo "data_dir_stat=$(stat -c '%%U:%%G %%a' "$d" 2>/dev/null)"
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
		case "bin_sha":
			f.BinSHA = value
		case "user":
			f.UserExists = value != ""
		case "user_groups":
			f.UserGroups = strings.Fields(value)
		case "unit_sha":
			f.UnitSHA = value
		case "service_active":
			f.ServiceActive = value == "active"
		case "service_enabled":
			f.ServiceEnabled = value == "enabled"
		case "data_dir_stat":
			f.DataDirStat = value
		case "agent_config":
			f.AgentConfig = value == "yes"
		case "agent_config_stat":
			f.AgentConfigStat = value
		}
	}
	return f
}

// PathFacts describe one hosted environment's key and state directory.
type PathFacts struct {
	KeyExists                     bool
	KeySHA, KeyStat, StateDirStat string
	StateDB, StateForeign         bool
	ParentStats                   map[string]string
}

func managedParents(stateDir, keyFile string) []string {
	seen := map[string]bool{}
	for _, name := range []string{stateDir, keyFile} {
		for dir := path.Dir(path.Clean(name)); strings.HasPrefix(dir, dataDir+"/"); dir = path.Dir(dir) {
			seen[dir] = true
		}
	}
	parents := make([]string, 0, len(seen))
	for dir := range seen {
		parents = append(parents, dir)
	}
	slices.Sort(parents)
	return parents
}

func probePaths(ctx context.Context, r Runner, stateDir, keyFile string) (PathFacts, error) {
	script := `# hosted path probe
s=%s
k=%s
[ -d "$s" ] && echo "state_dir_stat=$(stat -c '%%U:%%G %%a' "$s" 2>/dev/null)"
[ -e "$s/state.db" ] && echo "state_db=yes"
if [ -d "$s" ]; then
  bu=$(id -u berthy 2>/dev/null)
  if [ -z "$bu" ]; then
    [ -n "$(find "$s" -mindepth 1 2>/dev/null | head -n 1)" ] && echo "state_foreign=yes"
  elif [ -n "$(find "$s" ! -uid "$bu" 2>/dev/null | head -n 1)" ]; then
    echo "state_foreign=yes"
  fi
fi
if [ -f "$k" ]; then
  echo "key_sha=$(sha256sum "$k" 2>/dev/null | cut -d' ' -f1)"
  echo "key_stat=$(stat -c '%%s %%U:%%G %%a' "$k" 2>/dev/null)"
fi
`
	command := fmt.Sprintf(script, shellQuote(stateDir), shellQuote(keyFile))
	parents := managedParents(stateDir, keyFile)
	for i, dir := range parents {
		command += fmt.Sprintf("echo \"parent_%d=$(stat -c '%%U:%%G %%a' %s 2>/dev/null)\"\n", i, shellQuote(dir))
	}
	command += "exit 0\n"
	res, err := r.Run(ctx, command, nil)
	if err != nil {
		return PathFacts{}, fmt.Errorf("reading hosted environment paths: %w", err)
	}
	if res.Code != 0 {
		return PathFacts{}, fmt.Errorf("reading hosted environment paths: %s", failureText(res))
	}
	f := PathFacts{ParentStats: make(map[string]string)}
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "key_sha":
			f.KeyExists = true
			f.KeySHA = v
		case "key_stat":
			f.KeyExists = true
			f.KeyStat = v
		case "state_dir_stat":
			f.StateDirStat = v
		case "state_db":
			f.StateDB = v == "yes"
		case "state_foreign":
			f.StateForeign = v == "yes"
		default:
			if strings.HasPrefix(k, "parent_") {
				if i, err := strconv.Atoi(strings.TrimPrefix(k, "parent_")); err == nil && i >= 0 && i < len(parents) {
					f.ParentStats[parents[i]] = v
				}
			}
		}
	}
	return f, nil
}
