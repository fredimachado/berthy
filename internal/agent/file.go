package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"strings"

	"berthy/internal/manifest"
	"berthy/internal/state"
)

// File is agent.json: what the agent process needs, and the environments it hosts.
type File struct {
	Listen           string        `json:"listen"`
	DockerSocket     string        `json:"docker_socket"`
	CloudflareAPIURL string        `json:"cloudflare_api_url,omitempty"`
	TelegramAPIURL   string        `json:"telegram_api_url,omitempty"`
	ComposeCommand   []string      `json:"compose_command,omitempty"`
	ApprovalTTL      string        `json:"approval_ttl,omitempty"`
	Cluster          ClusterConfig `json:"cluster"`
	Environments     []Hosted      `json:"environments"`
}

// ClusterConfig holds the server's state and alert settings.
type ClusterConfig struct {
	StateDir string         `json:"state_dir"`
	KeyFile  string         `json:"key_file"`
	Alerts   *ClusterAlerts `json:"alerts,omitempty"`
}

// ClusterAlerts configures delivery of server alerts.
type ClusterAlerts struct {
	Reminder string             `json:"reminder,omitempty" yaml:"reminder,omitempty"`
	Telegram *manifest.Telegram `json:"telegram,omitempty" yaml:"telegram,omitempty"`
	Webhook  *manifest.Webhook  `json:"webhook,omitempty" yaml:"webhook,omitempty"`
}

func (a ClusterAlerts) manifestAlerts() manifest.Alerts {
	return manifest.Alerts{Reminder: a.Reminder, Telegram: a.Telegram, Webhook: a.Webhook}
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
	return parseAgentFile(path)
}

// LoadFileForCLI reads agent.json for an offline CLI command such as berthy agent token --config. On Windows the
// permission bits are not meaningful, so the private-file check of LoadFile is skipped. Elsewhere it is LoadFile.
func LoadFileForCLI(path string) (File, error) {
	if runtime.GOOS == "windows" {
		return parseAgentFile(path)
	}
	return LoadFile(path)
}

func parseAgentFile(path string) (File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	f, err := ParseFile(b)
	if err != nil {
		return File{}, err
	}
	return f, f.Validate()
}

func (f File) Validate() error {
	if len(f.Environments) == 0 {
		return fmt.Errorf("at least one environment is required")
	}
	if f.Cluster.StateDir == "" || f.Cluster.KeyFile == "" {
		return errors.New("cluster needs state_dir and key_file")
	}
	if f.Cluster.Alerts != nil {
		if err := manifest.CheckAlertChannels(f.Cluster.Alerts.manifestAlerts()); err != nil {
			return fmt.Errorf("cluster: %w", err)
		}
	}
	clusterPaths := Hosted{StateDir: f.Cluster.StateDir, KeyFile: f.Cluster.KeyFile}
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
		if hostedPathsOverlap(clusterPaths, h) {
			return fmt.Errorf("cluster and hosted environment paths overlap: %s/%s", h.Project, h.Environment)
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
	for i, a := range f.Environments {
		for _, b := range f.Environments[i+1:] {
			if hostedPathsOverlap(a, b) {
				return fmt.Errorf("hosted environment paths overlap: %s/%s and %s/%s", a.Project, a.Environment, b.Project, b.Environment)
			}
		}
	}
	for _, c := range f.Configs() {
		if err := c.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// hostedPathsOverlap compares Linux server paths, including rotation files.
func hostedPathsOverlap(a, b Hosted) bool {
	clean := func(p string) string { return path.Clean("/" + p) }
	contains := func(dir, p string) bool { return p == dir || strings.HasPrefix(p, strings.TrimRight(dir, "/")+"/") }
	ad, bd := clean(a.StateDir), clean(b.StateDir)
	if contains(ad, bd) || contains(bd, ad) {
		return true
	}
	ak, bk := clean(a.KeyFile), clean(b.KeyFile)
	af := []string{ak, ak + state.KeyNextSuffix, ak + state.KeyPrevSuffix}
	bf := []string{bk, bk + state.KeyNextSuffix, bk + state.KeyPrevSuffix}
	for _, x := range af {
		if contains(bd, x) {
			return true
		}
		for _, y := range bf {
			if x == y {
				return true
			}
		}
	}
	for _, y := range bf {
		if contains(ad, y) {
			return true
		}
	}
	return false
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
