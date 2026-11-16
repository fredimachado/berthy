package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

// readCLIConfig reads the CLI configuration file, strictly as the CLI does. A missing file is an empty configuration.
func readCLIConfig(path string) (agentclient.Config, []byte, error) {
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return agentclient.Config{}, nil, nil
	case err != nil:
		return agentclient.Config{}, nil, errors.New("cannot read the CLI configuration")
	}
	c, err := agentclient.ParseConfig(b)
	if err != nil {
		return c, nil, fmt.Errorf("the CLI configuration is not valid: %v", err)
	}
	return c, b, nil
}

// writeFileAtomic replaces path with data: a temporary file in the same directory, then a rename. A new file is private, 0600;
// an existing one keeps its mode.
func writeFileAtomic(path string, data []byte) error {
	mode := fs.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// cliConfigStep adds the environment to the CLI configuration --config names: the agent's address and the variable that holds
// its token. The file never holds a token, and the other environments are kept as they are.
func cliConfigStep(ctx context.Context, s *Session) (string, string, error) {
	env := s.Opts.Env
	url := agentURL(s.Opts.listen())
	tokenEnv := DefaultTokenEnv(env)
	path := s.Opts.CLIConfig
	if path == "" {
		s.TokenEnv = tokenEnv
		return StatusSkipped, fmt.Sprintf(`add {"url": %q, "token_env": %q} to the CLI configuration`, url, tokenEnv), nil
	}
	c, _, err := readCLIConfig(path)
	if err != nil {
		return "", "", err
	}
	if e, ok := c.Environments[env]; ok && e.TokenEnv != "" {
		tokenEnv = e.TokenEnv
	}
	s.TokenEnv = tokenEnv
	want := agentclient.Environment{URL: url, TokenEnv: tokenEnv}
	if c.Environments[env] == want {
		return StatusOK, fmt.Sprintf("%s has %s at %s (token in $%s)", filepath.Base(path), env, url, tokenEnv), nil
	}
	if s.DryRun {
		return StatusWouldChange, fmt.Sprintf("would write %s to %s", env, filepath.Base(path)), nil
	}
	if c.Environments == nil {
		c.Environments = map[string]agentclient.Environment{}
	}
	c.Environments[env] = want
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", "", err
	}
	if err := writeFileAtomic(path, append(bytes.TrimRight(data, "\n"), '\n')); err != nil {
		return "", "", fmt.Errorf("writing the CLI configuration: %w", err)
	}
	return StatusChanged, fmt.Sprintf("wrote %s at %s to %s (token in $%s)", env, url, filepath.Base(path), tokenEnv), nil
}
