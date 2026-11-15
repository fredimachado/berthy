package telemetry

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"

	"berthy/internal/metrics"
	"berthy/internal/state"
)

const containerID = "0123456789abcdef0123"

// fakeHost and fakeDocker feed the collector fixed samples, so a test knows exactly what the exporter must send.
type fakeHost struct{ host metrics.Host }

func (f fakeHost) Read(context.Context) metrics.Host { return f.host }

type fakeDocker struct{}

func (fakeDocker) Containers(context.Context) ([]string, error) { return []string{containerID}, nil }
func (fakeDocker) Stats(context.Context, string) (metrics.RawContainer, error) {
	return metrics.RawContainer{MemoryUsage: 300, MemoryLimit: 1000, NetworkRXBytes: 10, NetworkTXBytes: 20, BlockReadBytes: 30, BlockWriteBytes: 40}, nil
}

// testHost is a host sample with round numbers, so every expected value is easy to read.
func testHost() metrics.Host {
	cpu := 25.0
	total, available, swapTotal, swapFree := uint64(1000), uint64(400), uint64(500), uint64(500)
	read, written, rx, tx := uint64(4096), uint64(2048), uint64(100), uint64(200)
	return metrics.Host{
		CPUPercent:      &cpu,
		MemoryTotal:     &total,
		MemoryAvailable: &available,
		SwapTotal:       &swapTotal,
		SwapFree:        &swapFree,
		Load:            []float64{0.5, 0.6, 0.7},
		DiskReadBytes:   &read,
		DiskWriteBytes:  &written,
		NetworkRXBytes:  &rx,
		NetworkTXBytes:  &tx,
		Mounts:          []metrics.Mount{{Path: "/", Total: 1000, Available: 250}},
	}
}

func testChecks(context.Context) ([]state.CheckState, error) {
	return []state.CheckState{
		{Name: "disk:/", State: "ok"},
		{Name: "peer:prod", State: "warning"},
		{Name: "probe:web", State: "critical"},
		{Name: "restarts:api-web", State: "unknown"},
	}, nil
}

// testCollector returns a collector fed by testHost and one container, once its first sample exists.
func testCollector(t *testing.T) *metrics.Collector {
	t.Helper()
	col := metrics.New(fakeHost{host: testHost()}, fakeDocker{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go col.Run(ctx)
	waitFor(t, "the first sample", func() bool { _, ok := col.Latest(); return ok })
	return col
}

// point is one data point of an exported metric.
type point struct {
	attrs map[string]string
	value float64
}

// fromSDK flattens SDK metric data into metric name -> data points.
func fromSDK(rm metricdata.ResourceMetrics) map[string][]point {
	out := map[string][]point{}
	add := func(name string, set attribute.Set, v float64) {
		attrs := map[string]string{}
		for it := set.Iter(); it.Next(); {
			kv := it.Attribute()
			attrs[string(kv.Key)] = kv.Value.Emit()
		}
		out[name] = append(out[name], point{attrs, v})
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Gauge[int64]:
				for _, dp := range d.DataPoints {
					add(m.Name, dp.Attributes, float64(dp.Value))
				}
			case metricdata.Gauge[float64]:
				for _, dp := range d.DataPoints {
					add(m.Name, dp.Attributes, dp.Value)
				}
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					add(m.Name, dp.Attributes, float64(dp.Value))
				}
			case metricdata.Sum[float64]:
				for _, dp := range d.DataPoints {
					add(m.Name, dp.Attributes, dp.Value)
				}
			}
		}
	}
	return out
}

// checkPoint fails the test unless the metric has a data point with exactly these attributes and this value.
func checkPoint(t *testing.T, points map[string][]point, name string, attrs map[string]string, want float64) {
	t.Helper()
	if attrs == nil {
		attrs = map[string]string{}
	}
	for _, p := range points[name] {
		if maps.Equal(p.attrs, attrs) {
			if p.value != want {
				t.Errorf("%s %v = %v, want %v", name, attrs, p.value, want)
			}
			return
		}
	}
	t.Errorf("%s %v: no such data point (have %v)", name, attrs, points[name])
}

