// Package cftest is an in-memory Cloudflare API for tests. It serves the endpoints the cloudflare
// package calls, with Cloudflare's response envelope. It imports no testing package, so an end-to-end
// binary can use it too.
package cftest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"berthy/internal/cloudflare"
)

const (
	maxBody  = 1 << 20
	openedAt = "2026-11-15T00:00:00Z"
)

// Fake is an in-memory Cloudflare account. Serve it with Handler, seed it with the Add and Set methods,
// and inspect it with the getters. It is safe for concurrent use.
type Fake struct {
	Token     string // required bearer token; any other request gets 403 with code 10000
	AccountID string

	// ConfigPuts and RecordWrites count successful writes, for assertions.
	ConfigPuts   int
	RecordWrites int

	mu          sync.Mutex
	faults      []fault // checked in order before each request, under mu
	seq         int
	tunnels     map[string]cloudflare.Tunnel
	tunnelOrder []string
	configs     map[string]json.RawMessage
	versions    map[string]int
	zones       map[string]cloudflare.Zone
	zoneOrder   []string
	records     map[string][]cloudflare.DNSRecord // by zone id, in creation order
}

// New returns an empty account that accepts only token and belongs to accountID.
func New(token, accountID string) *Fake {
	return &Fake{
		Token:     token,
		AccountID: accountID,
		tunnels:   map[string]cloudflare.Tunnel{},
		configs:   map[string]json.RawMessage{},
		versions:  map[string]int{},
		zones:     map[string]cloudflare.Zone{},
		records:   map[string][]cloudflare.DNSRecord{},
	}
}

// fault answers requests that match a method and a path prefix with a status.
type fault struct {
	method, prefix string
	status         int
}

// Handler serves the Cloudflare API. Request paths may include the /client/v4 prefix or omit it.
func (f *Fake) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if status := f.faulted(r); status != 0 {
			writeFailure(w, status, 1000, "injected failure")
			return
		}
		f.serve(w, r)
	})
}

// FailRequests answers later requests whose method is method and whose path, without the /client/v4 prefix, starts
// with prefix, with status. It runs before the account is consulted, so the request changes nothing. A zero status
// removes the fault. For example, FailRequests("PUT", "/accounts/", 500) fails every tunnel configuration write.
func (f *Fake) FailRequests(method, prefix string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, ft := range f.faults {
		if ft.method == method && ft.prefix == prefix {
			f.faults = append(f.faults[:i], f.faults[i+1:]...)
			break
		}
	}
	if status != 0 {
		f.faults = append(f.faults, fault{method: method, prefix: prefix, status: status})
	}
}

// faulted returns the status of the first fault that matches r, or 0 when none does. The caller must hold f.mu.
func (f *Fake) faulted(r *http.Request) int {
	path := strings.TrimPrefix(r.URL.Path, "/client/v4")
	for _, ft := range f.faults {
		if r.Method == ft.method && strings.HasPrefix(path, ft.prefix) {
			return ft.status
		}
	}
	return 0
}

// AddZone adds an active zone to the account and returns its id.
func (f *Fake) AddZone(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.nextID()
	f.zones[id] = cloudflare.Zone{ID: id, Name: name, Status: "active"}
	f.zoneOrder = append(f.zoneOrder, id)
	return id
}

// AddTunnel adds an inactive tunnel and returns its id. Remote tunnels are remotely managed. Duplicate
// names are allowed here so tests can build the ambiguous state Berthy must refuse.
func (f *Fake) AddTunnel(name string, remote bool) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addTunnel(name, remote)
}

// DeleteTunnel marks a tunnel deleted. Like Cloudflare, it is listed only when deleted tunnels are asked for.
func (f *Fake) DeleteTunnel(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.mustTunnel(id)
	deleted := openedAt
	t.DeletedAt = &deleted
	f.tunnels[id] = t
}

// SetTunnelStatus sets a tunnel's status and gives it the given number of connections.
func (f *Fake) SetTunnelStatus(id, status string, connections int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.mustTunnel(id)
	t.Status = status
	t.Connections = make([]cloudflare.Connection, 0, connections)
	for i := 0; i < connections; i++ {
		t.Connections = append(t.Connections, cloudflare.Connection{
			ColoName:      "fake01",
			ID:            f.nextUUID(),
			ClientVersion: "2026.11.0",
			OpenedAt:      openedAt,
			OriginIP:      "203.0.113.10",
		})
	}
	f.tunnels[id] = t
}

