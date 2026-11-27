package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var swarmNetworkID = regexp.MustCompile(`^[a-z0-9]{25}$`)
var swarmServiceRef = regexp.MustCompile(`^[a-z0-9]{12}([a-z0-9]{13})?$`)

type removalNetwork struct {
	id   string
	used bool
}

func removalDockerReady(ctx context.Context, r Runner, socket string) bool {
	script := "# verify removal Docker service\nset -eu\n"
	switch socket {
	case dockerSocket, "/run/docker.sock":
		script += "systemctl is-active --quiet docker\n"
	default:
		if !removalPathOK(socket) {
			return false
		}
		// The listening Unix socket must be open in dockerd itself. A systemd socket alone can start Docker on connect.
		script += "socket=" + shellQuote(socket) + "\n" + `
canonical=$(realpath -e -- "$socket")
[ -S "$canonical" ]
while read -r num ref proto flags type state inode address; do
  [ "$flags" = 00010000 ] && [ "$type" = 0001 ] && [ "$state" = 01 ] || continue
  [ "$address" = "$socket" ] || [ "$address" = "$canonical" ] || continue
  for comm in /proc/[0-9]*/comm; do
    [ "$(cat "$comm" 2>/dev/null)" = dockerd ] || continue
    for fd in "${comm%/comm}"/fd/*; do
      if [ "$(readlink -- "$fd" 2>/dev/null)" = "socket:[$inode]" ]; then exit 0; fi
    done
  done
done < /proc/net/unix
exit 1
`
	}
	res, err := r.Run(ctx, script, nil)
	return err == nil && res.Code == 0 && len(res.Stderr) == 0 && len(res.Stdout) == 0
}

func removalDockerCommand(ctx context.Context, r Runner, socket, name string) (string, error) {
	if socket == "" {
		socket = dockerSocket
	}
	if !removalDockerReady(ctx, r, socket) {
		return "", fmt.Errorf("cannot establish the disposition of network %s because its Docker daemon could not be verified as running", name)
	}
	return "timeout 30 docker --host " + shellQuote("unix://"+socket) + " ", nil
}

func inspectRemovalNetwork(ctx context.Context, r Runner, docker, name string, expected *string) (removalNetwork, error) {
	read := func(command string) (string, bool) {
		res, err := r.Run(ctx, docker+command, nil)
		return string(res.Stdout), err == nil && res.Code == 0 && len(res.Stderr) == 0 && len(res.Stdout) < maxCaptured
	}
	list, ok := read("network ls --no-trunc --filter " + shellQuote("name=^"+name+"$") + " --format '{{.ID}} {{.Name}}'")
	if !ok {
		return removalNetwork{}, errors.New("cannot verify the configured Docker network")
	}
	if list == "" {
		return removalNetwork{}, nil
	}
	id, ok := strings.CutSuffix(list, " "+name+"\n")
	if !ok || !swarmNetworkID.MatchString(id) {
		return removalNetwork{}, errors.New("cannot identify the configured Docker network")
	}
	if expected != nil && id != *expected {
		return removalNetwork{}, fmt.Errorf("the configured Docker network %s changed since this removal started", name)
	}
	out, ok := read("network inspect --format '{{json .}}' " + shellQuote(id))
	var network struct {
		ID, Name, Scope, Driver string
		Attachable, Ingress     *bool
		Containers              json.RawMessage
	}
	if !ok || !strings.HasSuffix(out, "\n") || json.Unmarshal([]byte(out), &network) != nil {
		return removalNetwork{}, errors.New("cannot verify the configured Docker network")
	}
	if network.ID != id || network.Name != name || network.Scope != "swarm" || network.Driver != "overlay" ||
		network.Attachable == nil || !*network.Attachable || network.Ingress == nil || *network.Ingress {
		return removalNetwork{}, errors.New("the configured Docker network is not the expected attachable swarm overlay")
	}
	var endpoints map[string]struct{ Name string }
	if json.Unmarshal(network.Containers, &endpoints) != nil {
		return removalNetwork{}, errors.New("cannot verify Docker container attachments")
	}
	for key, endpoint := range endpoints {
		// Docker puts its own load-balancer endpoint on an overlay network.
		if key != "lb-"+name || endpoint.Name != name+"-endpoint" {
			return removalNetwork{id: id, used: true}, nil
		}
	}
	containers, ok := read("container ls -aq --no-trunc --filter " + shellQuote("network="+id))
	containerIDs, valid := removalDockerLines(containers, validHexID)
	if !ok || !valid {
		return removalNetwork{}, errors.New("cannot verify Docker container attachments")
	}
	if len(containerIDs) != 0 {
		return removalNetwork{id: id, used: true}, nil
	}
	services, ok := read("service ls -q")
	serviceIDs, valid := removalDockerLines(services, swarmServiceRef.MatchString)
	if !ok || !valid {
		return removalNetwork{}, errors.New("cannot verify Docker service references")
	}
	const references = `{{.ID}}{{range (index .Spec.TaskTemplate "Networks")}}{{printf " %s" .Target}}{{end}}{{range (index .Spec "Networks")}}{{printf " %s" .Target}}{{end}}{{with (index . "Endpoint")}}{{range (index . "VirtualIPs")}}{{printf " %s" .NetworkID}}{{end}}{{end}}`
	for _, service := range serviceIDs {
		out, ok := read("service inspect --format " + shellQuote(references) + " " + shellQuote(service))
		line, complete := strings.CutSuffix(out, "\n")
		fields := strings.Split(line, " ")
		if !ok || !complete || strings.Contains(line, "\n") || !swarmNetworkID.MatchString(fields[0]) || !strings.HasPrefix(fields[0], service) {
			return removalNetwork{}, errors.New("cannot verify Docker service references")
		}
		for _, ref := range fields[1:] {
			if ref == id || ref == name {
				return removalNetwork{id: id, used: true}, nil
			}
			if ref == "" || strings.ContainsAny(ref, "\r\t") {
				return removalNetwork{}, errors.New("cannot verify Docker service references")
			}
		}
	}
	return removalNetwork{id: id}, nil
}