func TestObservationsCoverHostContainersAndChecks(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	meter := mp.Meter("test")
	in, observables, err := newInstruments(meter)
	if err != nil {
		t.Fatal(err)
	}
	cpu := 150.0
	containers := []metrics.Container{
		{ID: containerID, CPUPercent: &cpu, MemoryUsage: 300, MemoryLimit: 1000, NetworkRXBytes: 10, NetworkTXBytes: 20, BlockReadBytes: 30, BlockWriteBytes: 40},
		{ID: "fedcba987654ffff", Error: "container is gone"},
	}
	host := testHost()
	e := &Exporter{Checks: testChecks}
	if _, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		observeHost(o, in, host)
		observeContainers(o, in, containers)
		e.observe(ctx, o, in)
		return nil
	}, observables...); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	points := fromSDK(rm)

	checkPoint(t, points, "system.cpu.utilization", nil, 0.25)
	checkPoint(t, points, "system.memory.usage", map[string]string{"system.memory.state": "used"}, 600)
	checkPoint(t, points, "system.memory.usage", map[string]string{"system.memory.state": "free"}, 400)
	checkPoint(t, points, "system.memory.limit", nil, 1000)
	checkPoint(t, points, "system.paging.usage", map[string]string{"system.paging.state": "used"}, 0)
	checkPoint(t, points, "system.paging.usage", map[string]string{"system.paging.state": "free"}, 500)
	checkPoint(t, points, "system.cpu.load_average.1m", nil, 0.5)
	checkPoint(t, points, "system.cpu.load_average.5m", nil, 0.6)
	checkPoint(t, points, "system.cpu.load_average.15m", nil, 0.7)
	checkPoint(t, points, "system.filesystem.usage", map[string]string{"system.filesystem.mountpoint": "/", "system.filesystem.state": "used"}, 750)
	checkPoint(t, points, "system.filesystem.usage", map[string]string{"system.filesystem.mountpoint": "/", "system.filesystem.state": "free"}, 250)
	checkPoint(t, points, "system.disk.io", map[string]string{"disk.io.direction": "read"}, 4096)
	checkPoint(t, points, "system.disk.io", map[string]string{"disk.io.direction": "write"}, 2048)
	checkPoint(t, points, "system.network.io", map[string]string{"network.io.direction": "receive"}, 100)
	checkPoint(t, points, "system.network.io", map[string]string{"network.io.direction": "transmit"}, 200)
	checkPoint(t, points, "container.cpu.utilization", map[string]string{"container.id": "0123456789ab"}, 1.5)
	checkPoint(t, points, "container.memory.usage", map[string]string{"container.id": "0123456789ab"}, 300)
	checkPoint(t, points, "container.memory.limit", map[string]string{"container.id": "0123456789ab"}, 1000)
	checkPoint(t, points, "container.network.io", map[string]string{"container.id": "0123456789ab", "network.io.direction": "transmit"}, 20)
	checkPoint(t, points, "container.disk.io", map[string]string{"container.id": "0123456789ab", "disk.io.direction": "write"}, 40)
	checkPoint(t, points, "berthy.check.state", map[string]string{"berthy.check": "disk:/"}, 0)
	checkPoint(t, points, "berthy.check.state", map[string]string{"berthy.check": "peer:prod"}, 1)
	checkPoint(t, points, "berthy.check.state", map[string]string{"berthy.check": "probe:web"}, 2)
	checkPoint(t, points, "berthy.check.state", map[string]string{"berthy.check": "restarts:api-web"}, -1)

	for name, pts := range points {
		for _, p := range pts {
			if p.attrs["container.id"] == "fedcba987654" {
				t.Errorf("%s reported for a container whose sample failed", name)
			}
		}
	}
	counters := map[string]bool{"system.disk.io": true, "system.network.io": true, "container.network.io": true, "container.disk.io": true}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if _, ok := m.Data.(metricdata.Sum[int64]); ok != counters[m.Name] {
				t.Errorf("%s: counter %v, want %v", m.Name, ok, counters[m.Name])
			}
		}
	}
}

