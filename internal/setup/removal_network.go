package setup

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var swarmNetworkID = regexp.MustCompile(`^[a-z0-9]{25}$`)

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

func inspectRemovalNetwork(ctx context.Context, r Runner, socket, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	if socket == "" {
		socket = dockerSocket
	}
	if !removalDockerReady(ctx, r, socket) {
		return "", fmt.Errorf("cannot establish the disposition of network %s because its Docker daemon could not be verified as running", name)
	}
	docker := "timeout 30 docker --host " + shellQuote("unix://"+socket) + " "
	read := func(command string) (string, error) {
		res, err := r.Run(ctx, docker+command, nil)
		if err != nil || res.Code != 0 || len(res.Stderr) != 0 {
			return "", errors.New("cannot verify the configured Docker network")
		}
		return string(res.Stdout), nil
	}
	list, err := read("network ls --no-trunc --filter " + shellQuote("name=^"+name+"$") + " --format '{{.ID}} {{.Name}}'")
	if err != nil || list == "" {
		return "", err
	}
	id, ok := strings.CutSuffix(list, " "+name+"\n")
	if !ok || !swarmNetworkID.MatchString(id) {
		return "", errors.New("cannot identify the configured Docker network")
	}
	out, err := read("network inspect --format '{{.Id}} {{.Name}} {{.Scope}} {{.Driver}} {{.Attachable}} {{.Ingress}}' " + shellQuote(id))
	if err != nil {
		return "", err
	}
	if out != id+" "+name+" swarm overlay true false\n" {
		return "", errors.New("the configured Docker network is not the expected attachable swarm overlay")
	}
	return id, nil
}
