package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/metrics"
	"berthy/internal/scope"
	"berthy/internal/state"
	"berthy/internal/telemetry"
)

// fakeTelemetry is a telemetryStatus with fixed answers.
type fakeTelemetry struct {
	configured bool
	lastErr    string
	lastOK     time.Time
	target     string
}

func (f fakeTelemetry) Status() (bool, string, time.Time) { return f.configured, f.lastErr, f.lastOK }
func (f fakeTelemetry) Target() string                    { return f.target }

func TestTelemetryDoctorCheckStates(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	const target = "http://10.0.0.5:4318"
	cases := []struct {
		name   string
		status fakeTelemetry
		listed bool
		ok     bool
		detail string
	}{
		{name: "export off, nothing failed", status: fakeTelemetry{}, listed: false},
		{name: "exporting", status: fakeTelemetry{configured: true, lastOK: now.Add(-5 * time.Second), target: target}, listed: true, ok: true, detail: "exporting to " + target},
		{name: "recent success after a failure", status: fakeTelemetry{configured: true, lastOK: now.Add(-30 * time.Second), lastErr: "boom", target: target}, listed: true, ok: true, detail: "exporting to " + target},
		{name: "last success too old", status: fakeTelemetry{configured: true, lastOK: now.Add(-61 * time.Second), target: target}, listed: true, detail: "last export succeeded at 2026-11-16T09:58:59Z"},
		{name: "failing since the start", status: fakeTelemetry{configured: true, lastErr: "failed to send metrics", target: target}, listed: true, detail: "failed to send metrics"},
		{name: "no export yet", status: fakeTelemetry{configured: true, target: target}, listed: true, detail: "no export has succeeded yet"},
		{name: "secret missing", status: fakeTelemetry{lastErr: missingHeadersSecret}, listed: true, detail: missingHeadersSecret},
	}
	for _, tc := range cases {
		c, listed := telemetryDoctorCheck(tc.status, now)
		if listed != tc.listed {
			t.Fatalf("%s: listed %v, want %v", tc.name, listed, tc.listed)
		}
		if !listed {
			continue
		}
		if c.Name != "telemetry" || c.Critical || c.OK != tc.ok || c.Detail != tc.detail {
			t.Fatalf("%s: got %+v, want ok %v detail %q (non-critical telemetry)", tc.name, c, tc.ok, tc.detail)
		}
	}
}

// telemetryStore returns a store whose last applied manifest is manifestJSON.
func telemetryStore(t *testing.T, members string) *state.Store {
	t.Helper()
	s := openIngressStore(t)
	recordRelease(t, s, "telemetry", monitoredManifest(members))
	return s
}

func TestTelemetryConfigFollowsLastManifest(t *testing.T) {
	ctx := context.Background()
	const endpoint = `"telemetry":{"otlp_endpoint":"http://10.0.0.5:4318","headers_secret":"OTLP_HEADERS"}`

	if cfg, err := telemetryConfig(ctx, openIngressStore(t)); cfg != nil || err != nil {
		t.Fatalf("nothing deployed: config %+v, error %v", cfg, err)
	}
	if cfg, err := telemetryConfig(ctx, telemetryStore(t, "")); cfg != nil || err != nil {
		t.Fatalf("manifest without telemetry: config %+v, error %v", cfg, err)
	}
	noSecret := telemetryStore(t, `,"telemetry":{"otlp_endpoint":"http://10.0.0.5:4318"}`)
	if cfg, err := telemetryConfig(ctx, noSecret); err != nil || cfg == nil || cfg.Endpoint != "http://10.0.0.5:4318" || len(cfg.Headers) != 0 {
		t.Fatalf("endpoint without a headers secret: config %+v, error %v", cfg, err)
	}

	missing := telemetryStore(t, ","+endpoint)
	if _, err := telemetryConfig(ctx, missing); err == nil || err.Error() != missingHeadersSecret {
		t.Fatalf("secret not stored: error %v", err)
	}

	registry := telemetryStore(t, ","+endpoint)
	if err := registry.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: "OTLP_HEADERS", Registry: "ghcr.io"}, []byte("authorization=Bearer TEST-ONLY-R")); err != nil {
		t.Fatal(err)
	}
	if _, err := telemetryConfig(ctx, registry); err == nil || err.Error() != missingHeadersSecret {
		t.Fatalf("registry credential used as headers: error %v", err)
	}

	const malformed = "authorization Bearer SECRET-VALUE-9"
	bad := telemetryStore(t, ","+endpoint)
	if err := bad.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: "OTLP_HEADERS"}, []byte(malformed)); err != nil {
		t.Fatal(err)
	}
	if _, err := telemetryConfig(ctx, bad); err == nil || strings.Contains(err.Error(), "SECRET-VALUE-9") {
		t.Fatalf("malformed headers: error %v", err)
	}

	good := telemetryStore(t, ","+endpoint)
	if err := good.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: "OTLP_HEADERS"}, []byte("authorization=Bearer%20TEST-ONLY-5, x-team=ops")); err != nil {
		t.Fatal(err)
	}
	cfg, err := telemetryConfig(ctx, good)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "http://10.0.0.5:4318" || cfg.Headers["authorization"] != "Bearer TEST-ONLY-5" || cfg.Headers["x-team"] != "ops" || len(cfg.Headers) != 2 {
		t.Fatalf("config %+v", cfg)
	}
}

