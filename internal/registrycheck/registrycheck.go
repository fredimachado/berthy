// Package registrycheck checks repository pull access, lists tags, and resolves a tag with a manifest HEAD request.
// It handles registry authentication challenges and Bearer token requests using only the standard library.
package registrycheck

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrDenied means the registry answered and refused: the credential is wrong, or it cannot pull the repository.
var ErrDenied = errors.New("registry refused the credential")

// maxResponse bounds how much of a response body is read.
const maxResponse = 1 << 20

// Probe is a credential for one registry.
type Probe struct {
	Host     string // the registry host as the manifest names it: "ghcr.io", "127.0.0.1:5000"
	Username string
	Password string       // never logged and never put in an error
	HTTP     *http.Client // nil => &http.Client{Timeout: 15 * time.Second}
}

// CanPull reports whether the credential can read repository repo ("acme/api") on the registry.
//   - nil: the registry listed the repository's tags for this credential, or needs no credential at all.
//   - ErrDenied (test with errors.Is): the registry answered 401, 403 or 404 to the token request or to the
//     repository request made with the credential.
//   - any other error: the registry could not be asked (connection failure, unexpected status, malformed challenge,
//     a token realm that is not allowed).
func (p Probe) CanPull(ctx context.Context, repo string) error {
	listURL := baseURL(p.Host) + "/v2/" + repo + "/tags/list?n=1"

	status, header, err := p.get(ctx, listURL, "")
	if err != nil {
		return fmt.Errorf("ask %s about %s: %w", p.Host, repo, err)
	}
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
	default:
		return fmt.Errorf("%s answered HTTP %d to the request for %s", p.Host, status, repo)
	}

	scheme, params, ok := parseChallenge(header.Values("WWW-Authenticate"))
	var auth string
	switch {
	case ok && scheme == "basic":
		auth = p.basic()
	case ok && scheme == "bearer":
		token, err := p.token(ctx, params, repo)
		if err != nil {
			return err
		}
		auth = "Bearer " + token
	default:
		return fmt.Errorf("%s sent an authentication challenge that is not understood", p.Host)
	}

	status, _, err = p.get(ctx, listURL, auth)
	if err != nil {
		return fmt.Errorf("ask %s about %s: %w", p.Host, repo, err)
	}
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return fmt.Errorf("%w: %s denied access to %s", ErrDenied, p.Host, repo)
	default:
		return fmt.Errorf("%s answered HTTP %d to the request for %s", p.Host, status, repo)
	}
}

// token asks a Bearer challenge's realm for a pull token for repo, authenticating with the credential.
func (p Probe) token(ctx context.Context, params map[string]string, repo string) (string, error) {
	realm, err := url.Parse(params["realm"])
	if err != nil || params["realm"] == "" || realm.Hostname() == "" {
		return "", fmt.Errorf("%s sent a Bearer challenge without a usable realm", p.Host)
	}
	if realm.Scheme != "https" && (realm.Scheme != "http" || !isLoopback(realm.Hostname())) {
		return "", fmt.Errorf("%s sent a token realm on %s://%s, which is not allowed: the credential is only sent over https or to a loopback host",
			p.Host, realm.Scheme, realm.Host)
	}
	q := realm.Query()
	if service := params["service"]; service != "" {
		q.Set("service", service)
	}
	q.Set("scope", "repository:"+repo+":pull")
	realm.RawQuery = q.Encode()

	status, body, err := p.fetch(ctx, realm.String(), p.basic())
	if err != nil {
		return "", fmt.Errorf("ask %s for a token: %w", realm.Host, err)
	}
	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", fmt.Errorf("%w: %s denied the credential", ErrDenied, realm.Host)
	default:
		return "", fmt.Errorf("%s answered HTTP %d to the token request", realm.Host, status)
	}
	var issued struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &issued); err != nil {
		return "", fmt.Errorf("%s sent a token response that is not JSON", realm.Host)
	}
	if issued.Token != "" {
		return issued.Token, nil
	}
	if issued.AccessToken != "" {
		return issued.AccessToken, nil
	}
	return "", fmt.Errorf("%s sent a token response without a token", realm.Host)
}

// basic is the Authorization header value for the credential.
func (p Probe) basic() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(p.Username+":"+p.Password))
}

// get requests rawURL, with the Authorization header when auth is not empty, and returns the status and headers.
func (p Probe) get(ctx context.Context, rawURL, auth string) (int, http.Header, error) {
	status, header, _, err := p.do(ctx, rawURL, auth)
	return status, header, err
}

// fetch is like get and also returns the (bounded) body.
func (p Probe) fetch(ctx context.Context, rawURL, auth string) (int, []byte, error) {
	status, _, body, err := p.do(ctx, rawURL, auth)
	return status, body, err
}

func (p Probe) do(ctx context.Context, rawURL, auth string) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, body, nil
}

// baseURL is https://host, or http://host for a loopback registry (as Docker does).
func baseURL(host string) string {
	// Docker Hub's names (docker.io, index.docker.io) are not its registry API host; this is Docker's own mapping.
	if h := strings.ToLower(hostname(host)); h == "docker.io" || h == "index.docker.io" {
		return "https://registry-1.docker.io"
	}
	if isLoopback(hostname(host)) {
		return "http://" + host
	}
	return "https://" + host
}

// hostname returns host without its port: "ghcr.io", "[::1]:5000" -> "::1".
func hostname(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

func isLoopback(name string) bool {
	if strings.EqualFold(name, "localhost") {
		return true
	}
	ip := net.ParseIP(name)
	return ip != nil && ip.IsLoopback()
}

// parseChallenge picks the first Basic or Bearer challenge among the WWW-Authenticate header values. The scheme is
// returned lower-cased and the parameter names lower-cased.
func parseChallenge(values []string) (scheme string, params map[string]string, ok bool) {
	for _, v := range values {
		v = strings.TrimSpace(v)
		name, rest, _ := strings.Cut(v, " ")
		name = strings.ToLower(name)
		if name == "basic" || name == "bearer" {
			return name, parseParams(rest), true
		}
	}
	return "", nil, false
}

// parseParams reads comma-separated key=value pairs whose values are tokens or quoted strings.
func parseParams(s string) map[string]string {
	params := map[string]string{}
	for {
		s = strings.TrimLeft(s, " \t,")
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return params
		}
		key := strings.ToLower(strings.TrimSpace(s[:eq]))
		s = strings.TrimLeft(s[eq+1:], " \t")
		var val string
		if strings.HasPrefix(s, `"`) {
			var b strings.Builder
			i := 1
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				b.WriteByte(s[i])
			}
			val = b.String()
			s = s[min(i+1, len(s)):]
		} else {
			end := strings.IndexByte(s, ',')
			if end < 0 {
				end = len(s)
			}
			val = strings.TrimSpace(s[:end])
			s = s[end:]
		}
		if _, dup := params[key]; !dup {
			params[key] = val
		}
	}
}