// receiver is an OTLP/HTTP metrics endpoint. It decodes each accepted request and keeps the headers it arrived with.
type receiver struct {
	srv *httptest.Server

	mu       sync.Mutex
	status   int    // when not zero, every request is answered with this status and is not recorded
	reply    string // the body that goes with status
	requests []received
}

type received struct {
	header  http.Header
	request *colmetricspb.ExportMetricsServiceRequest
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	rc := &receiver{}
	rc.srv = httptest.NewServer(http.HandlerFunc(rc.serve))
	t.Cleanup(rc.srv.Close)
	return rc
}

func (rc *receiver) serve(w http.ResponseWriter, r *http.Request) {
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
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if r.Method != http.MethodPost || r.URL.Path != "/v1/metrics" {
		http.NotFound(w, r)
		return
	}
	if rc.status != 0 {
		w.WriteHeader(rc.status)
		_, _ = io.WriteString(w, rc.reply)
		return
	}
	var req colmetricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		http.Error(w, "not OTLP protobuf", http.StatusBadRequest)
		return
	}
	rc.requests = append(rc.requests, received{header: r.Header.Clone(), request: &req})
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

func (rc *receiver) setStatus(status int, reply string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.status, rc.reply = status, reply
}

func (rc *receiver) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.requests)
}

func (rc *receiver) snapshot() []received {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return slices.Clone(rc.requests)
}

// decodeRequests flattens the metrics of the requests into metric name -> data points. It also returns the resource
// attributes of the first request.
func decodeRequests(reqs []received) (map[string][]point, map[string]string) {
	out := map[string][]point{}
	resource := map[string]string{}
	for i, rq := range reqs {
		for _, rm := range rq.request.GetResourceMetrics() {
			if i == 0 {
				for _, kv := range rm.GetResource().GetAttributes() {
					resource[kv.GetKey()] = kv.GetValue().GetStringValue()
				}
			}
			for _, sm := range rm.GetScopeMetrics() {
				for _, m := range sm.GetMetrics() {
					for _, dp := range numberPoints(m) {
						attrs := map[string]string{}
						for _, kv := range dp.GetAttributes() {
							attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
						}
						v := dp.GetAsDouble()
						if _, isInt := dp.Value.(*metricspb.NumberDataPoint_AsInt); isInt {
							v = float64(dp.GetAsInt())
						}
						out[m.GetName()] = append(out[m.GetName()], point{attrs, v})
					}
				}
			}
		}
	}
	return out, resource
}

// numberPoints returns the data points of a gauge or a sum.
func numberPoints(m *metricspb.Metric) []*metricspb.NumberDataPoint {
	if g := m.GetGauge(); g != nil {
		return g.GetDataPoints()
	}
	return m.GetSum().GetDataPoints()
}

// waitFor polls cond until it holds, and fails the test when the deadline passes first.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startExporter runs e in the background. It returns the function that cancels Run, and a channel that closes once Run
// has returned.
func startExporter(t *testing.T, e *Exporter) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		e.Run(ctx)
	}()
	t.Cleanup(func() {
		stop()
		<-finished
	})
	return stop, finished
}

// logSink keeps the operational messages an exporter logs.
type logSink struct {
	mu   sync.Mutex
	msgs []string
}

func (l *logSink) add(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, msg)
}

func (l *logSink) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.msgs)
}

func fixedConfig(cfg *Config) func(context.Context) (*Config, error) {
	return func(context.Context) (*Config, error) { return cfg, nil }
}

