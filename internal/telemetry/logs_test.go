package telemetry

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"
)

// logReceiver is an OTLP/HTTP logs endpoint. It decodes each accepted request and keeps the headers it arrived with.
type logReceiver struct {
	srv *httptest.Server

	mu       sync.Mutex
	status   int
	requests []receivedLogs
}

type receivedLogs struct {
	header  http.Header
	request *collogspb.ExportLogsServiceRequest
}

func newLogReceiver(t *testing.T) *logReceiver {
	t.Helper()
	rc := &logReceiver{}
	rc.srv = httptest.NewServer(http.HandlerFunc(rc.serve))
	t.Cleanup(rc.srv.Close)
	return rc
}

func (rc *logReceiver) serve(w http.ResponseWriter, r *http.Request) {
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
	if r.Method != http.MethodPost || r.URL.Path != "/v1/logs" {
		http.NotFound(w, r)
		return
	}
	if rc.status != 0 {
		w.WriteHeader(rc.status)
		_, _ = io.WriteString(w, "rejected")
		return
	}
	var req collogspb.ExportLogsServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		http.Error(w, "not OTLP protobuf", http.StatusBadRequest)
		return
	}
	rc.requests = append(rc.requests, receivedLogs{header: r.Header.Clone(), request: &req})
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

// records flattens the received requests into their log records.
func (rc *logReceiver) records() []*logspb.LogRecord {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var out []*logspb.LogRecord
	for _, rq := range rc.requests {
		for _, rl := range rq.request.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				out = append(out, sl.GetLogRecords()...)
			}
		}
	}
	return out
}

func (rc *logReceiver) first() receivedLogs {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.requests[0]
}

func startLogs(t *testing.T, l *Logs) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		l.Run(ctx)
	}()
	t.Cleanup(func() {
		stop()
		<-finished
	})
}

func sampleRecord(body string) LogRecord {
	return LogRecord{
		Time:     time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC),
		Severity: SeverityWarn,
		Body:     body,
		Attrs:    []Attr{String("berthy.environment", "prod"), String("berthy.app", "api"), Int("berthy.operation.duration_ms", 48000)},
	}
}

func TestLogsPostsProtobufRecordsWithHeadersAndResource(t *testing.T) {
	rc := newLogReceiver(t)
	cfg := &Config{Endpoint: rc.srv.URL, Headers: map[string]string{"authorization": "Bearer TEST-ONLY-2"}}
	l := &Logs{Env: "lab", Config: fixedConfig(cfg)}
	l.Emit(sampleRecord("deploy started"))
	l.Emit(LogRecord{Time: time.Date(2026, 11, 18, 10, 0, 48, 0, time.UTC), Severity: SeverityInfo, Body: "deploy succeeded"})
	startLogs(t, l)
	waitFor(t, "two log records", func() bool { return len(rc.records()) == 2 })

	req := rc.first()
	if got := req.header.Get("Authorization"); got != "Bearer TEST-ONLY-2" {
		t.Fatal("the authorization header from the secret did not arrive")
	}
	if got := req.header.Get("Content-Type"); got != "application/x-protobuf" {
		t.Fatalf("content type %q", got)
	}
	resource := map[string]string{}
	for _, kv := range req.request.GetResourceLogs()[0].GetResource().GetAttributes() {
		resource[kv.GetKey()] = kv.GetValue().GetStringValue()
	}
	if resource["service.name"] != "berthy-agent" || resource["deployment.environment.name"] != "lab" || resource["host.name"] == "" {
		t.Fatalf("resource %v", resource)
	}
	recs := rc.records()
	first, second := recs[0], recs[1]
	if first.GetBody().GetStringValue() != "deploy started" || first.GetSeverityNumber() != logspb.SeverityNumber_SEVERITY_NUMBER_WARN || first.GetSeverityText() != "WARN" {
		t.Fatalf("first record %v", first)
	}
	if first.GetTimeUnixNano() != uint64(time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC).UnixNano()) {
		t.Fatalf("time %d", first.GetTimeUnixNano())
	}
	attrs := map[string]string{}
	var duration int64
	for _, kv := range first.GetAttributes() {
		attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
		if kv.GetKey() == "berthy.operation.duration_ms" {
			duration = kv.GetValue().GetIntValue()
		}
	}
	if attrs["berthy.environment"] != "prod" || attrs["berthy.app"] != "api" || duration != 48000 {
		t.Fatalf("attributes %v duration %d", attrs, duration)
	}
	if second.GetBody().GetStringValue() != "deploy succeeded" || second.GetSeverityNumber() != logspb.SeverityNumber_SEVERITY_NUMBER_INFO || second.GetSeverityText() != "INFO" {
		t.Fatalf("second record %v", second)
	}
}

