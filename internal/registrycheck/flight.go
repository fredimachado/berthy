package registrycheck

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type registryFlight struct {
	ctx    context.Context
	client http.Client
	probe  Probe
	origin *url.URL
	repo   string
}

func newRegistryFlight(ctx context.Context, p Probe, origin *url.URL, repo string) registryFlight {
	client := http.Client{Timeout: 15 * time.Second}
	if p.HTTP != nil {
		client = *p.HTTP
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return registryFlight{ctx: ctx, client: client, probe: p, origin: origin, repo: repo}
}

func (f registryFlight) request(method string, u *url.URL, auth, accept string) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(f.ctx, method, u.String(), nil)
	if err != nil {
		return 0, nil, nil, errors.New("invalid registry request")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return 0, nil, nil, errors.New("registry request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || method == http.MethodHead {
		return resp.StatusCode, resp.Header, nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(body) > maxResponse {
		return 0, nil, nil, errors.New("invalid registry response")
	}
	return resp.StatusCode, resp.Header, body, nil
}

func (f registryFlight) authenticate(header http.Header) (string, error) {
	scheme, params, ok := parseChallenge(header.Values("WWW-Authenticate"))
	if !ok {
		return "", errors.New("invalid registry challenge")
	}
	switch scheme {
	case "basic":
		return f.probe.basic(), nil
	case "bearer":
		realm, err := url.Parse(params["realm"])
		if err != nil || !trustedRealm(f.origin, realm, params["service"]) {
			return "", errors.New("untrusted registry token issuer")
		}
		q := realm.Query()
		if !sameOrigin(f.origin, realm) {
			q = url.Values{}
		}
		if params["service"] != "" {
			q.Set("service", params["service"])
		}
		q.Set("scope", "repository:"+f.repo+":pull")
		realm.RawQuery = q.Encode()
		tokenAuth := ""
		if f.probe.Username != "" || f.probe.Password != "" {
			tokenAuth = f.probe.basic()
		}
		status, _, body, err := f.request(http.MethodGet, realm, tokenAuth, "")
		if err != nil {
			return "", err
		}
		if status == 401 || status == 403 || status == 404 {
			return "", ErrDenied
		}
		if status != 200 {
			return "", errors.New("registry token request failed")
		}
		var issued struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
		}
		if json.Unmarshal(body, &issued) != nil {
			return "", errors.New("invalid registry token response")
		}
		token := issued.Token
		if token == "" {
			token = issued.AccessToken
		}
		if token == "" || strings.ContainsAny(token, "\r\n") {
			return "", errors.New("invalid registry token response")
		}
		return "Bearer " + token, nil
	default:
		return "", errors.New("invalid registry challenge")
	}
}
