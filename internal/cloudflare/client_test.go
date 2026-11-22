package cloudflare_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"berthy/internal/cloudflare"
	"berthy/internal/cloudflare/cftest"
	"berthy/internal/scope"
)

// token is a test value, not a credential.
const token = "cf-test-token-not-real"

func newFake(t *testing.T) (*cftest.Fake, *cloudflare.Client) {
	t.Helper()
	f := cftest.New(token, "acct")
	srv := httptest.NewServer(f.Handler())
	t.Cleanup(srv.Close)
	return f, &cloudflare.Client{BaseURL: srv.URL + "/client/v4", Token: token}
}

// requireAPIError checks the error's type, status and codes, and that its text holds neither secret nor body.
func requireAPIError(t *testing.T, err error, secret string, status int, codes ...int) {
	t.Helper()
	var api *cloudflare.APIError
	if !errors.As(err, &api) {
		t.Fatalf("want APIError, got %v", err)
	}
	if api.Status != status || !reflect.DeepEqual(api.Codes, codes) {
		t.Fatalf("APIError %+v, want status %d codes %v", api, status, codes)
	}
	if secret != "" && strings.Contains(err.Error(), secret) {
		t.Fatalf("error text contains a secret: %q", err.Error())
	}
}

func assertSameJSON(t *testing.T, want, got []byte) {
	t.Helper()
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w, g) {
		t.Fatalf("JSON differs:\nwant %s\n got %s", want, got)
	}
}

func TestClientMethodsAgainstFake(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	if err := c.VerifyToken(ctx, "acct"); err != nil {
		t.Fatal(err)
	}
	zone := f.AddZone("example.com")
	if z, err := c.FindZone(ctx, "acct", "api.example.com"); err != nil || z.ID != zone || z.Name != "example.com" {
		t.Fatalf("zone %+v %v", z, err)
	}
	if tun, err := c.FindTunnel(ctx, "acct", "demo-prod"); err != nil || tun != nil {
		t.Fatalf("tunnel before create: %+v %v", tun, err)
	}
	created, err := c.CreateTunnel(ctx, "acct", "demo-prod")
	if err != nil || created.ID == "" || created.Name != "demo-prod" || created.Status != "inactive" || !created.RemoteConfig {
		t.Fatalf("create tunnel %+v %v", created, err)
	}
	if found, err := c.FindTunnel(ctx, "acct", "demo-prod"); err != nil || found == nil || found.ID != created.ID {
		t.Fatalf("find tunnel %+v %v", found, err)
	}
	if got, err := c.GetTunnel(ctx, "acct", created.ID); err != nil || got.ID != created.ID {
		t.Fatalf("get tunnel %+v %v", got, err)
	}
	f.SetTunnelStatus(created.ID, "healthy", 2)
	if got, err := c.GetTunnel(ctx, "acct", created.ID); err != nil || got.Status != "healthy" || len(got.Connections) != 2 {
		t.Fatalf("tunnel status %+v %v", got, err)
	}
	if tok, err := c.TunnelToken(ctx, "acct", created.ID); err != nil || tok != f.TunnelToken(created.ID) {
		t.Fatalf("tunnel token: %v", err)
	}
	if cfg, err := c.GetConfig(ctx, "acct", created.ID); err != nil || len(cfg.Ingress) != 0 || cfg.Extra != nil {
		t.Fatalf("empty config %+v %v", cfg, err)
	}
	cfg := cloudflare.TunnelConfig{Ingress: []cloudflare.Rule{
		{Hostname: "api.example.com", Service: "http://demo-prod-api-web:8080"},
		{Service: "http_status:404"},
	}}
	if err = c.PutConfig(ctx, "acct", created.ID, cfg); err != nil || f.ConfigPuts != 1 {
		t.Fatalf("put config: %v puts=%d", err, f.ConfigPuts)
	}
	if back, err := c.GetConfig(ctx, "acct", created.ID); err != nil || !reflect.DeepEqual(back.Ingress, cfg.Ingress) {
		t.Fatalf("config round trip %+v %v", back, err)
	}

	target := cloudflare.TunnelTarget(created.ID)
	if records, err := c.DNSRecords(ctx, zone, "api.example.com"); err != nil || len(records) != 0 {
		t.Fatalf("records before create: %+v %v", records, err)
	}
	prod := scope.ID{Project: "shop", Environment: "prod"}
	made, err := c.CreateDNSRecord(ctx, zone, cloudflare.DNSRecord{Type: "CNAME", Name: "api.example.com", Content: target, Proxied: true, TTL: 1, Comment: cloudflare.RecordComment(prod)})
	if err != nil || made.ID == "" || made.Content != target || !cloudflare.IsManaged(made, prod) {
		t.Fatalf("create record %+v %v", made, err)
	}
	if _, err = c.CreateDNSRecord(ctx, zone, cloudflare.DNSRecord{Type: "A", Name: "api.example.com", Content: "203.0.113.5"}); err == nil {
		t.Fatal("record beside a CNAME accepted")
	}
	requireAPIError(t, err, token, 400, 81053)
	made.Proxied = false
	updated, err := c.UpdateDNSRecord(ctx, zone, made.ID, made)
	if err != nil || updated.Proxied || f.RecordWrites != 2 {
		t.Fatalf("update record %+v %v writes=%d", updated, err, f.RecordWrites)
	}
	if records, err := c.DNSRecords(ctx, zone, "api.example.com"); err != nil || len(records) != 1 || records[0].ID != made.ID {
		t.Fatalf("records after update %+v %v", records, err)
	}
	if err = c.DeleteDNSRecord(ctx, zone, made.ID); err != nil || len(f.Records(zone)) != 0 {
		t.Fatalf("delete record: %v", err)
	}
}