func TestLogsSendsNothingWithoutAConfiguration(t *testing.T) {
	rc := newLogReceiver(t)
	var calls atomic.Int32
	l := &Logs{Env: "lab", Config: func(context.Context) (*Config, error) { calls.Add(1); return nil, nil }}
	l.Emit(sampleRecord("deploy started"))
	startLogs(t, l)
	waitFor(t, "the configuration to be read", func() bool { return calls.Load() > 0 })
	time.Sleep(50 * time.Millisecond)
	if n := len(rc.records()); n != 0 {
		t.Fatalf("%d records sent without a configuration", n)
	}
	// And with no Config function at all.
	l2 := &Logs{Env: "lab"}
	l2.Emit(sampleRecord("deploy started"))
	startLogs(t, l2)
	time.Sleep(50 * time.Millisecond)
	if n := len(rc.records()); n != 0 {
		t.Fatalf("%d records sent without a Config function", n)
	}
}

func TestLogsFullQueueDropsInsteadOfBlocking(t *testing.T) {
	rc := newLogReceiver(t)
	sink := &logSink{}
	l := &Logs{Env: "lab", Config: fixedConfig(&Config{Endpoint: rc.srv.URL}), Log: sink.add}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Nothing runs the sender yet, so the queue fills.
		for i := 0; i < logQueueSize+100; i++ {
			l.Emit(sampleRecord("deploy started"))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Emit blocked on a full queue")
	}
	if !slices.ContainsFunc(sink.lines(), func(s string) bool { return strings.Contains(s, "dropped") }) {
		t.Fatalf("the drop was not logged: %v", sink.lines())
	}
	startLogs(t, l)
	waitFor(t, "the queued records", func() bool { return len(rc.records()) == logQueueSize })
	time.Sleep(50 * time.Millisecond)
	if n := len(rc.records()); n != logQueueSize {
		t.Fatalf("%d records arrived, want %d", n, logQueueSize)
	}
}

func TestLogsFailureIsLoggedWithoutHeaderValues(t *testing.T) {
	rc := newLogReceiver(t)
	rc.status = http.StatusInternalServerError
	sink := &logSink{}
	cfg := &Config{Endpoint: rc.srv.URL, Headers: map[string]string{"authorization": "Bearer TEST-ONLY-3"}}
	l := &Logs{Env: "lab", Config: fixedConfig(cfg), Log: sink.add}
	startLogs(t, l)
	l.Emit(sampleRecord("deploy started"))
	waitFor(t, "the failure to be logged", func() bool { return len(sink.lines()) > 0 })
	for _, line := range sink.lines() {
		if strings.Contains(line, "TEST-ONLY-3") || strings.Contains(line, "/v1/logs") {
			t.Fatalf("log line leaks the header value or the path: %s", line)
		}
		if !strings.HasPrefix(line, "telemetry: ") {
			t.Fatalf("unexpected line %s", line)
		}
	}
}

func TestLogsRunFlushesWhatIsQueuedWhenItsContextEnds(t *testing.T) {
	rc := newLogReceiver(t)
	l := &Logs{Env: "lab", Config: fixedConfig(&Config{Endpoint: rc.srv.URL})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l.Emit(sampleRecord("deploy started"))
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		l.Run(ctx)
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	if n := len(rc.records()); n != 1 {
		t.Fatalf("%d records flushed, want 1", n)
	}
}