// SetConfig stores a tunnel's configuration document, replacing any earlier one. An empty cfg removes it.
// Unlike the API, it does not require a catch-all rule, so tests can seed any state.
func (f *Fake) SetConfig(tunnelID string, cfg json.RawMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mustTunnel(tunnelID)
	if len(cfg) == 0 {
		delete(f.configs, tunnelID)
	} else {
		if !json.Valid(cfg) {
			panic("cftest: invalid tunnel configuration")
		}
		f.configs[tunnelID] = append(json.RawMessage(nil), cfg...)
	}
	f.versions[tunnelID]++
}

// Config returns a tunnel's configuration document, or nil when it has none.
func (f *Fake) Config(tunnelID string) json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append(json.RawMessage(nil), f.configs[tunnelID]...)
}

// AddRecord adds a record to a zone without the API's checks, so tests can build conflicting state.
// It returns the record's id.
func (f *Fake) AddRecord(zoneID string, r cloudflare.DNSRecord) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mustZone(zoneID)
	r.ID = f.nextID()
	if r.TTL == 0 {
		r.TTL = 1
	}
	f.records[zoneID] = append(f.records[zoneID], r)
	return r.ID
}

// Records returns a zone's records in creation order.
func (f *Fake) Records(zoneID string) []cloudflare.DNSRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cloudflare.DNSRecord(nil), f.records[zoneID]...)
}

// TunnelToken returns the deterministic token of a tunnel. A connector must be given this value.
func (f *Fake) TunnelToken(id string) string {
	return "fake-tunnel-token-" + id
}

// nextID returns a new 32-digit hex id. The caller must hold f.mu.
func (f *Fake) nextID() string {
	f.seq++
	return fmt.Sprintf("%032x", f.seq)
}

// nextUUID returns a new UUID-shaped id. The caller must hold f.mu.
func (f *Fake) nextUUID() string {
	f.seq++
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", f.seq, f.seq)
}

// addTunnel adds a tunnel. The caller must hold f.mu.
func (f *Fake) addTunnel(name string, remote bool) string {
	id := f.nextUUID()
	f.tunnels[id] = cloudflare.Tunnel{ID: id, Name: name, Status: "inactive", RemoteConfig: remote, Connections: []cloudflare.Connection{}}
	f.tunnelOrder = append(f.tunnelOrder, id)
	return id
}

// mustTunnel returns a tunnel and panics on an unknown id, which is a bug in the test that seeds it.
func (f *Fake) mustTunnel(id string) cloudflare.Tunnel {
	t, ok := f.tunnels[id]
	if !ok {
		panic("cftest: unknown tunnel " + id)
	}
	return t
}

func (f *Fake) mustZone(id string) {
	if _, ok := f.zones[id]; !ok {
		panic("cftest: unknown zone " + id)
	}
}

// serve routes one request. The caller must hold f.mu.
func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	if f.Token == "" || r.Header.Get("Authorization") != "Bearer "+f.Token {
		writeFailure(w, http.StatusForbidden, 10000, "Authentication error")
		return
	}
	p := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/client/v4"), "/"), "/")
	switch {
	case len(p) == 3 && p[0] == "user" && p[1] == "tokens" && p[2] == "verify":
		if allow(w, r, http.MethodGet) {
			writeResult(w, map[string]string{"status": "active"})
		}
	case len(p) == 4 && p[0] == "accounts" && p[2] == "tokens" && p[3] == "verify":
		if f.ownAccount(w, p[1]) && allow(w, r, http.MethodGet) {
			writeResult(w, map[string]string{"status": "active"})
		}
	case len(p) == 3 && p[0] == "accounts" && p[2] == "cfd_tunnel":
		if f.ownAccount(w, p[1]) {
			f.tunnelCollection(w, r)
		}
	case len(p) == 4 && p[0] == "accounts" && p[2] == "cfd_tunnel":
		if f.ownAccount(w, p[1]) {
			f.tunnel(w, r, p[3])
		}
	case len(p) == 5 && p[0] == "accounts" && p[2] == "cfd_tunnel" && p[4] == "token":
		if f.ownAccount(w, p[1]) {
			f.tunnelToken(w, r, p[3])
		}
	case len(p) == 5 && p[0] == "accounts" && p[2] == "cfd_tunnel" && p[4] == "configurations":
		if f.ownAccount(w, p[1]) {
			f.tunnelConfig(w, r, p[3])
		}
	case len(p) == 1 && p[0] == "zones":
		f.zoneList(w, r)
	case len(p) == 3 && p[0] == "zones" && p[2] == "dns_records":
		f.recordCollection(w, r, p[1])
	case len(p) == 4 && p[0] == "zones" && p[2] == "dns_records":
		f.record(w, r, p[1], p[3])
	default:
		writeNotFound(w)
	}
}

