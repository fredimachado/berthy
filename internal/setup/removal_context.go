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
	status, detail, err := removeLocalContext(s)
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

func removeLocalContext(s *Session) (string, string, error) {
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
	if !localContextMatches(s.Opts, entry.URL) {
		return StatusSkipped, fmt.Sprintf("kept the local CLI entry for %s because its URL does not match this server. Use berthy context remove --project %s -e %s to remove it explicitly", id, id.Project, id.Environment), nil
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
	if err != nil || res.Code != 0 || len(res.Stderr) != 0 || len(res.Stdout) != 0 {
		return errors.New("cleanup finished, but the pending removal record could not be cleared. Retry this removal")
	}
	return nil
}
