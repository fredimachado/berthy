package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// File is agent.json: what the agent process needs, and the environments it hosts.
type File struct {
	Listen           string   `json:"listen"`
	DockerSocket     string   `json:"docker_socket"`
	CloudflareAPIURL string   `json:"cloudflare_api_url,omitempty"`
	TelegramAPIURL   string   `json:"telegram_api_url,omitempty"`
	ComposeCommand   []string `json:"compose_command,omitempty"`
	ApprovalTTL      string   `json:"approval_ttl,omitempty"`
	Environments     []Hosted `json:"environments"`
}

// Hosted is one hosted environment of agent.json.
type Hosted struct {
	Project     string  `json:"project"`
	Environment string  `json:"environment"`
	StateDir    string  `json:"state_dir"`
	KeyFile     string  `json:"key_file"`
	Protected   bool    `json:"protected,omitempty"`
	Network     string  `json:"network,omitempty"`
	Tokens      []Token `json:"tokens"`
}

// ErrOldLayout is the refusal of an agent.json of a release before projects.
var ErrOldLayout = errors.New("agent.json is in the layout of an earlier Berthy release: set the server up again with berthy setup")

func ParseFile(b []byte) (File, error) {
	var keys map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&keys); err != nil {
		return File{}, err
	}
	if _, ok := keys["environment"]; ok {
		return File{}, ErrOldLayout
	}
	var f File
	dec = json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return File{}, err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return File{}, fmt.Errorf("invalid trailing JSON")
	}
	return f, nil
}

func LoadFile(path string) (File, error) {
	var f File
	st, err := os.Stat(path)
	if err != nil {
		return f, err
	}
	if st.Mode().Perm()&0077 != 0 {
		return f, fmt.Errorf("agent config must not be accessible by group or others")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	f, err = ParseFile(b)
	if err != nil {
		return File{}, err
	}
	return f, f.Validate()
}

func (f File) Validate() error {
	if len(f.Environments) == 0 {
		return fmt.Errorf("at least one environment is required")
	}
	ids := map[string]bool{}
	prefixes := map[string]bool{}
	states := map[string]bool{}
	keyFiles := map[string]bool{}
	networks := map[string]bool{}
	digests := map[string]bool{}
	for _, h := range f.Environments {
		if h.Project == "" {
			return fmt.Errorf("a hosted environment needs a project")
		}
		if h.StateDir == "" || h.KeyFile == "" {
			return fmt.Errorf("a hosted environment needs state_dir and key_file")
		}
		id := h.Project + "/" + h.Environment
		if ids[id] {
			return fmt.Errorf("duplicate hosted environment")
		}
		ids[id] = true
		prefix := h.Project + "-" + h.Environment
		if prefixes[prefix] {
			return fmt.Errorf("duplicate project-environment prefix")
		}
		prefixes[prefix] = true
		if states[h.StateDir] {
			return fmt.Errorf("duplicate state_dir")
		}
		states[h.StateDir] = true
		if keyFiles[h.KeyFile] {
			return fmt.Errorf("duplicate key_file")
		}
		keyFiles[h.KeyFile] = true
		if h.Network != "" {
			if networks[h.Network] {
				return fmt.Errorf("duplicate network")
			}
			networks[h.Network] = true
		}
		for _, tok := range h.Tokens {
			d := strings.ToLower(tok.Digest)
			if digests[d] {
				return fmt.Errorf("duplicate token digest")
			}
			digests[d] = true
		}
	}
	for _, c := range f.Configs() {
		if err := c.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (f File) Configs() []Config {
	out := make([]Config, len(f.Environments))
	for i, h := range f.Environments {
		out[i] = Config{
			Project:          h.Project,
			Environment:      h.Environment,
			Listen:           f.Listen,
			DockerSocket:     f.DockerSocket,
			StateDir:         h.StateDir,
			KeyFile:          h.KeyFile,
			Protected:        h.Protected,
			Network:          h.Network,
			CloudflareAPIURL: f.CloudflareAPIURL,
			TelegramAPIURL:   f.TelegramAPIURL,
			ComposeCommand:   f.ComposeCommand,
			ApprovalTTL:      f.ApprovalTTL,
			Tokens:           h.Tokens,
		}
	}
	return out
}
