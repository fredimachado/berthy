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
func DefaultTokenEnv(env string) string {
	return "BERTHY_" + strings.ToUpper(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, env)) + "_TOKEN"
}

// agentURL is the address of the agent as the CLI configuration holds it.
func agentURL(listen string) string { return "http://" + listen }

// cliConfigStep adds the environment to the CLI configuration, under its project: the agent's address and the variable that
// holds its token. The file is the one --config names, or else the one the CLI finds without it (BERTHY_CONFIG, then the default
// file). The file never holds a token, and the other environments and projects are kept as they are. Without a project, which
// only a manifest names, the entry is for the person to add by hand.
func cliConfigStep(ctx context.Context, s *Session) (string, string, error) {
	project, env := s.Opts.Project, s.Opts.Env
	url := agentURL(s.Opts.listen())
	tokenEnv := DefaultTokenEnv(env)
	if project == "" {
		s.TokenEnv = tokenEnv
		return StatusSkipped, fmt.Sprintf(`add {"url": %q, "token_env": %q} as projects.<project>.environments.%s of the CLI configuration: there is no berthy.yml to name the project`, url, tokenEnv, env), nil
	}
	path, err := agentclient.ConfigPath(s.Opts.CLIConfig)
	if err != nil {
		// No location is known, so the entry is for the person to add by hand.
		s.TokenEnv = tokenEnv
		return StatusSkipped, fmt.Sprintf(`add {"url": %q, "token_env": %q} as projects.%s.environments.%s of the CLI configuration: there is no default location, so give --config or set BERTHY_CONFIG`, url, tokenEnv, project, env), nil
	}
	c, err := agentclient.ReadConfig(path)
	switch {
	case errors.Is(err, agentclient.ErrEarlierLayout):
		// The CLI's own text sends the person to setup, which is where they are.
		return "", "", fmt.Errorf("the CLI configuration %s is in the layout of an earlier release: move it aside, or give --config with a new file, then run setup again", path)
	case err != nil:
		if strings.HasPrefix(err.Error(), "cannot read config ") {
			return "", "", err
		}
		return "", "", fmt.Errorf("the CLI configuration is not valid: %v", err)
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
		// Without --config the file is the person's own location, so its directory is made private when it is missing.
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", "", fmt.Errorf("creating the CLI configuration directory: %w", err)
		}
	}
	if err := agentclient.WriteConfig(path, c); err != nil {
		return "", "", fmt.Errorf("writing the CLI configuration: %w", err)
	}
	return StatusChanged, fmt.Sprintf("wrote %s at %s to %s (token in $%s)", env, url, filepath.Base(path), tokenEnv), nil
}
