// Package cloudflare is the part of the Cloudflare v4 API that managed ingress uses: tunnels, their
// remotely managed configuration, zones and DNS records. It uses only the standard library.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"berthy/internal/scope"
)

// DefaultBaseURL is the production Cloudflare API.
const DefaultBaseURL = "https://api.cloudflare.com/client/v4"

// ManagedComment marks the DNS records Berthy creates. RecordComment adds the hosted environment to it.
const ManagedComment = "managed by berthy"

// ErrZoneNotFound means no zone in the account holds the hostname.
var ErrZoneNotFound = errors.New("cloudflare zone not found for hostname")

// ErrTokenInactive means Cloudflare answered that the API token is not active.
var ErrTokenInactive = errors.New("cloudflare API token is not active")

// maxResponse bounds how much of a response body is read.
const maxResponse = 4 << 20

// Client calls the Cloudflare API with one API token.
type Client struct {
	BaseURL string       // no trailing slash; DefaultBaseURL when empty
	Token   string       // API token; sent as "Authorization: Bearer"; never logged or put in errors
	HTTP    *http.Client // nil => &http.Client{Timeout: 30 * time.Second}
}

// APIError is returned for non-2xx or success=false. Its Error() never includes the token or response bodies, only status and codes.
type APIError struct {
	Status int
	Codes  []int

	// failed is set when the response envelope reported success=false.
	failed bool
}

func (e *APIError) Error() string {
	if len(e.Codes) == 0 {
		return fmt.Sprintf("cloudflare API request failed with HTTP %d", e.Status)
	}
	return fmt.Sprintf("cloudflare API request failed with HTTP %d, error codes %v", e.Status, e.Codes)
}

// Tunnel is a Cloudflare Tunnel.
type Tunnel struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Status       string       `json:"status"` // healthy, degraded, down, inactive
	RemoteConfig bool         `json:"remote_config"`
	Connections  []Connection `json:"connections"`
	DeletedAt    *string      `json:"deleted_at"`
}

// Connection is one connector's connection to Cloudflare.
type Connection struct {
	ColoName      string `json:"colo_name"`
	ID            string `json:"id"`
	ClientVersion string `json:"client_version"`
	OpenedAt      string `json:"opened_at"`
	OriginIP      string `json:"origin_ip"`
}

// Zone is a domain in the account.
type Zone struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// DNSRecord is a DNS record in a zone.
type DNSRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl,omitempty"` // 1 = automatic
	Comment string `json:"comment,omitempty"`
}

// Rule is one ingress rule. Unknown fields (originRequest, path, ...) are preserved byte-for-byte across Get/Put.
type Rule struct {
	Hostname string
	Service  string
	Path     string
	Extra    map[string]json.RawMessage // every other field, re-emitted on MarshalJSON
}

// TunnelConfig is the remotely managed configuration document. Fields other than ingress are preserved.
type TunnelConfig struct {
	Ingress []Rule
	Extra   map[string]json.RawMessage
}

var ruleFields = map[string]bool{"hostname": true, "service": true, "path": true}

// MarshalJSON writes the typed fields and then every preserved field. Empty hostname and path are omitted.
func (r Rule) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(r.Extra)+3)
	for k, v := range r.Extra {
		if !ruleFields[k] {
			out[k] = v
		}
	}
	var err error
	if r.Hostname != "" {
		if out["hostname"], err = json.Marshal(r.Hostname); err != nil {
			return nil, err
		}
	}
	if out["service"], err = json.Marshal(r.Service); err != nil {
		return nil, err
	}
	if r.Path != "" {
		if out["path"], err = json.Marshal(r.Path); err != nil {
			return nil, err
		}
	}
	return json.Marshal(out)
}

// UnmarshalJSON reads the typed fields and keeps every other field in Extra.
func (r *Rule) UnmarshalJSON(b []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	*r = Rule{}
	for k, v := range fields {
		var err error
		switch k {
		case "hostname":
			err = json.Unmarshal(v, &r.Hostname)
		case "service":
			err = json.Unmarshal(v, &r.Service)
		case "path":
			err = json.Unmarshal(v, &r.Path)
		default:
			if r.Extra == nil {
				r.Extra = map[string]json.RawMessage{}
			}
			r.Extra[k] = v
		}
		if err != nil {
			return fmt.Errorf("invalid ingress rule field %q", k)
		}
	}
	return nil
}

