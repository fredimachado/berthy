package agentclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"berthy/internal/scope"
)

var tokenEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ErrUnreadable is a CLI configuration path that exists but cannot be read.
var ErrUnreadable = errors.New("cannot read config")

// ReadConfig reads the CLI configuration at path, strictly as ParseConfig does. A missing file is an empty configuration.
func ReadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Config{}, nil
	case err != nil:
		return Config{}, fmt.Errorf("%w %s", ErrUnreadable, path)
	}
	return ParseConfig(b)
}

// WriteConfig replaces the file at path with c: a temporary file in the same directory, then a rename. A new file is
// private (0600); an existing one keeps its mode.
func WriteConfig(path string, c Config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(bytes.TrimRight(data, "\n"), '\n'))
}

// Put adds or replaces the entry of one environment of one project, after checking every part. changed is false when
// the entry was already exactly that.
func (c *Config) Put(project, env string, e Environment) (changed bool, err error) {
	if !scope.ValidProject(project) {
		return false, errors.New("invalid project")
	}
	if !scope.ValidEnvironment(env) {
		return false, errors.New("invalid environment")
	}
	if err := (scope.ID{Project: project, Environment: env}).CheckLength(); err != nil {
		return false, err
	}
	if _, err := parseAgentURL(e.URL); err != nil {
		return false, err
	}
	if !ValidTokenEnv(e.TokenEnv) {
		return false, errors.New("invalid token environment variable")
	}
	if c.Projects == nil {
		c.Projects = map[string]Project{}
	}
	p := c.Projects[project]
	if p.Environments == nil {
		p.Environments = map[string]Environment{}
	}
	if have, ok := p.Environments[env]; ok && have == e {
		return false, nil
	}
	p.Environments[env] = e
	c.Projects[project] = p
	return true, nil
}

// Remove deletes the entry, and the project when no environment of it is left. It reports whether there was one.
func (c *Config) Remove(project, env string) bool {
	p, ok := c.Projects[project]
	if !ok {
		return false
	}
	if _, ok := p.Environments[env]; !ok {
		return false
	}
	delete(p.Environments, env)
	if len(p.Environments) == 0 {
		delete(c.Projects, project)
	} else {
		c.Projects[project] = p
	}
	return true
}

// ValidTokenEnv reports whether s can name an environment variable: ^[A-Za-z_][A-Za-z0-9_]*$.
func ValidTokenEnv(s string) bool { return tokenEnvName.MatchString(s) }

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