func TestFindTunnelIgnoresDeletedAndRejectsDuplicates(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	old := f.AddTunnel("demo-prod", true)
	f.DeleteTunnel(old)
	if tun, err := c.FindTunnel(ctx, "acct", "demo-prod"); err != nil || tun != nil {
		t.Fatalf("deleted tunnel found: %+v %v", tun, err)
	}
	if _, err := c.CreateTunnel(ctx, "acct", "demo-prod"); err != nil {
		t.Fatalf("name free after delete: %v", err)
	}
	if _, err := c.CreateTunnel(ctx, "acct", "demo-prod"); err == nil {
		t.Fatal("duplicate tunnel name created")
	} else {
		requireAPIError(t, err, token, 409, 1013)
	}
	if tun, err := c.FindTunnel(ctx, "acct", "demo"); err != nil || tun != nil {
		t.Fatalf("partial name matched: %+v %v", tun, err)
	}
	f.AddTunnel("demo-prod", true)
	if _, err := c.FindTunnel(ctx, "acct", "demo-prod"); err == nil || !strings.Contains(err.Error(), "2 tunnels") {
		t.Fatalf("duplicates accepted: %v", err)
	}
}

func TestFindZoneWalksSuffixesMostSpecificFirst(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	parent := f.AddZone("example.com")
	if z, err := c.FindZone(ctx, "acct", "a.b.example.com"); err != nil || z.ID != parent {
		t.Fatalf("parent zone %+v %v", z, err)
	}
	child := f.AddZone("b.example.com")
	if z, err := c.FindZone(ctx, "acct", "a.b.example.com"); err != nil || z.ID != child {
		t.Fatalf("most specific zone %+v %v", z, err)
	}
	if _, err := c.FindZone(ctx, "acct", "a.other.test"); !errors.Is(err, cloudflare.ErrZoneNotFound) {
		t.Fatalf("missing zone: %v", err)
	}
	if _, err := c.FindZone(ctx, "acct", "localhost"); !errors.Is(err, cloudflare.ErrZoneNotFound) {
		t.Fatalf("single label: %v", err)
	}
}

func TestAnotherAccountIsNotFound(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	id := f.AddTunnel("demo-prod", true)
	f.AddZone("example.com")
	if _, err := c.GetTunnel(ctx, "other", id); err == nil {
		t.Fatal("tunnel read through another account")
	} else {
		requireAPIError(t, err, token, 404, 7003)
	}
	if _, err := c.FindZone(ctx, "other", "api.example.com"); err == nil {
		t.Fatal("zone read through another account")
	} else {
		requireAPIError(t, err, token, 404, 7003)
	}
}

func TestWrongTokenIsAPIErrorWithoutToken(t *testing.T) {
	f := cftest.New(token, "acct")
	srv := httptest.NewServer(f.Handler())
	t.Cleanup(srv.Close)
	const wrong = "cf-wrong-token-value"
	c := &cloudflare.Client{BaseURL: srv.URL, Token: wrong}
	err := c.VerifyToken(context.Background(), "acct")
	requireAPIError(t, err, wrong, 403, 10000)
	if strings.Contains(err.Error(), token) {
		t.Fatal("error text contains the valid token")
	}
}