// MarshalJSON writes ingress as an array (never null) and then every preserved field.
func (c TunnelConfig) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(c.Extra)+1)
	for k, v := range c.Extra {
		if k != "ingress" {
			out[k] = v
		}
	}
	rules := c.Ingress
	if rules == nil {
		rules = []Rule{}
	}
	ingress, err := json.Marshal(rules)
	if err != nil {
		return nil, err
	}
	out["ingress"] = ingress
	return json.Marshal(out)
}

// UnmarshalJSON reads ingress and keeps every other field in Extra.
func (c *TunnelConfig) UnmarshalJSON(b []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	*c = TunnelConfig{}
	for k, v := range fields {
		if k == "ingress" {
			if err := json.Unmarshal(v, &c.Ingress); err != nil {
				return errors.New("invalid tunnel ingress")
			}
			continue
		}
		if c.Extra == nil {
			c.Extra = map[string]json.RawMessage{}
		}
		c.Extra[k] = v
	}
	return nil
}

// TunnelTarget is the address a tunnel's public hostname CNAMEs to.
func TunnelTarget(tunnelID string) string {
	return tunnelID + ".cfargotunnel.com"
}

// RecordComment is the comment on a DNS record Berthy created for the hosted environment id.
func RecordComment(id scope.ID) string {
	return ManagedComment + " (" + id.String() + ")"
}

// IsManaged reports whether r was created by Berthy for the hosted environment id.
func IsManaged(r DNSRecord, id scope.ID) bool {
	return r.Comment == RecordComment(id)
}

// ManagedBy reports the hosted environment named in r's managed mark, when the comment is one.
func ManagedBy(r DNSRecord) (scope.ID, bool) {
	prefix, suffix := ManagedComment+" (", ")"
	if !strings.HasPrefix(r.Comment, prefix) || !strings.HasSuffix(r.Comment, suffix) {
		return scope.ID{}, false
	}
	id, err := scope.Parse(strings.TrimSuffix(strings.TrimPrefix(r.Comment, prefix), suffix))
	if err != nil {
		return scope.ID{}, false
	}
	return id, true
}

// TokenStatus is what Cloudflare says about the API token a client carries.
type TokenStatus struct {
	Status  string    // "active", "disabled" or "expired"
	Expires time.Time // when the token stops working; zero when Cloudflare names no expiry
}

// TokenStatus asks Cloudflare about the token. It tries the user endpoint first and falls back to the account
// endpoint when the token is account-owned.
func (c *Client) TokenStatus(ctx context.Context, accountID string) (TokenStatus, error) {
	st, err := c.tokenStatus(ctx, "/user/tokens/verify")
	if err != nil && userEndpointRefused(err) {
		st, err = c.tokenStatus(ctx, "/accounts/"+url.PathEscape(accountID)+"/tokens/verify")
	}
	return st, err
}

// VerifyToken checks that the token is active. It tries the user endpoint first and falls back to
// the account endpoint when the token is account-owned.
func (c *Client) VerifyToken(ctx context.Context, accountID string) error {
	st, err := c.TokenStatus(ctx, accountID)
	if err != nil {
		return err
	}
	if st.Status != "active" {
		return ErrTokenInactive
	}
	return nil
}

func userEndpointRefused(err error) bool {
	var api *APIError
	if !errors.As(err, &api) {
		return false
	}
	return api.failed || api.Status == http.StatusUnauthorized || api.Status == http.StatusForbidden || api.Status == http.StatusNotFound
}

func (c *Client) tokenStatus(ctx context.Context, path string) (TokenStatus, error) {
	var out struct {
		Status    string `json:"status"`
		ExpiresOn string `json:"expires_on"`
	}
	if err := c.call(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return TokenStatus{}, err
	}
	st := TokenStatus{Status: out.Status}
	// An expiry Berthy cannot read is no expiry: a wrong claim is worse than none, and the check must not fail over it.
	if t, err := time.Parse(time.RFC3339, out.ExpiresOn); err == nil {
		st.Expires = t
	}
	return st, nil
}

// Refused reports whether Cloudflare answered and refused the token or the request. Only an answer counts: an inactive
// token, a hostname whose zone the account does not hold, or an API error with a status below 500 other than 429 (rate
// limiting). A server error, a rate limit, a connection failure, a timeout or a malformed answer means Cloudflare could
// not be asked, so the caller must treat the check as "Cloudflare could not be asked".
func Refused(err error) bool {
	if errors.Is(err, ErrTokenInactive) || errors.Is(err, ErrZoneNotFound) {
		return true
	}
	var api *APIError
	return errors.As(err, &api) && api.Status < 500 && api.Status != http.StatusTooManyRequests
}

