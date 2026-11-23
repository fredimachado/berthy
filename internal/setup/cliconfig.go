package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"berthy/internal/agentclient"
	"berthy/internal/credstore"
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
	s.credentialRead = false
	url := s.AgentURL()
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
	s.cliEntry = want
	s.cliConfigKnown = true
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

// AgentURL is the agent's URL as the CLI configuration holds it.
func (s *Session) AgentURL() string {
	if s.Opts.listen() == "" {
		return ""
	}
	return agentURL(s.Opts.listen())
}

// tokenStoreStep saves a newly live token when the CLI entry can find it.
func tokenStoreStep(ctx context.Context, s *Session) (string, string, error) {
	if s.DryRun && s.dryToken {
		return StatusWouldChange, "would save the admin token on this machine, or show it when there is no credential store", nil
	}
	store := s.Opts.Tokens
	usable := s.cliConfigKnown && agentclient.StoreUsable(s.Opts.Project, s.Opts.Env, s.cliEntry)
	if s.Token != "" && s.TokenLive {
		if !s.cliConfigKnown {
			return StatusSkipped, "not saved: there is no CLI configuration to find it from; the token is shown below", nil
		}
		if store != nil && usable {
			err := store.Set(agentclient.StoreKey(s.Opts.Project, s.Opts.Env, s.cliEntry), s.Token)
			if err == nil {
				s.TokenStored = true
				return StatusChanged, fmt.Sprintf("saved the admin token in the %s", store.Name()), nil
			}
			if errors.Is(err, credstore.ErrGivenUp) {
				return StatusSkipped, fmt.Sprintf("could not confirm the save in the %s: the token is shown below; set it in $%s", store.Name(), s.TokenEnv), nil
			}
		}
		return StatusSkipped, fmt.Sprintf("no credential store on this machine: the token is shown below; set it in $%s", s.TokenEnv), nil
	}
	_, source := s.existingCredential()
	if source == agentclient.TokenFromEnv {
		return StatusOK, fmt.Sprintf("the admin token is in $%s", s.TokenEnv), nil
	}
	if source == agentclient.TokenFromStore {
		return StatusOK, fmt.Sprintf("the admin token is in the %s", store.Name()), nil
	}
	return StatusSkipped, fmt.Sprintf("no admin token on this machine: set $%s, or run setup with --new-token", s.TokenEnv), nil
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