func TestVerifyTokenFallsBackToAccountEndpoint(t *testing.T) {
	f := cftest.New(token, "acct")
	h := f.Handler()
	var userCalls, accountCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/client/v4/user/tokens/verify":
			userCalls.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":1000,"message":"Invalid API Token"}]}`))
		case "/client/v4/accounts/acct/tokens/verify":
			accountCalls.Add(1)
			h.ServeHTTP(w, r)
		default:
			h.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := &cloudflare.Client{BaseURL: srv.URL + "/client/v4", Token: token}
	if err := c.VerifyToken(context.Background(), "acct"); err != nil {
		t.Fatal(err)
	}
	if userCalls.Load() != 1 || accountCalls.Load() != 1 {
		t.Fatalf("calls user=%d account=%d", userCalls.Load(), accountCalls.Load())
	}
}

func TestVerifyTokenDoesNotFallBackWhenInactive(t *testing.T) {
	var accountCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tokens/verify") && !strings.HasPrefix(r.URL.Path, "/client/v4/user") {
			accountCalls.Add(1)
		}
		_, _ = w.Write([]byte(`{"success":true,"errors":[],"messages":[],"result":{"status":"disabled"}}`))
	}))
	t.Cleanup(srv.Close)
	c := &cloudflare.Client{BaseURL: srv.URL + "/client/v4", Token: token}
	if err := c.VerifyToken(context.Background(), "acct"); err == nil {
		t.Fatal("disabled token verified")
	}
	if accountCalls.Load() != 0 {
		t.Fatal("fell back after a readable inactive status")
	}
}

func TestAPIErrorHidesResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`<html>upstream body secret-body-text</html>`))
	}))
	t.Cleanup(srv.Close)
	c := &cloudflare.Client{BaseURL: srv.URL, Token: token}
	_, err := c.GetTunnel(context.Background(), "acct", "tid")
	requireAPIError(t, err, token, 502)
	if strings.Contains(err.Error(), "secret-body-text") {
		t.Fatalf("error shows body: %q", err.Error())
	}
}

func TestResponseBodyIsLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"errors":[],"messages":[],"result":"`))
		_, _ = w.Write(bytes.Repeat([]byte("a"), 5<<20))
		_, _ = w.Write([]byte(`"}`))
	}))
	t.Cleanup(srv.Close)
	c := &cloudflare.Client{BaseURL: srv.URL, Token: token}
	if _, err := c.TunnelToken(context.Background(), "acct", "tid"); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized response accepted: %v", err)
	}
}

func TestRuleAndConfigPreserveUnknownFields(t *testing.T) {
	const original = `{"ingress":[{"hostname":"api.example.com","service":"http://demo-prod-api-web:8080","path":"/v1/*","originRequest":{"connectTimeout":"30s","noTLSVerify":false}},{"service":"http_status:404"}],"warp-routing":{"enabled":false},"originRequest":{"keepAliveTimeout":"90s"}}`
	var cfg cloudflare.TunnelConfig
	if err := json.Unmarshal([]byte(original), &cfg); err != nil {
		t.Fatal(err)
	}
	first := cfg.Ingress[0]
	if first.Hostname != "api.example.com" || first.Service != "http://demo-prod-api-web:8080" || first.Path != "/v1/*" {
		t.Fatalf("typed fields %+v", first)
	}
	if got := string(first.Extra["originRequest"]); got != `{"connectTimeout":"30s","noTLSVerify":false}` {
		t.Fatalf("rule extra changed: %s", got)
	}
	if got := string(cfg.Extra["warp-routing"]); got != `{"enabled":false}` {
		t.Fatalf("config extra changed: %s", got)
	}
	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertSameJSON(t, []byte(original), out)

	f, c := newFake(t)
	ctx := context.Background()
	id := f.AddTunnel("demo-prod", true)
	f.SetConfig(id, json.RawMessage(original))
	got, err := c.GetConfig(ctx, "acct", id)
	if err != nil {
		t.Fatal(err)
	}
	got.Ingress[0].Service = "http://demo-prod-api-web:8081"
	if err = c.PutConfig(ctx, "acct", id, got); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(original, "8080", "8081", 1)
	assertSameJSON(t, []byte(want), f.Config(id))
}

