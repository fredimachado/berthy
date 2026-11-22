package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"berthy/internal/agentclient"
)

// DefaultTokenEnv is the variable that holds the token of env unless the CLI configuration names another.
func DefaultTokenEnv(project, env string) string {
	return "BERTHY_" + strings.ToUpper(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, project+"_"+env)) + "_TOKEN"
}

// agentURL is the address of the agent as the CLI configuration holds it.
func agentURL(listen string) string { return "http://" + listen }

// cliConfigStep adds the environment to the CLI configuration, under its project: the agent's address and the variable that
// holds its token. The file is the one --config names, or else the one the CLI finds without it (BERTHY_CONFIG, then the default
// file). The file never holds a token, and the other environments and projects are kept as they are.
func cliConfigStep(ctx context.Context, s *Session) (string, string, error) {
	project, env := s.Opts.Project, s.Opts.Env
	url := agentURL(s.Opts.listen())
	tokenEnv := DefaultTokenEnv(project, env)
	path, err := agentclient.ConfigPath(s.Opts.CLIConfig)
	if err != nil {
		// No location is known, so the entry is for the person to add by hand.
		s.TokenEnv = tokenEnv
		return StatusSkipped, fmt.Sprintf(`add {"url": %q, "token_env": %q} as projects.%s.environments.%s of the CLI configuration: there is no default location, so give --config or set BERTHY_CONFIG`, url, tokenEnv, project, env), nil
	}
	c, err := ReadCLIConfig(path)
	if err != nil {
		return "", "", err
	}

	have := c.Projects[project].Environments[env]
	if have.TokenEnv != "" {
		tokenEnv = have.TokenEnv
	}
	s.TokenEnv = tokenEnv
	want := agentclient.Environment{URL: url, TokenEnv: tokenEnv}
	changed, err := c.Put(project, env, want)
	if err != nil {
		return "", "", err
	}
	if !changed {
		return StatusOK, fmt.Sprintf("%s has %s at %s (token in $%s)", filepath.Base(path), env, url, tokenEnv), nil
	}
	if s.DryRun {
		return StatusWouldChange, fmt.Sprintf("would write %s to %s", env, filepath.Base(path)), nil
	}
	if s.Opts.CLIConfig == "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", "", fmt.Errorf("creating the CLI configuration directory: %w", err)
		}
	}
	if err := agentclient.WriteConfig(path, c); err != nil {
		return "", "", fmt.Errorf("writing the CLI configuration: %w", err)
	}
	return StatusChanged, fmt.Sprintf("wrote %s at %s to %s (token in $%s)", env, url, filepath.Base(path), tokenEnv), nil
}

// ReadCLIConfig reports configuration errors as setup does.
func ReadCLIConfig(path string) (agentclient.Config, error) {
	c, err := agentclient.ReadConfig(path)
	switch {
	case errors.Is(err, agentclient.ErrEarlierLayout):
		// The CLI's own text sends the person to setup, which is where they are.
		return c, fmt.Errorf("the CLI configuration %s is in the layout of an earlier release: move it aside, or give --config with a new file, then run setup again", path)
	case errors.Is(err, agentclient.ErrUnreadable):
		return c, err
	case err != nil:
		return c, fmt.Errorf("the CLI configuration is not valid: %v", err)
	}
	return c, nil
}