func TestExportDeliversMetricsWithHeaders(t *testing.T) {
	rc := newReceiver(t)
	cfg := &Config{Endpoint: rc.srv.URL, Headers: map[string]string{"authorization": "Bearer TEST-ONLY-1"}}
	e := &Exporter{Env: "lab", Collector: testCollector(t), Checks: testChecks, Config: fixedConfig(cfg), exportEvery: 20 * time.Millisecond, configEvery: time.Hour}
	startExporter(t, e)
	waitFor(t, "an export", func() bool { return rc.count() > 0 })
	reqs := rc.snapshot()

	h := reqs[0].header
	if got := h.Get("Authorization"); got != "Bearer TEST-ONLY-1" {
		t.Fatal("the authorization header from the secret did not arrive")
	}
	if got := h.Get("Content-Type"); got != "application/x-protobuf" {
		t.Fatalf("content type %q", got)
	}
	points, resource := decodeRequests(reqs)
	for k, want := range map[string]string{"service.name": "berthy-agent", "deployment.environment.name": "lab"} {
		if resource[k] != want {
			t.Errorf("resource %s = %q, want %q", k, resource[k], want)
		}
	}
	if resource["host.name"] == "" {
		t.Error("resource has no host.name")
	}
	checkPoint(t, points, "system.cpu.utilization", nil, 0.25)
	checkPoint(t, points, "system.memory.usage", map[string]string{"system.memory.state": "used"}, 600)
	checkPoint(t, points, "system.filesystem.usage", map[string]string{"system.filesystem.mountpoint": "/", "system.filesystem.state": "free"}, 250)
	checkPoint(t, points, "system.network.io", map[string]string{"network.io.direction": "transmit"}, 200)
	checkPoint(t, points, "container.memory.usage", map[string]string{"container.id": "0123456789ab"}, 300)
	checkPoint(t, points, "berthy.check.state", map[string]string{"berthy.check": "probe:web"}, 2)
	checkPoint(t, points, "berthy.check.state", map[string]string{"berthy.check": "restarts:api-web"}, -1)
}

func TestReconfigurationSwitchesEndpointAndStopsWhenOff(t *testing.T) {
	first, second := newReceiver(t), newReceiver(t)
	var mu sync.Mutex
	current := &Config{Endpoint: first.srv.URL, Headers: map[string]string{"x-team": "one"}}
	setConfig := func(c *Config) {
		mu.Lock()
		defer mu.Unlock()
		current = c
	}
	e := &Exporter{Env: "lab", Collector: testCollector(t), Checks: testChecks, exportEvery: 20 * time.Millisecond, configEvery: 20 * time.Millisecond}
	e.Config = func(context.Context) (*Config, error) {
		mu.Lock()
		defer mu.Unlock()
		return current, nil
	}
	startExporter(t, e)
	waitFor(t, "an export to the first endpoint", func() bool { return first.count() > 0 })

	setConfig(&Config{Endpoint: second.srv.URL, Headers: map[string]string{"x-team": "two"}})
	waitFor(t, "an export to the second endpoint", func() bool { return second.count() > 0 })
	if got := second.snapshot()[0].header.Get("X-Team"); got != "two" {
		t.Fatalf("second endpoint got the header %q, want the new one", got)
	}
	// The old provider is shut down before the new one starts, so the first endpoint gets nothing more.
	before := first.count()
	time.Sleep(150 * time.Millisecond)
	if first.count() != before {
		t.Fatal("the old endpoint still receives exports after the switch")
	}

	setConfig(nil)
	waitFor(t, "export to stop", func() bool { configured, _, _ := e.Status(); return !configured })
	stopped := second.count()
	time.Sleep(150 * time.Millisecond)
	if second.count() != stopped {
		t.Fatal("exports continued after the configuration was turned off")
	}
	if configured, lastErr, _ := e.Status(); configured || lastErr != "" || e.Target() != "" {
		t.Fatalf("off state: configured %v, lastErr %q, target %q", configured, lastErr, e.Target())
	}
}

