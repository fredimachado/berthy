package setup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"berthy/internal/agentclient"
)

func removeCLIConfigStep(ctx context.Context, s *Session) (string, string, error) {
	if err := cleanupReady(s); err != nil {
		return "", "", err
	}
	status, detail, err := removeLocalContext(ctx, s)
	if err != nil {
		if s.DryRun {
			return "", "", err
		}
		return "", "", fmt.Errorf("server cleanup finished, but the local CLI configuration could not be updated. Retry this removal: %w", err)
	}
	if !s.DryRun {
		if err := clearRemovalRecord(ctx, s); err != nil {
			return "", "", err
		}
	}
	return status, detail, nil
}

func removeLocalContext(ctx context.Context, s *Session) (string, string, error) {
	id := s.removal.ID
	path, err := agentclient.ConfigPath(s.Opts.CLIConfig)
	if err != nil {
		return StatusSkipped, "no local CLI configuration was found", nil
	}
	c, err := ReadCLIConfig(path)
	if err != nil {
		return "", "", err
	}
	entry, ok := c.Projects[id.Project].Environments[id.Environment]
	if !ok {
		return StatusOK, id.String() + " has no local CLI entry", nil
	}
	matches := localContextMatches(s.Opts, entry.URL)
	if matches && !s.Opts.Local {
		matches, err = verifiedSSHAddress(ctx, s.R, s.Opts.ListenIP)
		if err != nil {
			return "", "", err
		}
	}
	if !matches {
		verb := "kept"
		if s.DryRun {
			verb = "would keep"
		}
		return StatusSkipped, fmt.Sprintf("%s the local CLI entry for %s because its URL could not be tied to this server. To remove it explicitly, run berthy context remove --project %s -e %s --config %s", verb, id, id.Project, id.Environment, shellQuote(path)), nil
	}
	if s.DryRun {
		return StatusWouldChange, "would remove the local CLI entry for " + id.String(), nil
	}
	c.Remove(id.Project, id.Environment)
	if err := agentclient.WriteConfig(path, c); err != nil {
		return "", "", err
	}
	return StatusChanged, "removed the local CLI entry for " + id.String(), nil
}

type sshLoginRunner interface {
	runSSHLogin(context.Context, string) (Result, error)
}

var errNoSSHConnection = errors.New("SSH connection addresses are unavailable")

func verifiedSSHAddress(ctx context.Context, r Runner, listen net.IP) (bool, error) {
	if listen.IsLoopback() {
		return false, nil
	}
	login, ok := r.(sshLoginRunner)
	if !ok {
		return false, nil
	}
	// sshd sets SSH_CONNECTION for the login shell. sudo can drop it.
	res, err := login.runSSHLogin(ctx, `printf '%s\n' "$SSH_CONNECTION"`)
	var transport *SSHTransportError
	if errors.As(err, &transport) {
		return false, err
	}
	if err != nil || res.Code != 0 || len(res.Stderr) != 0 {
		return false, nil
	}
	fields := strings.Fields(string(res.Stdout))
	if len(fields) != 4 || !net.ParseIP(fields[2]).Equal(listen) {
		return false, nil
	}
	client := net.ParseIP(fields[0])
	if client == nil || client.IsLoopback() {
		return false, nil
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false, nil
	}
	// A local listen address can be a port forward. A nonlocal client address can be a jump host.
	direct := false
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil {
			continue
		}
		if ip.Equal(listen) {
			return false, nil
		}
		direct = direct || ip.Equal(client)
	}
	return direct, nil
}

func localContextMatches(opts Options, address string) bool {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "http" || strings.TrimRight(u.EscapedPath(), "/") != "" {
		return false
	}
	port := 80
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
	}
	return err == nil && port == opts.ListenPort && net.ParseIP(u.Hostname()).Equal(opts.ListenIP)
}

func clearRemovalRecord(ctx context.Context, s *Session) error {
	script := "# clear removal record\nset -eu\n" + removalStat +
		"file=" + shellQuote(removalRecordPath(s.removal.ID)) + "\n" + `
stat_entry "$file"
case "$entry" in
  absent) exit 0;;
  *" 0 600 1 regular file") rm -- "$file";;
  *) exit 1;;
esac
`
	res, err := s.R.Run(ctx, script, nil)
	var transport *SSHTransportError
	if errors.As(err, &transport) {
		return fmt.Errorf("cleanup finished, but deletion of the pending removal record is unconfirmed. Check %s on the server and retry this removal if it remains: %w", removalRecordPath(s.removal.ID), err)
	}
	if err != nil || res.Code != 0 || len(res.Stderr) != 0 || len(res.Stdout) != 0 {
		return errors.New("cleanup finished, but the pending removal record could not be cleared. Retry this removal")
	}
	return nil
}
