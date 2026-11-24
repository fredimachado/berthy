package registrycheck

import (
	"bytes"
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
	flight := newRegistryFlight(ctx, p, origin, repo)
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
		status, header, body, e := flight.request(http.MethodGet, current, auth, "")
		if e != nil {
			return nil, e
		}
		if page == 0 && status == http.StatusUnauthorized {
			auth, e = flight.authenticate(header)
			if e != nil {
				return nil, e
			}
			status, header, body, e = flight.request(http.MethodGet, current, auth, "")
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
		values, nullTags, e := parseTagPage(body, repo)
		if e != nil {
			return nil, e
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
		if nullTags || len(tags) == before {
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
	return origin.Scheme == "https" && strings.EqualFold(origin.Hostname(), "registry-1.docker.io") && (origin.Port() == "" || origin.Port() == "443") && realm.Scheme == "https" && strings.EqualFold(realm.Hostname(), "auth.docker.io") && (realm.Port() == "" || realm.Port() == "443") && realm.EscapedPath() == "/token" && realm.RawQuery == "" && !realm.ForceQuery && service == "registry.docker.io"
}

// parseTagPage checks protocol fields before decoding their values; unknown fields remain compatible.
func parseTagPage(body []byte, repo string) ([]string, bool, error) {
	invalid := errors.New("invalid registry listing")
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, false, invalid
	}
	var name string
	var rawTags json.RawMessage
	seenName, seenTags := false, false
	for dec.More() {
		tok, err = dec.Token()
		if err != nil {
			return nil, false, invalid
		}
		key, ok := tok.(string)
		if !ok {
			return nil, false, invalid
		}
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			return nil, false, invalid
		}
		switch {
		case strings.EqualFold(key, "name"):
			if key != "name" || seenName || json.Unmarshal(raw, &name) != nil {
				return nil, false, invalid
			}
			seenName = true
		case strings.EqualFold(key, "tags"):
			if key != "tags" || seenTags {
				return nil, false, invalid
			}
			seenTags, rawTags = true, raw
		}
	}
	if tok, err = dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, false, invalid
	}
	if dec.Decode(new(json.RawMessage)) != io.EOF || !seenName || !seenTags || name != repo {
		return nil, false, invalid
	}
	var tags []string
	if json.Unmarshal(rawTags, &tags) != nil {
		return nil, false, invalid
	}
	return tags, string(rawTags) == "null", nil
}

// splitLink respects URI references and quoted strings, including quoted-pair escapes.
func splitLink(s string, sep byte) ([]string, error) {
	var parts []string
	start := 0
	angle, quoted, escaped := false, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 32 && c != '\t' || c == 127 {
			return nil, errors.New("invalid registry continuation")
		}
		if escaped {
			escaped = false
			continue
		}
		if quoted {
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		if angle {
			if c == '>' {
				angle = false
			} else if c == '<' {
				return nil, errors.New("invalid registry continuation")
			}
			continue
		}
		switch c {
		case '<':
			angle = true
		case '>':
			return nil, errors.New("invalid registry continuation")
		case '"':
			quoted = true
		default:
			if c == sep {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	if angle || quoted || escaped {
		return nil, errors.New("invalid registry continuation")
	}
	return append(parts, s[start:]), nil
}

func linkToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c))) {
			return false
		}
	}
	return true
}

func linkValue(s string) (string, bool) {
	if !strings.HasPrefix(s, `"`) {
		return s, linkToken(s)
	}
	if len(s) < 2 || s[len(s)-1] != '"' {
		return "", false
	}
	var b strings.Builder
	for i := 1; i < len(s)-1; i++ {
		if s[i] == '"' {
			return "", false
		}
		if s[i] == '\\' {
			i++
			if i >= len(s)-1 {
				return "", false
			}
		}
		b.WriteByte(s[i])
	}
	return b.String(), true
}

func nextListing(headers []string, current, list *url.URL) (*url.URL, error) {
	invalid := errors.New("invalid registry continuation")
	var next *url.URL
	for _, h := range headers {
		links, err := splitLink(h, ',')
		if err != nil {
			return nil, err
		}
		for _, link := range links {
			parts, err := splitLink(link, ';')
			if err != nil {
				return nil, err
			}
			target := strings.TrimSpace(parts[0])
			if len(target) < 2 || target[0] != '<' || target[len(target)-1] != '>' {
				return nil, invalid
			}
			isNext, hasRel := false, false
			for _, part := range parts[1:] {
				key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
				key, value = strings.TrimSpace(key), strings.TrimSpace(value)
				if !linkToken(key) {
					return nil, invalid
				}
				if !ok {
					if strings.EqualFold(key, "rel") {
						return nil, invalid
					}
					continue
				}
				value, ok = linkValue(value)
				if !ok {
					return nil, invalid
				}
				if strings.EqualFold(key, "rel") {
					if hasRel || len(strings.Fields(value)) == 0 {
						return nil, invalid
					}
					hasRel = true
					for _, rel := range strings.Fields(value) {
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
			ref, err := url.Parse(target[1 : len(target)-1])
			if err != nil {
				return nil, invalid
			}
			u := current.ResolveReference(ref)
			if u.User != nil || u.Fragment != "" || !sameOrigin(list, u) || u.Path != list.Path || u.RawPath != "" {
				return nil, invalid
			}
			next = u
		}
	}
	return next, nil
}
