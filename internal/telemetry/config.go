package telemetry

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// metricsPath is the OTLP/HTTP path that carries metrics.
const metricsPath = "/v1/metrics"

// Config is where the exporter sends its data. Endpoint is the OTLP/HTTP base URL, such as http://10.0.0.5:4318, and
// Headers are added to every request.
type Config struct {
	Endpoint string
	Headers  map[string]string
}

// ParseHeaders parses headers in the OpenTelemetry form name=value,name=value. Values are percent-decoded, as the
// OpenTelemetry specification requires, so a literal plus sign stays a plus sign. Spaces around names and values are
// trimmed, and empty entries are skipped. An error names the entry's position and never includes a name or a value.
func ParseHeaders(s string) (map[string]string, error) {
	headers := map[string]string{}
	if strings.TrimSpace(s) == "" {
		return headers, nil
	}
	for i, entry := range strings.Split(s, ",") {
		n := i + 1
		if strings.TrimSpace(entry) == "" {
			continue
		}
		name, value, found := strings.Cut(entry, "=")
		if !found {
			return nil, fmt.Errorf("header entry %d: expected name=value", n)
		}
		name = strings.TrimSpace(name)
		if !validHeaderName(name) {
			return nil, fmt.Errorf("header entry %d: name is not a valid header name", n)
		}
		decoded, err := url.PathUnescape(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("header entry %d: value is not valid percent-encoding", n)
		}
		if !validHeaderValue(decoded) {
			return nil, fmt.Errorf("header entry %d: value contains a control character", n)
		}
		headers[name] = decoded
	}
	return headers, nil
}

// validHeaderName reports whether s is a non-empty HTTP token, as RFC 9110 defines a field name.
func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// validHeaderValue rejects control characters other than tab, which would split or corrupt the request.
func validHeaderValue(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}

// MetricsURL returns the URL that metrics are posted to for a base endpoint. The metrics path is added unless the endpoint
// already ends with it, and a trailing slash is dropped first. The endpoint must be an http or https URL with a host. It
// must not carry user information, a query or a fragment, because those could hold credentials.
func MetricsURL(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("OTLP endpoint must be an http or https URL with a host, and no user information, query or fragment")
	}
	p := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(p, metricsPath) {
		p += metricsPath
	}
	u.Path = p
	u.RawPath = ""
	return u.String(), nil
}

// endpointTarget returns scheme://host of a metrics URL. It is what the doctor shows, without the path.
func endpointTarget(metricsURL string) string {
	u, err := url.Parse(metricsURL)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