func TestMarshalOmitsEmptyHostnameAndPath(t *testing.T) {
	rule, err := json.Marshal(cloudflare.Rule{Service: "http_status:404"})
	if err != nil || string(rule) != `{"service":"http_status:404"}` {
		t.Fatalf("catch-all rule %s %v", rule, err)
	}
	empty, err := json.Marshal(cloudflare.TunnelConfig{})
	if err != nil || string(empty) != `{"ingress":[]}` {
		t.Fatalf("empty config %s %v", empty, err)
	}
}

func TestGetConfigTreatsNullAsEmpty(t *testing.T) {
	f, c := newFake(t)
	id := f.AddTunnel("demo-prod", true)
	f.SetConfig(id, json.RawMessage(`null`))
	cfg, err := c.GetConfig(context.Background(), "acct", id)
	if err != nil || len(cfg.Ingress) != 0 || cfg.Extra != nil {
		t.Fatalf("null config %+v %v", cfg, err)
	}
}

func TestPutConfigNeedsCatchAllLast(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	id := f.AddTunnel("demo-prod", true)
	err := c.PutConfig(ctx, "acct", id, cloudflare.TunnelConfig{Ingress: []cloudflare.Rule{{Hostname: "api.example.com", Service: "http://x:80"}}})
	requireAPIError(t, err, token, 400, 1000)
	if f.ConfigPuts != 0 {
		t.Fatalf("rejected config counted as written: %d", f.ConfigPuts)
	}
}

func TestTunnelTokenHelpers(t *testing.T) {
	if got := cloudflare.TunnelTarget("abc"); got != "abc.cfargotunnel.com" {
		t.Fatalf("target %q", got)
	}
}

func TestTheRecordCommentNamesProjectAndEnvironment(t *testing.T) {
	id := scope.ID{Project: "shop", Environment: "prod"}
	if got := cloudflare.RecordComment(id); got != "managed by berthy (shop/prod)" {
		t.Fatalf("comment %q", got)
	}
	managed := cloudflare.DNSRecord{Comment: cloudflare.RecordComment(id)}
	if !cloudflare.IsManaged(managed, id) || cloudflare.IsManaged(managed, scope.ID{Project: "shop", Environment: "stage"}) || cloudflare.IsManaged(cloudflare.DNSRecord{}, id) {
		t.Fatal("ownership marks do not match the hosted environment")
	}
}

func TestARecordOfAnotherProjectIsNotManaged(t *testing.T) {
	ours := scope.ID{Project: "shop", Environment: "prod"}
	theirs := scope.ID{Project: "other", Environment: "prod"}
	managed := cloudflare.DNSRecord{Comment: cloudflare.RecordComment(theirs)}
	if cloudflare.IsManaged(managed, ours) {
		t.Fatal("a record of another project was treated as managed")
	}
}

