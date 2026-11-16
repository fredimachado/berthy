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
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ErrInvalidResponse is an answer of the agent that cannot be used: unreadable or over 1 MiB.
var ErrInvalidResponse = errors.New("invalid agent response")

// Config is the CLI configuration file: where each environment's agent is and which environment variable holds its token.
type Config struct {
	Environments map[string]struct {
		URL      string `json:"url"`
		TokenEnv string `json:"token_env"`
	} `json:"environments"`
}

// Target is one environment's agent as the configuration names it.
type Target struct {
	Env string
	// Base is the agent's URL.
	Base *url.URL
	// Prefix is the path of the environment's API, with a trailing slash.
	Prefix string
	// Token is the bearer token. Nothing prints it.
	Token string
}

// Load reads the configuration file and returns the target of env, with the token from the variable the file names. The
// errors say what is wrong without repeating any value of the file.
func Load(path, env string) (*Target, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read config")
	}
	var c Config
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return nil, errors.New("invalid config")
	}
	if err = d.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("invalid trailing JSON")
	}
	target, ok := c.Environments[env]
	if !ok || target.TokenEnv == "" {
		return nil, errors.New("unknown environment or missing token_env")
	}
	token := os.Getenv(target.TokenEnv)
	if token == "" {
		return nil, errors.New("token environment variable is empty")
	}
	u, err := url.Parse(target.URL)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid agent URL")
	}
	return &Target{Env: env, Base: u, Prefix: strings.TrimRight(u.Path, "/") + "/v1/environments/" + url.PathEscape(env) + "/", Token: token}, nil
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

// EnvPattern is what an environment name looks like.
var EnvPattern = regexp.MustCompile(`^[a-z0-9-]+$`)

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