func TestFailureIsReportedWithoutHeaderValues(t *testing.T) {
	const header = "Bearer TEST-ONLY-SECRET-42"
	rc := newReceiver(t)
	rc.setStatus(http.StatusInternalServerError, "rejected "+header)
	logs := &logSink{}
	cfg := &Config{Endpoint: rc.srv.URL, Headers: map[string]string{"authorization": header}}
	e := &Exporter{Env: "lab", Collector: testCollector(t), Config: fixedConfig(cfg), exportEvery: 20 * time.Millisecond, configEvery: time.Hour, Log: logs.add}
	startExporter(t, e)
	waitFor(t, "a recorded failure", func() bool { _, lastErr, _ := e.Status(); return lastErr != "" })

	configured, lastErr, lastOK := e.Status()
	if !configured || !lastOK.IsZero() {
		t.Fatalf("configured %v, lastOK %v after a failed export", configured, lastOK)
	}
	if !strings.Contains(lastErr, "500") {
		t.Fatalf("failure does not name the status: %q", lastErr)
	}
	if strings.Contains(lastErr, "/v1/metrics") {
		t.Fatalf("failure names the metrics path: %q", lastErr)
	}
	for _, text := range append(logs.lines(), lastErr) {
		if strings.Contains(text, "TEST-ONLY-SECRET-42") {
			t.Fatalf("a header value reached an error or a log line: %q", text)
		}
	}
	if len(logs.lines()) == 0 {
		t.Fatal("the failure was not logged")
	}
	if e.Target() != rc.srv.URL {
		t.Fatalf("target %q, want %q", e.Target(), rc.srv.URL)
	}

	rc.setStatus(0, "")
	waitFor(t, "recovery", func() bool { _, lastErr, lastOK := e.Status(); return lastErr == "" && !lastOK.IsZero() })
}

func TestConfigErrorsAreReportedAndExportNothing(t *testing.T) {
	cases := map[string]struct {
		config func(context.Context) (*Config, error)
		want   string
	}{
		"missing secret": {
			config: func(context.Context) (*Config, error) { return nil, errors.New("OTLP headers secret missing") },
			want:   "OTLP headers secret missing",
		},
		"invalid endpoint": {
			config: fixedConfig(&Config{Endpoint: "ftp://collector:4318"}),
			want:   "OTLP endpoint must be an http or https URL",
		},
	}
	for name, tc := range cases {
		e := &Exporter{Env: "lab", Config: tc.config, exportEvery: 20 * time.Millisecond, configEvery: time.Hour}
		startExporter(t, e)
		waitFor(t, name, func() bool { _, lastErr, _ := e.Status(); return lastErr != "" })
		configured, lastErr, _ := e.Status()
		if configured || !strings.HasPrefix(lastErr, tc.want) {
			t.Fatalf("%s: configured %v, lastErr %q", name, configured, lastErr)
		}
	}
}

func TestNoConfigFunctionMeansNoExport(t *testing.T) {
	e := &Exporter{Env: "lab", exportEvery: 20 * time.Millisecond, configEvery: time.Hour}
	startExporter(t, e)
	if configured, lastErr, lastOK := e.Status(); configured || lastErr != "" || !lastOK.IsZero() {
		t.Fatalf("status %v %q %v without a config function", configured, lastErr, lastOK)
	}
}

func TestRunReturnsAfterCancelAfterFinalFlush(t *testing.T) {
	rc := newReceiver(t)
	cfg := &Config{Endpoint: rc.srv.URL}
	e := &Exporter{Env: "lab", Collector: testCollector(t), Config: fixedConfig(cfg), exportEvery: time.Hour, configEvery: time.Hour}
	cancel, done := startExporter(t, e)
	waitFor(t, "the export to be configured", func() bool { configured, _, _ := e.Status(); return configured })
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context was canceled")
	}
	// The export interval is an hour, so the only export is the final flush on shutdown.
	if rc.count() == 0 {
		t.Fatal("the final flush sent nothing")
	}
}