func TestZonesListsEveryZoneOfTheAccountAcrossPages(t *testing.T) {
	f, c := newFake(t)
	var want []string
	for i := 0; i < 120; i++ {
		name := fmt.Sprintf("zone%03d.example", i)
		f.AddZone(name)
		want = append(want, name)
	}
	zones, err := c.Zones(context.Background(), "acct")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, z := range zones {
		got = append(got, z.Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("zones: got %d, want %d", len(got), len(want))
	}
	if _, err = c.Zones(context.Background(), "other"); err == nil {
		t.Fatal("another account's zones were listed")
	}
}

func TestDNSRecordsByContentFindsRecordsOfAnyNameAndType(t *testing.T) {
	f, c := newFake(t)
	zone := f.AddZone("example.com")
	target := cloudflare.TunnelTarget("tun-1")
	f.AddRecord(zone, cloudflare.DNSRecord{Type: "CNAME", Name: "a.example.com", Content: target})
	f.AddRecord(zone, cloudflare.DNSRecord{Type: "CNAME", Name: "b.example.com", Content: strings.ToUpper(target)})
	f.AddRecord(zone, cloudflare.DNSRecord{Type: "CNAME", Name: "c.example.com", Content: "other.example.net"})
	f.AddRecord(zone, cloudflare.DNSRecord{Type: "TXT", Name: "d.example.com", Content: target + ".extra"})
	got, err := c.DNSRecordsByContent(context.Background(), zone, target)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range got {
		names = append(names, r.Name)
	}
	if !reflect.DeepEqual(names, []string{"a.example.com", "b.example.com"}) {
		t.Fatalf("records %v", names)
	}
}

func TestDeleteTunnelRefusesAnActiveOne(t *testing.T) {
	f, c := newFake(t)
	id := f.AddTunnel("demo", true)
	f.SetTunnelStatus(id, "healthy", 2)
	if err := c.DeleteTunnel(context.Background(), "acct", id); err == nil {
		t.Fatal("a tunnel with connections was deleted")
	}
	if tunnel, _ := c.FindTunnel(context.Background(), "acct", "demo"); tunnel == nil {
		t.Fatal("the tunnel is gone")
	}
	f.SetTunnelStatus(id, "inactive", 0)
	if err := c.DeleteTunnel(context.Background(), "acct", id); err != nil {
		t.Fatal(err)
	}
	if tunnel, err := c.FindTunnel(context.Background(), "acct", "demo"); err != nil || tunnel != nil {
		t.Fatalf("tunnel after delete: %v %v", tunnel, err)
	}
	err := c.DeleteTunnel(context.Background(), "acct", id)
	var api *cloudflare.APIError
	if !errors.As(err, &api) || api.Status != http.StatusNotFound {
		t.Fatalf("a second delete: %v", err)
	}
	var kinds []string
	for _, w := range f.Snapshot().Writes {
		kinds = append(kinds, w.Kind)
	}
	if !reflect.DeepEqual(kinds, []string{"tunnel_delete"}) {
		t.Fatalf("writes %v", kinds)
	}
}

func TestTokenStatusReportsTheExpiryCloudflareNames(t *testing.T) {
	f, c := newFake(t)
	expiry := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	f.SetTokenExpiry(expiry)
	st, err := c.TokenStatus(context.Background(), "acct")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "active" || !st.Expires.Equal(expiry) {
		t.Fatalf("token status %+v, want active until %s", st, expiry)
	}
}

func TestTokenStatusWithoutAnExpiryIsZero(t *testing.T) {
	_, c := newFake(t)
	st, err := c.TokenStatus(context.Background(), "acct")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "active" || !st.Expires.IsZero() {
		t.Fatalf("token status %+v, want active with no expiry", st)
	}
}

func TestTokenStatusIgnoresAnExpiryItCannotRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"errors":[],"messages":[],"result":{"status":"active","expires_on":"soon"}}`))
	}))
	t.Cleanup(srv.Close)
	c := &cloudflare.Client{BaseURL: srv.URL + "/client/v4", Token: token}
	st, err := c.TokenStatus(context.Background(), "acct")
	if err != nil {
		t.Fatalf("an unreadable expiry failed the check: %v", err)
	}
	if st.Status != "active" || !st.Expires.IsZero() {
		t.Fatalf("token status %+v, want active with no expiry", st)
	}
}

func TestTokenStatusFallsBackToTheAccountEndpointWithItsExpiry(t *testing.T) {
	f := cftest.New(token, "acct")
	expiry := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	f.SetTokenExpiry(expiry)
	h := f.Handler()
	var userCalls, accountCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/client/v4/user/tokens/verify":
			userCalls.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":1000,"message":"Invalid API Token"}]}`))
		case "/client/v4/accounts/acct/tokens/verify":
			accountCalls.Add(1)
			h.ServeHTTP(w, r)
		default:
			h.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := &cloudflare.Client{BaseURL: srv.URL + "/client/v4", Token: token}
	st, err := c.TokenStatus(context.Background(), "acct")
	if err != nil {
		t.Fatal(err)
	}
	if userCalls.Load() != 1 || accountCalls.Load() != 1 {
		t.Fatalf("calls user=%d account=%d", userCalls.Load(), accountCalls.Load())
	}
	if st.Status != "active" || !st.Expires.Equal(expiry) {
		t.Fatalf("token status %+v, want the account endpoint's expiry", st)
	}
}

func TestRefusedCountsOnlyAnAnswer(t *testing.T) {
	wrapped := fmt.Errorf("x: %w", &cloudflare.APIError{Status: http.StatusUnauthorized})
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"inactive token", cloudflare.ErrTokenInactive, true},
		{"zone not found", cloudflare.ErrZoneNotFound, true},
		{"unauthorized", &cloudflare.APIError{Status: 401}, true},
		{"forbidden", &cloudflare.APIError{Status: 403}, true},
		{"rate limited", &cloudflare.APIError{Status: 429}, false},
		{"server error", &cloudflare.APIError{Status: 500}, false},
		{"unavailable", &cloudflare.APIError{Status: 503}, false},
		{"connection failure", errors.New("dial failed"), false},
		{"timeout", context.DeadlineExceeded, false},
		{"wrapped refusal", wrapped, true},
	}
	for _, tc := range cases {
		if got := cloudflare.Refused(tc.err); got != tc.want {
			t.Errorf("Refused(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