func removalDockerLines(out string, valid func(string) bool) ([]string, bool) {
	if out == "" {
		return nil, true
	}
	line, ok := strings.CutSuffix(out, "\n")
	if !ok {
		return nil, false
	}
	lines := strings.Split(line, "\n")
	for _, line := range lines {
		if !valid(line) {
			return nil, false
		}
	}
	return lines, true
}

func removeNetworkStep(ctx context.Context, s *Session) (string, string, error) {
	if err := cleanupReady(s); err != nil {
		return "", "", err
	}
	rec := s.removal
	if rec.Network == "" {
		return StatusSkipped, "no Docker network was configured", nil
	}
	socket := s.existing.DockerSocket
	docker := ""
	var network removalNetwork
	if s.DryRun && s.removalNetwork != nil {
		network = *s.removalNetwork
	} else {
		var err error
		docker, err = removalDockerCommand(ctx, s.R, socket, rec.Network)
		if err != nil {
			return "", "", err
		}
		network, err = inspectRemovalNetwork(ctx, s.R, docker, rec.Network, &rec.NetworkID)
		if err != nil {
			return "", "", err
		}
	}
	absent := "network " + rec.Network + " is already absent"
	kept := "kept network " + rec.Network + " because it is in use"
	if network.id == "" {
		return StatusOK, absent, nil
	}
	if network.used {
		if s.DryRun {
			return StatusSkipped, "would keep network " + rec.Network + " because it is in use", nil
		}
		return StatusSkipped, kept, nil
	}
	command := docker + "network rm " + shellQuote(network.id)
	if s.DryRun {
		return StatusWouldChange, "would remove network " + rec.Network + " (" + network.id + ")", nil
	}
	res, err := s.R.Run(ctx, command, nil)
	if err == nil && res.Code == 0 && len(res.Stderr) == 0 && string(res.Stdout) == network.id+"\n" {
		return StatusChanged, "removed network " + rec.Network + " (" + network.id + ")", nil
	}
	docker, err = removalDockerCommand(ctx, s.R, socket, rec.Network)
	if err != nil {
		return "", "", err
	}
	after, err := inspectRemovalNetwork(ctx, s.R, docker, rec.Network, &rec.NetworkID)
	if err != nil {
		return "", "", err
	}
	if after.id == "" {
		return StatusOK, absent, nil
	}
	if after.used {
		return StatusSkipped, kept, nil
	}
	return "", "", fmt.Errorf("could not remove network %s (%s)", rec.Network, network.id)
}