// ownAccount answers 404 for any account other than the fake's.
func (f *Fake) ownAccount(w http.ResponseWriter, id string) bool {
	if id != f.AccountID {
		writeNotFound(w)
		return false
	}
	return true
}

func (f *Fake) tunnelCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		includeDeleted := q.Get("is_deleted") == "true"
		list := []cloudflare.Tunnel{}
		for _, id := range f.tunnelOrder {
			t := f.tunnels[id]
			if q.Has("name") && t.Name != q.Get("name") {
				continue
			}
			if t.DeletedAt != nil && !includeDeleted {
				continue
			}
			list = append(list, t)
		}
		writeList(w, list, len(list))
	case http.MethodPost:
		var body struct {
			Name      string `json:"name"`
			ConfigSrc string `json:"config_src"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Name == "" {
			writeFailure(w, http.StatusBadRequest, 1001, "Tunnel name required")
			return
		}
		for _, t := range f.tunnels {
			if t.Name == body.Name && t.DeletedAt == nil {
				writeFailure(w, http.StatusConflict, 1013, "A tunnel with this name already exists")
				return
			}
		}
		t := f.tunnels[f.addTunnel(body.Name, body.ConfigSrc == "cloudflare")]
		writeResult(w, t)
	default:
		writeMethodNotAllowed(w)
	}
}

func (f *Fake) tunnel(w http.ResponseWriter, r *http.Request, id string) {
	if !allow(w, r, http.MethodGet) {
		return
	}
	t, ok := f.tunnels[id]
	if !ok {
		writeNotFound(w)
		return
	}
	writeResult(w, t)
}

func (f *Fake) tunnelToken(w http.ResponseWriter, r *http.Request, id string) {
	if !allow(w, r, http.MethodGet) {
		return
	}
	if _, ok := f.tunnels[id]; !ok {
		writeNotFound(w)
		return
	}
	writeResult(w, f.TunnelToken(id))
}

func (f *Fake) tunnelConfig(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := f.tunnels[id]; !ok {
		writeNotFound(w)
		return
	}
	switch r.Method {
	case http.MethodGet:
		var cfg json.RawMessage
		if raw, has := f.configs[id]; has {
			cfg = raw
		}
		writeResult(w, map[string]any{"tunnel_id": id, "version": f.versions[id], "config": cfg})
	case http.MethodPut:
		var body struct {
			Config json.RawMessage `json:"config"`
		}
		if !decode(w, r, &body) {
			return
		}
		var cfg cloudflare.TunnelConfig
		if len(body.Config) == 0 || json.Unmarshal(body.Config, &cfg) != nil || !catchAll(cfg) {
			writeFailure(w, http.StatusBadRequest, 1000, "The last ingress rule must match all URLs")
			return
		}
		f.configs[id] = append(json.RawMessage(nil), body.Config...)
		f.versions[id]++
		f.ConfigPuts++
		writeResult(w, map[string]any{"tunnel_id": id, "version": f.versions[id], "config": f.configs[id]})
	default:
		writeMethodNotAllowed(w)
	}
}

// catchAll reports whether the last rule matches every request, which the API requires.
func catchAll(cfg cloudflare.TunnelConfig) bool {
	n := len(cfg.Ingress)
	if n == 0 {
		return false
	}
	last := cfg.Ingress[n-1]
	return last.Hostname == "" && last.Path == "" && last.Service != ""
}

func (f *Fake) zoneList(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) {
		return
	}
	q := r.URL.Query()
	if q.Has("account.id") && q.Get("account.id") != f.AccountID {
		writeNotFound(w)
		return
	}
	list := []cloudflare.Zone{}
	for _, id := range f.zoneOrder {
		z := f.zones[id]
		if q.Has("name") && z.Name != q.Get("name") {
			continue
		}
		list = append(list, z)
	}
	writeList(w, list, len(list))
}

func (f *Fake) recordCollection(w http.ResponseWriter, r *http.Request, zoneID string) {
	if _, ok := f.zones[zoneID]; !ok {
		writeNotFound(w)
		return
	}
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		list := []cloudflare.DNSRecord{}
		for _, rec := range f.records[zoneID] {
			if q.Has("name") && rec.Name != q.Get("name") {
				continue
			}
			list = append(list, rec)
		}
		writeList(w, list, len(list))
	case http.MethodPost:
		var rec cloudflare.DNSRecord
		if !decode(w, r, &rec) {
			return
		}
		if !validRecord(rec) {
			writeFailure(w, http.StatusBadRequest, 1004, "DNS Validation Error")
			return
		}
		if conflicts(f.records[zoneID], "", rec) {
			writeFailure(w, http.StatusBadRequest, 81053, "An A, AAAA, or CNAME record with that host already exists.")
			return
		}
		rec.ID = f.nextID()
		if rec.TTL == 0 {
			rec.TTL = 1
		}
		f.records[zoneID] = append(f.records[zoneID], rec)
		f.RecordWrites++
		writeResult(w, rec)
	default:
		writeMethodNotAllowed(w)
	}
}

func (f *Fake) record(w http.ResponseWriter, r *http.Request, zoneID, id string) {
	if _, ok := f.zones[zoneID]; !ok {
		writeNotFound(w)
		return
	}
	idx := -1
	for i, rec := range f.records[zoneID] {
		if rec.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeNotFound(w)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var rec cloudflare.DNSRecord
		if !decode(w, r, &rec) {
			return
		}
		if !validRecord(rec) {
			writeFailure(w, http.StatusBadRequest, 1004, "DNS Validation Error")
			return
		}
		if conflicts(f.records[zoneID], id, rec) {
			writeFailure(w, http.StatusBadRequest, 81053, "An A, AAAA, or CNAME record with that host already exists.")
			return
		}
		rec.ID = id
		if rec.TTL == 0 {
			rec.TTL = 1
		}
		f.records[zoneID][idx] = rec
		f.RecordWrites++
		writeResult(w, rec)
	case http.MethodDelete:
		f.records[zoneID] = append(f.records[zoneID][:idx], f.records[zoneID][idx+1:]...)
		f.RecordWrites++
		writeResult(w, map[string]string{"id": id})
	default:
		writeMethodNotAllowed(w)
	}
}

// conflicts reports whether rec would share a name with a CNAME, or would be a CNAME beside another
// record. Cloudflare rejects both with code 81053. self is the id of the record being replaced, if any.
func conflicts(existing []cloudflare.DNSRecord, self string, rec cloudflare.DNSRecord) bool {
	for _, e := range existing {
		if e.ID == self || !strings.EqualFold(e.Name, rec.Name) {
			continue
		}
		if e.Type == "CNAME" || rec.Type == "CNAME" {
			return true
		}
	}
	return false
}

func validRecord(r cloudflare.DNSRecord) bool {
	return r.Type != "" && r.Name != "" && r.Content != ""
}

// allow answers 405 unless the request uses the method.
func allow(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		writeMethodNotAllowed(w)
		return false
	}
	return true
}

// decode reads a JSON request body and answers 400 when it is not valid.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err == nil {
		err = json.Unmarshal(b, v)
	}
	if err != nil {
		writeFailure(w, http.StatusBadRequest, 1001, "Bad request body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeResult(w http.ResponseWriter, result any) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "errors": []any{}, "messages": []any{}, "result": result})
}

func writeList(w http.ResponseWriter, list any, n int) {
	writeJSON(w, http.StatusOK, map[string]any{
		"success":     true,
		"errors":      []any{},
		"messages":    []any{},
		"result":      list,
		"result_info": map[string]int{"page": 1, "per_page": 20, "count": n, "total_count": n},
	})
}

func writeFailure(w http.ResponseWriter, status, code int, message string) {
	writeJSON(w, status, map[string]any{
		"success":  false,
		"errors":   []map[string]any{{"code": code, "message": message}},
		"messages": []any{},
		"result":   nil,
	})
}

func writeNotFound(w http.ResponseWriter) {
	writeFailure(w, http.StatusNotFound, 7003, "Could not route to the request")
}

func writeMethodNotAllowed(w http.ResponseWriter) {
	writeFailure(w, http.StatusMethodNotAllowed, 7000, "Method not allowed")
}
