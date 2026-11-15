package cftest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"berthy/internal/cloudflare"
)

const testToken = "test-token-value"

// newClient starts the fake behind an httptest server and returns a client that calls it the way the agent does.
func newClient(t *testing.T, f *Fake) *cloudflare.Client {
	t.Helper()
	srv := httptest.NewServer(f.Handler())
	t.Cleanup(srv.Close)
	return &cloudflare.Client{BaseURL: srv.URL + "/client/v4", Token: testToken}
}

func TestSnapshotLogsSuccessfulWritesInOrder(t *testing.T) {
	f := New(testToken, "acct")
	zone := f.AddZone("example.com")
	c := newClient(t, f)
	ctx := context.Background()
	tunnel, err := c.CreateTunnel(ctx, "acct", "t1")
	if err != nil {
		t.Fatal(err)
	}
	cfg := cloudflare.TunnelConfig{Ingress: []cloudflare.Rule{{Service: "http_status:404"}}}
	if err := c.PutConfig(ctx, "acct", tunnel.ID, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateDNSRecord(ctx, zone, cloudflare.DNSRecord{Type: "CNAME", Name: "a.example.com", Content: "x.cfargotunnel.com", Proxied: true}); err != nil {
		t.Fatal(err)
	}
	// A read is not a write, so it leaves the log alone.
	if _, err := c.GetConfig(ctx, "acct", tunnel.ID); err != nil {
		t.Fatal(err)
	}
	s := f.Snapshot()
	var kinds []string
	for _, w := range s.Writes {
		kinds = append(kinds, w.Kind)
	}
	if strings.Join(kinds, ",") != "tunnel_create,config_put,record_create" {
		t.Fatalf("write log kinds = %v", kinds)
	}
	for i, w := range s.Writes {
		if w.Seq != i+1 || w.At == "" {
			t.Fatalf("write %d has seq %d and time %q", i, w.Seq, w.At)
		}
	}
	if s.Writes[0].Method != http.MethodPost || s.Writes[0].Path != "/accounts/acct/cfd_tunnel" || s.Writes[0].Target != "t1" {
		t.Fatalf("tunnel write = %+v", s.Writes[0])
	}
	if s.Writes[1].Path != "/accounts/acct/cfd_tunnel/"+tunnel.ID+"/configurations" || s.Writes[1].Target != tunnel.ID {
		t.Fatalf("config write = %+v", s.Writes[1])
	}
	if s.ConfigPuts != 1 || s.RecordWrites != 1 {
		t.Fatalf("counters = %d config puts, %d record writes", s.ConfigPuts, s.RecordWrites)
	}
	if got := s.Records["example.com"]; len(got) != 1 || got[0].Name != "a.example.com" {
		t.Fatalf("records by zone name = %+v", got)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), testToken) {
		t.Fatal("snapshot holds the API token")
	}
}

func TestInjectedFailureIsLoggedAndCleared(t *testing.T) {
	f := New(testToken, "acct")
	tunnelID := f.AddTunnel("t1", true)
	c := newClient(t, f)
	ctx := context.Background()
	cfg := cloudflare.TunnelConfig{Ingress: []cloudflare.Rule{{Service: "http_status:404"}}}

	f.FailRequests(http.MethodPut, "/accounts/", http.StatusInternalServerError)
	if err := c.PutConfig(ctx, "acct", tunnelID, cfg); err == nil {
		t.Fatal("PutConfig succeeded under an injected 500")
	}
	s := f.Snapshot()
	if s.ConfigPuts != 0 || len(s.Writes) != 1 {
		t.Fatalf("refused write changed the counters or log: %d puts, %d writes", s.ConfigPuts, len(s.Writes))
	}
	if w := s.Writes[0]; w.Kind != "injected_failure" || w.Target != "HTTP 500" || w.Method != http.MethodPut {
		t.Fatalf("refused write = %+v", w)
	}

	f.ClearFailures()
	if err := c.PutConfig(ctx, "acct", tunnelID, cfg); err != nil {
		t.Fatalf("PutConfig after ClearFailures: %v", err)
	}
	if s := f.Snapshot(); s.ConfigPuts != 1 {
		t.Fatalf("config puts after clearing = %d, want 1", s.ConfigPuts)
	}
}

func TestClearFailuresRemovesEveryFault(t *testing.T) {
	f := New(testToken, "acct")
	tunnelID := f.AddTunnel("t1", true)
	c := newClient(t, f)
	ctx := context.Background()
	f.FailRequests(http.MethodGet, "/accounts/", http.StatusInternalServerError)
	f.FailRequests(http.MethodPut, "/accounts/", http.StatusInternalServerError)
	f.ClearFailures()
	if _, err := c.GetConfig(ctx, "acct", tunnelID); err != nil {
		t.Fatalf("GET after ClearFailures: %v", err)
	}
}

func TestZoneIDAndHasTunnel(t *testing.T) {
	f := New(testToken, "acct")
	zone := f.AddZone("example.com")
	if got, ok := f.ZoneID("example.com"); !ok || got != zone {
		t.Fatalf("ZoneID(example.com) = %q, %v; want %q", got, ok, zone)
	}
	if _, ok := f.ZoneID("missing.test"); ok {
		t.Fatal("ZoneID found a zone the account does not hold")
	}
	tunnel := f.AddTunnel("t1", false)
	if !f.HasTunnel(tunnel) {
		t.Fatal("HasTunnel missed an existing tunnel")
	}
	if f.HasTunnel("00000000-0000-4000-8000-000000000000") {
		t.Fatal("HasTunnel found an unknown tunnel")
	}
}

func TestSnapshotCarriesSeededStateWithoutLoggingIt(t *testing.T) {
	f := New(testToken, "acct")
	zone := f.AddZone("example.com")
	tunnel := f.AddTunnel("legacy", true)
	f.SetConfig(tunnel, json.RawMessage(`{"ingress":[{"hostname":"legacy.example.net","service":"http://legacy:80"},{"service":"http_status:404"}]}`))
	f.AddRecord(zone, cloudflare.DNSRecord{Type: "A", Name: "www.example.com", Content: "192.0.2.1"})

	s := f.Snapshot()
	if len(s.Tunnels) != 1 || s.Tunnels[0].ID != tunnel || s.Tunnels[0].Name != "legacy" || !s.Tunnels[0].RemoteConfig {
		t.Fatalf("tunnels = %+v", s.Tunnels)
	}
	if string(s.Configs[tunnel]) != `{"ingress":[{"hostname":"legacy.example.net","service":"http://legacy:80"},{"service":"http_status:404"}]}` {
		t.Fatalf("config = %s", s.Configs[tunnel])
	}
	if got := s.Records["example.com"]; len(got) != 1 || got[0].Type != "A" {
		t.Fatalf("records = %+v", got)
	}
	if len(s.Writes) != 0 || s.ConfigPuts != 0 || s.RecordWrites != 0 {
		t.Fatalf("seeding was logged as API writes: %+v", s.Writes)
	}
}