// FindTunnel returns the non-deleted tunnel with exactly this name, or nil when there is none.
func (c *Client) FindTunnel(ctx context.Context, accountID, name string) (*Tunnel, error) {
	var tunnels []Tunnel
	q := url.Values{"name": {name}, "is_deleted": {"false"}}
	if err := c.call(ctx, http.MethodGet, accountPath(accountID)+"/cfd_tunnel", q, nil, &tunnels); err != nil {
		return nil, err
	}
	var matches []Tunnel
	for _, t := range tunnels {
		if t.Name == name && t.DeletedAt == nil {
			matches = append(matches, t)
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return &matches[0], nil
	default:
		return nil, fmt.Errorf("%d tunnels are named %q; resolve the duplicates in Cloudflare", len(matches), name)
	}
}

// CreateTunnel creates a remotely managed tunnel.
func (c *Client) CreateTunnel(ctx context.Context, accountID, name string) (Tunnel, error) {
	var t Tunnel
	body := struct {
		Name      string `json:"name"`
		ConfigSrc string `json:"config_src"`
	}{Name: name, ConfigSrc: "cloudflare"}
	err := c.call(ctx, http.MethodPost, accountPath(accountID)+"/cfd_tunnel", nil, body, &t)
	return t, err
}

// GetTunnel returns one tunnel by id.
func (c *Client) GetTunnel(ctx context.Context, accountID, id string) (Tunnel, error) {
	var t Tunnel
	err := c.call(ctx, http.MethodGet, tunnelPath(accountID, id), nil, nil, &t)
	return t, err
}

// TunnelToken returns the token the connector uses to run the tunnel.
func (c *Client) TunnelToken(ctx context.Context, accountID, id string) (string, error) {
	var token string
	if err := c.call(ctx, http.MethodGet, tunnelPath(accountID, id)+"/token", nil, nil, &token); err != nil {
		return "", err
	}
	if token == "" {
		return "", errors.New("cloudflare returned an empty tunnel token")
	}
	return token, nil
}

// GetConfig returns the tunnel's remotely managed configuration. A missing config is an empty TunnelConfig.
func (c *Client) GetConfig(ctx context.Context, accountID, id string) (TunnelConfig, error) {
	var out struct {
		Config json.RawMessage `json:"config"`
	}
	if err := c.call(ctx, http.MethodGet, tunnelPath(accountID, id)+"/configurations", nil, nil, &out); err != nil {
		return TunnelConfig{}, err
	}
	if len(out.Config) == 0 || string(out.Config) == "null" {
		return TunnelConfig{}, nil
	}
	var cfg TunnelConfig
	if err := json.Unmarshal(out.Config, &cfg); err != nil {
		return TunnelConfig{}, errors.New("invalid tunnel configuration")
	}
	return cfg, nil
}

// PutConfig replaces the tunnel's remotely managed configuration.
func (c *Client) PutConfig(ctx context.Context, accountID, id string, cfg TunnelConfig) error {
	body := struct {
		Config TunnelConfig `json:"config"`
	}{Config: cfg}
	return c.call(ctx, http.MethodPut, tunnelPath(accountID, id)+"/configurations", nil, body, nil)
}

// FindZone returns the zone that holds hostname. It tries the full name, then each parent name down to
// the second-level domain. It returns ErrZoneNotFound when no zone matches.
func (c *Client) FindZone(ctx context.Context, accountID, hostname string) (Zone, error) {
	labels := strings.Split(hostname, ".")
	for i := 0; i < len(labels)-1; i++ {
		var zones []Zone
		q := url.Values{"name": {strings.Join(labels[i:], ".")}, "account.id": {accountID}}
		if err := c.call(ctx, http.MethodGet, "/zones", q, nil, &zones); err != nil {
			return Zone{}, err
		}
		if len(zones) > 0 {
			return zones[0], nil
		}
	}
	return Zone{}, ErrZoneNotFound
}

// maxPages bounds a paged listing, so a server that ignores the page number cannot hold the client forever.
const maxPages = 200

// Zones lists every zone in the account, following the pages. Cloudflare serves at most 50 zones a page.
func (c *Client) Zones(ctx context.Context, accountID string) ([]Zone, error) {
	const perPage = 50
	var all []Zone
	for page := 1; page <= maxPages; page++ {
		var zones []Zone
		q := url.Values{"account.id": {accountID}, "per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}
		if err := c.call(ctx, http.MethodGet, "/zones", q, nil, &zones); err != nil {
			return nil, err
		}
		all = append(all, zones...)
		if len(zones) < perPage {
			return all, nil
		}
	}
	return nil, errors.New("too many zones")
}

// DNSRecordsByContent lists the records in a zone, of any name and type, whose content is exactly content. The comparison
// is case-insensitive, as Cloudflare's is, and is repeated here so a server that ignores the filter cannot add records.
func (c *Client) DNSRecordsByContent(ctx context.Context, zoneID, content string) ([]DNSRecord, error) {
	const perPage = 100
	var out []DNSRecord
	for page := 1; page <= maxPages; page++ {
		var records []DNSRecord
		q := url.Values{"content.exact": {content}, "per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}
		if err := c.call(ctx, http.MethodGet, zonePath(zoneID)+"/dns_records", q, nil, &records); err != nil {
			return nil, err
		}
		for _, r := range records {
			if strings.EqualFold(r.Content, content) {
				out = append(out, r)
			}
		}
		if len(records) < perPage {
			return out, nil
		}
	}
	return nil, errors.New("too many records")
}

// DeleteTunnel deletes a tunnel. Cloudflare refuses a tunnel that has active connections.
func (c *Client) DeleteTunnel(ctx context.Context, accountID, id string) error {
	return c.call(ctx, http.MethodDelete, tunnelPath(accountID, id), nil, nil, nil)
}

// DNSRecords lists the records in a zone with exactly this name.
func (c *Client) DNSRecords(ctx context.Context, zoneID, name string) ([]DNSRecord, error) {
	var records []DNSRecord
	q := url.Values{"name": {name}, "per_page": {"100"}}
	err := c.call(ctx, http.MethodGet, zonePath(zoneID)+"/dns_records", q, nil, &records)
	return records, err
}

// CreateDNSRecord creates a record in a zone.
func (c *Client) CreateDNSRecord(ctx context.Context, zoneID string, r DNSRecord) (DNSRecord, error) {
	var out DNSRecord
	err := c.call(ctx, http.MethodPost, zonePath(zoneID)+"/dns_records", nil, r, &out)
	return out, err
}

// UpdateDNSRecord replaces a record in a zone.
func (c *Client) UpdateDNSRecord(ctx context.Context, zoneID, id string, r DNSRecord) (DNSRecord, error) {
	var out DNSRecord
	err := c.call(ctx, http.MethodPut, zonePath(zoneID)+"/dns_records/"+url.PathEscape(id), nil, r, &out)
	return out, err
}

// DeleteDNSRecord removes a record from a zone.
func (c *Client) DeleteDNSRecord(ctx context.Context, zoneID, id string) error {
	return c.call(ctx, http.MethodDelete, zonePath(zoneID)+"/dns_records/"+url.PathEscape(id), nil, nil, nil)
}

func accountPath(accountID string) string {
	return "/accounts/" + url.PathEscape(accountID)
}

func tunnelPath(accountID, id string) string {
	return accountPath(accountID) + "/cfd_tunnel/" + url.PathEscape(id)
}

func zonePath(zoneID string) string {
	return "/zones/" + url.PathEscape(zoneID)
}

// envelope is the common Cloudflare response wrapper.
type envelope struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code int `json:"code"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

func (e envelope) codes() []int {
	var codes []int
	for _, m := range e.Errors {
		codes = append(codes, m.Code)
	}
	return codes
}

func (c *Client) baseURL() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return strings.TrimRight(c.BaseURL, "/")
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// call sends one request. A non-nil result receives the envelope's result field.
func (c *Client) call(ctx context.Context, method, path string, query url.Values, body, result any) error {
	target := c.baseURL() + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return err
	}
	if len(raw) > maxResponse {
		return errors.New("cloudflare response too large")
	}
	var env envelope
	parseErr := json.Unmarshal(raw, &env)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Status: resp.StatusCode, Codes: env.codes(), failed: parseErr == nil && !env.Success}
	}
	if parseErr != nil {
		return errors.New("invalid cloudflare response")
	}
	if !env.Success {
		return &APIError{Status: resp.StatusCode, Codes: env.codes(), failed: true}
	}
	if result != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, result); err != nil {
			return errors.New("invalid cloudflare result")
		}
	}
	return nil
}