// waitForStatus polls the exporter until its status reports an error or configured state, and fails when it does not.
func waitForStatus(t *testing.T, exp *telemetry.Exporter, what string, done func(configured bool, lastErr string) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		configured, lastErr, _ := exp.Status()
		if done(configured, lastErr) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDoctorReportsTelemetryExport(t *testing.T) {
	s := telemetryStore(t, `,"telemetry":{"otlp_endpoint":"http://10.0.0.5:4318","headers_secret":"OTLP_HEADERS"}`)
	col := metrics.New(metrics.NewHostReader(), nil)
	exp := NewTelemetry(monitoringConfig(""), col, &deploy.Controller{Store: s, Environment: "lab"}, nil)
	if exp == nil {
		t.Fatal("no exporter for an agent with state")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		exp.Run(ctx)
	}()
	// Registered after the store's cleanup, so it runs first: the exporter stops before the store closes.
	t.Cleanup(func() {
		cancel()
		<-done
	})
	// The secret is not stored, so the export stays off and the doctor says why.
	waitForStatus(t, exp, "the missing secret to be recorded", func(configured bool, lastErr string) bool {
		return !configured && lastErr == missingHeadersSecret
	})
	h := NewDeploymentHandlerWithTelemetry(monitoringConfig(""), nil, col, &deploy.Controller{Store: s, Environment: "lab"}, exp)
	checks := doctorChecks(t, h)
	c, ok := findCheck(checks, "telemetry")
	if !ok || c.OK || c.Critical || c.Detail != missingHeadersSecret {
		t.Fatalf("telemetry check %+v (found %v)", c, ok)
	}

	// Without an exporter the doctor says nothing about telemetry.
	plain := NewDeploymentHandler(monitoringConfig(""), nil, col, &deploy.Controller{Store: s, Environment: "lab"})
	if _, ok := findCheck(doctorChecks(t, plain), "telemetry"); ok {
		t.Fatal("telemetry check listed without an exporter")
	}
}

// doctorChecks returns the checks of the doctor response.
func doctorChecks(t *testing.T, h http.Handler) []api.Check {
	t.Helper()
	w := serveRequest(t, h, http.MethodGet, "/v1/projects/demo/environments/lab/doctor", monitoringToken("a"))
	if w.Code != http.StatusOK {
		t.Fatalf("doctor answered %d", w.Code)
	}
	var body struct {
		Checks []api.Check `json:"checks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Checks
}

func findCheck(checks []api.Check, name string) (api.Check, bool) {
	for _, c := range checks {
		if c.Name == name {
			return c, true
		}
	}
	return api.Check{}, false
}

// otlpSink is an OTLP/HTTP metrics endpoint that signals each accepted export.
type otlpSink struct {
	srv *httptest.Server
	got chan *colmetricspb.ExportMetricsServiceRequest
}

func newOTLPSink(t *testing.T) *otlpSink {
	t.Helper()
	s := &otlpSink{got: make(chan *colmetricspb.ExportMetricsServiceRequest, 8)}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *otlpSink) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			http.Error(w, "bad gzip", http.StatusBadRequest)
			return
		}
		if body, err = io.ReadAll(zr); err != nil {
			http.Error(w, "bad gzip", http.StatusBadRequest)
			return
		}
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/metrics" {
		http.NotFound(w, r)
		return
	}
	var req colmetricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		http.Error(w, "not OTLP protobuf", http.StatusBadRequest)
		return
	}
	select {
	case s.got <- &req:
	default:
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

func (s *otlpSink) wait(t *testing.T, ctx context.Context) *colmetricspb.ExportMetricsServiceRequest {
	t.Helper()
	select {
	case req := <-s.got:
		return req
	case <-ctx.Done():
		t.Fatal("timed out waiting for an export")
	}
	return nil
}

func containerIDsIn(req *colmetricspb.ExportMetricsServiceRequest) map[string]bool {
	ids := map[string]bool{}
	if req == nil {
		return ids
	}
	for _, rm := range req.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				var dps []*metricspb.NumberDataPoint
				if g := m.GetGauge(); g != nil {
					dps = g.GetDataPoints()
				} else {
					dps = m.GetSum().GetDataPoints()
				}
				for _, dp := range dps {
					for _, kv := range dp.GetAttributes() {
						if kv.GetKey() == "container.id" {
							ids[kv.GetValue().GetStringValue()] = true
						}
					}
				}
			}
		}
	}
	return ids
}

func startExport(t *testing.T, exp *telemetry.Exporter) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		exp.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func testExporter(t *testing.T, col *metrics.Collector, keep func() func(map[string]string) bool) (*telemetry.Exporter, map[string]bool) {
	t.Helper()
	sink := newOTLPSink(t)
	exp := NewTelemetry(Config{Project: "demo", Environment: "lab"}, col, &deploy.Controller{Store: openIngressStore(t), Environment: "lab"}, nil)
	if exp == nil {
		t.Fatal("no exporter")
	}
	exp.Keep = keep
	exp.Config = func(context.Context) (*telemetry.Config, error) {
		return &telemetry.Config{Endpoint: sink.srv.URL}, nil
	}
	startExport(t, exp)
	if err := exp.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	return exp, containerIDsIn(sink.wait(t, t.Context()))
}

func TestTheExportLeavesOutContainersOfOtherEnvironments(t *testing.T) {
	lab, prod := scope.ID{Project: "demo", Environment: "lab"}, scope.ID{Project: "shop", Environment: "prod"}
	col := sampledCollector(t, metrics.Host{}, []metrics.ContainerRef{
		{ID: "lab-web", Labels: lab.Labels()},
		{ID: "prod-web", Labels: prod.Labels()},
	})
	_, ids := testExporter(t, col, func() func(map[string]string) bool { return lab.Owns })
	if !ids["lab-web"] {
		t.Fatalf("own container missing: %v", ids)
	}
	if ids["prod-web"] {
		t.Fatalf("other environment's container exported: %v", ids)
	}
}

func TestTheExportLeavesOutTheTasksOfAnAdoptedServiceOfAnotherEnvironment(t *testing.T) {
	lab := scope.ID{Project: "demo", Environment: "lab"}
	r := &Runtime{
		Config:     Config{Project: lab.Project, Environment: lab.Environment},
		peerClaims: func(context.Context) (map[state.Claim]bool, error) {
			return map[state.Claim]bool{{Kind: state.ClaimService, Name: "legacy-web"}: true}, nil
		},
	}
	col := sampledCollector(t, metrics.Host{}, []metrics.ContainerRef{
		{ID: "lab-own", Labels: lab.Labels()},
		{ID: "adopted-task", Labels: map[string]string{"com.docker.swarm.service.name": "legacy-web"}},
	})
	_, ids := testExporter(t, col, r.keep)
	if !ids["lab-own"] {
		t.Fatalf("own container missing: %v", ids)
	}
	if ids["adopted-task"] {
		t.Fatalf("adopted task of another environment exported: %v", ids)
	}
}
