// Package agentclient is what the CLI and the MCP server share to talk to an agent: the CLI configuration, the HTTP client, the
// request, the printable form of a refusal, and the request body of a deploy.
package agentclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"berthy/internal/manifest"
)

// ErrInvalidResponse is an answer of the agent that cannot be used: unreadable or over 1 MiB.
var ErrInvalidResponse = errors.New("invalid agent response")

// Environment is one environment's entry of the CLI configuration.
type Environment struct {
	URL      string `json:"url"`
	TokenEnv string `json:"token_env"`
}

// Project is one project's entry of the CLI configuration: its environments.
type Project struct {
	Environments map[string]Environment `json:"environments"`
}

// Config is the CLI configuration file: each project's environments, where each one's agent is and which environment variable
// holds its token.
type Config struct {
	Projects map[string]Project `json:"projects"`
}

// ErrEarlierLayout is the refusal of a CLI configuration written as environments.<environment>, before projects.
var ErrEarlierLayout = errors.New("the CLI configuration is in the layout of an earlier release: run berthy setup again, or write it as projects.<project>.environments.<environment>")

// ParseConfig reads the configuration file's content: unknown fields and trailing data are refused. The errors never repeat
// any value of the file.
func ParseConfig(b []byte) (Config, error) {
	var c Config
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		var earlier struct {
			Environments json.RawMessage `json:"environments"`
		}
		if json.Unmarshal(b, &earlier) == nil && earlier.Environments != nil {
			return Config{}, ErrEarlierLayout
		}
		return Config{}, errors.New("invalid config")
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return Config{}, errors.New("invalid trailing JSON")
	}
	return c, nil
}

// Target is one environment's agent as the configuration names it.
type Target struct {
	Project string
	Env     string
	// Base is the agent's URL.
	Base *url.URL
	// Prefix is the path of the environment's API, with a trailing slash.
	Prefix string
	// Token is the bearer token. Nothing prints it.
	Token string
}

// ConfigEnv is the environment variable that names the CLI configuration file when --config is not given.
const ConfigEnv = "BERTHY_CONFIG"

// ConfigPath is the CLI configuration file to use: the flag when it is not empty, else $BERTHY_CONFIG, else berthy/config.json in
// the user's configuration directory. It fails only when none of them can be found.
func ConfigPath(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if p := os.Getenv(ConfigEnv); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", errors.New("no CLI configuration: give --config or set BERTHY_CONFIG")
	}
	return filepath.Join(dir, "berthy", "config.json"), nil
}

// Selector says which environment a command means.
type Selector struct {
	Project     string // --project; empty when not given
	Environment string // -e
	ManifestDir string // where berthy.yml is looked for when Project is empty; empty means do not look
}

// Resolve reads the configuration file and returns the target the selector means, with the token from the variable the file
// names. The project is the selector's, else the one of the berthy.yml in ManifestDir, else the only project that has the
// environment. The errors name projects and environments and never repeat any other value of the file.
func Resolve(path string, sel Selector) (*Target, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read config %s", path)
	}
	c, err := ParseConfig(b)
	if err != nil {
		return nil, err
	}
	project, env := sel.Project, sel.Environment
	if project == "" && sel.ManifestDir != "" {
		name, ok, err := manifest.ProjectName(sel.ManifestDir)
		if err != nil {
			return nil, fmt.Errorf("cannot read the project of %s: give --project", filepath.Join(sel.ManifestDir, "berthy.yml"))
		}
		if ok {
			project = name
		}
	}
	var entry Environment
	switch {
	case project != "":
		var ok bool
		if entry, ok = c.Projects[project].Environments[env]; !ok {
			return nil, fmt.Errorf("no environment %s for project %s in the CLI configuration", env, project)
		}
	default:
		var have []string
		for name, p := range c.Projects {
			if _, ok := p.Environments[env]; ok {
				have = append(have, name)
			}
		}
		slices.Sort(have)
		switch len(have) {
		case 0:
			return nil, errors.New("unknown environment or missing token_env")
		case 1:
			project = have[0]
			entry = c.Projects[project].Environments[env]
		default:
			return nil, fmt.Errorf("environment %s exists in several projects (%s): give --project, or run the command in the project's directory", env, strings.Join(have, ", "))
		}
	}
	if entry.TokenEnv == "" {
		return nil, errors.New("unknown environment or missing token_env")
	}
	token := os.Getenv(entry.TokenEnv)
	if token == "" {
		return nil, errors.New("token environment variable is empty")
	}
	u, err := url.Parse(entry.URL)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid agent URL")
	}
	return &Target{Project: project, Env: env, Base: u, Prefix: strings.TrimRight(u.Path, "/") + "/v1/environments/" + url.PathEscape(env) + "/", Token: token}, nil
}

// NewHTTPClient is the HTTP client for the agent: no proxy, short connect and header limits, and no redirects.
func NewHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, ResponseHeaderTimeout: 20 * time.Second}, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Send sends a request to the agent with the token. A nil body sends none.
func Send(ctx context.Context, c *http.Client, method, address, token string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.Do(req)
}

// Caller returns a function that sends a request without a body to the agent under prefix and reads the answer, at most 1 MiB
// of it.
func Caller(ctx context.Context, client *http.Client, base *url.URL, prefix, token string) func(method, path, query string) ([]byte, int, error) {
	return func(method, path, query string) ([]byte, int, error) {
		return Do(ctx, client, base, prefix, token, method, path, query, nil)
	}
}

// Do sends one request to the agent under prefix and reads the answer, at most 1 MiB of it. A nil body sends none.
func Do(ctx context.Context, client *http.Client, base *url.URL, prefix, token, method, path, query string, body []byte) ([]byte, int, error) {
	u := *base
	u.Path, u.RawQuery = prefix+path, query
	resp, err := Send(ctx, client, method, u.String(), token, body)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, 0, ErrInvalidResponse
	}
	return data, resp.StatusCode, nil
}

// Message describes a refused agent request. The agent message is shown only when it is safe to print.
func Message(status int, body []byte) string {
	var reply struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &reply) == nil && PrintableMessage(reply.Message) {
		return fmt.Sprintf("agent returned HTTP %d: %s", status, reply.Message)
	}
	return fmt.Sprintf("agent returned HTTP %d", status)
}

// PrintableMessage reports whether s is at most 300 bytes of valid UTF-8 without control characters.
func PrintableMessage(s string) bool {
	if s == "" || len(s) > 300 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// WithTimeout returns c with its request limit, and the wait for response headers, set to d: for a request the agent holds open.
func WithTimeout(c *http.Client, d time.Duration) *http.Client {
	c.Timeout = d
	c.Transport.(*http.Transport).ResponseHeaderTimeout = d
	return c
}

// NewRequestID returns a random id for a change, with which the agent recognizes a retry of the same request.
func NewRequestID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(random[:]), nil
}

// Redact removes the token from s.
func (t *Target) Redact(s string) string {
	if t.Token == "" {
		return s
	}
	return strings.ReplaceAll(s, t.Token, "[redacted]")
}
