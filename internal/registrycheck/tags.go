package registrycheck

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

const maxTagPages = 100
const maxTags = 5000

var tagPattern = regexp.MustCompile(`^[\w][\w.-]{0,127}$`)
var repoPattern = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*)*$`)

// Tags returns a complete, validated list of repository tags.
func (p Probe) Tags(ctx context.Context, repo string) ([]string, error) {
	if !repoPattern.MatchString(repo) {
		return nil, errors.New("invalid repository")
	}
	origin, err := listingOrigin(p.Host)
	if err != nil {
		return nil, err
	}
	list := *origin
	list.Path = "/v2/" + repo + "/tags/list"
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client := http.Client{Timeout: 15 * time.Second}
	if p.HTTP != nil {
		client = *p.HTTP
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	request := func(u *url.URL, auth string) (int, http.Header, []byte, error) {
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if e != nil {
			return 0, nil, nil, errors.New("invalid registry request")
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, e := client.Do(req)
		if e != nil {
			return 0, nil, nil, errors.New("registry request failed")
		}
		defer resp.Body.Close()
		// Error bodies can contain credentials and are never needed.
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, resp.Header, nil, nil
		}
		body, e := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
		if e != nil || len(body) > maxResponse {
			return 0, nil, nil, errors.New("invalid registry response")
		}
		return resp.StatusCode, resp.Header, body, nil
	}
	current := &list
	auth := ""
	seenURLs := map[string]bool{}
	tags := map[string]bool{}
	count := 0
	for page := 0; page < maxTagPages; page++ {
		if seenURLs[current.String()] {
			return nil, errors.New("registry pagination loop")
		}
		seenURLs[current.String()] = true
		status, header, body, e := request(current, auth)
		if e != nil {
			return nil, e
		}
		if page == 0 && status == http.StatusUnauthorized {
			scheme, params, ok := parseChallenge(header.Values("WWW-Authenticate"))
			if !ok {
				return nil, errors.New("invalid registry challenge")
			}
			switch scheme {
			case "basic":
				auth = p.basic()
			case "bearer":
				realm, e := url.Parse(params["realm"])
				if e != nil || !trustedRealm(origin, realm, params["service"]) {
					return nil, errors.New("untrusted registry token issuer")
				}
				q := realm.Query()
				if params["service"] != "" {
					q.Set("service", params["service"])
				}
				q.Set("scope", "repository:"+repo+":pull")
				realm.RawQuery = q.Encode()
				tokenAuth := ""
				if p.Username != "" || p.Password != "" {
					tokenAuth = p.basic()
				}
				s, _, b, e := request(realm, tokenAuth)
				if e != nil {
					return nil, e
				}
				if s == 401 || s == 403 || s == 404 {
					return nil, ErrDenied
				}
				if s != 200 {
					return nil, errors.New("registry token request failed")
				}
				var issued struct {
					Token       string `json:"token"`
					AccessToken string `json:"access_token"`
				}
				if json.Unmarshal(b, &issued) != nil {
					return nil, errors.New("invalid registry token response")
				}
				token := issued.Token
				if token == "" {
					token = issued.AccessToken
				}
				if token == "" || strings.ContainsAny(token, "\r\n") {
					return nil, errors.New("invalid registry token response")
				}
				auth = "Bearer " + token
			default:
				return nil, errors.New("invalid registry challenge")
			}
			status, header, body, e = request(current, auth)
			if e != nil {
				return nil, e
			}
		}
		if status == 401 || status == 403 || status == 404 {
			return nil, ErrDenied
		}
		if status != 200 {
			return nil, errors.New("registry listing request failed")
		}
		var result struct {
			Name string          `json:"name"`
			Tags json.RawMessage `json:"tags"`
		}
		if json.Unmarshal(body, &result) != nil || result.Name != repo || len(result.Tags) == 0 {
			return nil, errors.New("invalid registry listing")
		}
		var values []string
		if json.Unmarshal(result.Tags, &values) != nil {
			return nil, errors.New("invalid registry listing")
		}
		count += len(values)
		if count > maxTags {
			return nil, errors.New("registry tag limit exceeded")
		}
		before := len(tags)
		for _, tag := range values {
			if !tagPattern.MatchString(tag) {
				return nil, errors.New("invalid registry tag")
			}
			tags[tag] = true
		}
		next, e := nextListing(header.Values("Link"), current, &list)
		if e != nil {
			return nil, e
		}
		if next == nil {
			out := make([]string, 0, len(tags))
			for tag := range tags {
				out = append(out, tag)
			}
			sort.Strings(out)
			return out, nil
		}
		if string(result.Tags) == "null" || len(tags) == before {
			return nil, errors.New("registry pagination did not progress")
		}
		current = next
	}
	return nil, errors.New("registry page limit exceeded")
}

func listingOrigin(host string) (*url.URL, error) {
	u, err := url.Parse("https://" + host)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid registry origin")
	}
	if isLoopback(u.Hostname()) {
		u.Scheme = "http"
	}
	h := strings.ToLower(u.Hostname())
	if (h == "docker.io" || h == "index.docker.io" || h == "registry-1.docker.io") && (u.Port() == "" || u.Port() == "443") {
		u.Host = "registry-1.docker.io"
	}
	return u, nil
}

func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if u.Port() != "" {
			return u.Port()
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

func trustedRealm(origin, realm *url.URL, service string) bool {
	if realm == nil || !realm.IsAbs() || realm.Hostname() == "" || realm.User != nil || realm.Fragment != "" || realm.Opaque != "" {
		return false
	}
	if sameOrigin(origin, realm) {
		return true
	}
	return origin.Scheme == "https" && strings.EqualFold(origin.Hostname(), "registry-1.docker.io") && (origin.Port() == "" || origin.Port() == "443") && realm.Scheme == "https" && strings.EqualFold(realm.Hostname(), "auth.docker.io") && (realm.Port() == "" || realm.Port() == "443") && realm.Path == "/token" && service == "registry.docker.io"
}

func nextListing(headers []string, current, list *url.URL) (*url.URL, error) {
	var next *url.URL
	for _, h := range headers {
		// Registry continuation links have a single URI reference and rel parameter.
		for _, part := range strings.Split(h, ",") {
			part = strings.TrimSpace(part)
			end := strings.Index(part, ">")
			if !strings.HasPrefix(part, "<") || end < 0 {
				return nil, errors.New("invalid registry continuation")
			}
			params := strings.Split(part[end+1:], ";")
			isNext := false
			hasRel := false
			for _, p := range params {
				k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
				if ok && strings.EqualFold(k, "rel") {
					if hasRel {
						return nil, errors.New("ambiguous registry continuation")
					}
					hasRel = true
					for _, rel := range strings.Fields(strings.Trim(v, `"`)) {
						if strings.EqualFold(rel, "next") {
							isNext = true
						}
					}
				}
			}
			if !isNext {
				continue
			}
			if next != nil {
				return nil, errors.New("ambiguous registry continuation")
			}
			ref, err := url.Parse(part[1:end])
			if err != nil {
				return nil, errors.New("invalid registry continuation")
			}
			u := current.ResolveReference(ref)
			if u.User != nil || u.Fragment != "" || !sameOrigin(list, u) || u.Path != list.Path || u.RawPath != "" {
				return nil, errors.New("invalid registry continuation")
			}
			next = u
		}
	}
	return next, nil
}
