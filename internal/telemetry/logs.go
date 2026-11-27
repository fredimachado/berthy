package telemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

const (
	// logQueueSize bounds the records that wait to be sent. A record that arrives when the queue is full is dropped, so an
	// operation never waits for the backend.
	logQueueSize = 256
	// logBatchSize is the most records one request carries.
	logBatchSize = 64
)

// Severity is the severity of a log record.
type Severity int

const (
	SeverityInfo Severity = iota
	SeverityWarn
)

// Attr is one attribute of a log record: a string, or an integer when IsInt is set.
type Attr struct {
	Key   string
	Str   string
	Int   int64
	IsInt bool
}

// String returns a string attribute.
func String(key, value string) Attr { return Attr{Key: key, Str: value} }

// Int returns an integer attribute.
func Int(key string, value int64) Attr { return Attr{Key: key, Int: value, IsInt: true} }

// LogRecord is one event for the OpenTelemetry backend.
type LogRecord struct {
	Time     time.Time
	Severity Severity
	Body     string
	Attrs    []Attr
}

// Logs sends log records to the same OpenTelemetry backend as the metrics. Emit queues a record and never waits; Run sends the
// queue in batches until its context ends. Config returns the export settings of the running manifest and is read for
// every batch, so a change of endpoint is followed at once; a nil config, or no Config function, drops the record. The zero
// value is ready to use, and records emitted before Run starts are kept, up to the queue's size.
type Logs struct {
	// Env is the environment name, which becomes the deployment.environment.name resource attribute.
	Env string
	// Config returns the export settings. It is the function the metrics exporter uses.
	Config func(ctx context.Context) (*Config, error)
	// Log receives operational messages. They never contain a header value. Nil discards them.
	Log func(string)

	once    sync.Once
	queue   chan LogRecord
	mu      sync.Mutex
	lastErr string // the last problem logged, so a repeat is not logged again
	dropped int    // records dropped since the last message about it
}

func (l *Logs) q() chan LogRecord {
	l.once.Do(func() { l.queue = make(chan LogRecord, logQueueSize) })
	return l.queue
}

// Emit queues a record. It returns at once: a record that finds the queue full is dropped and counted.
func (l *Logs) Emit(r LogRecord) {
	select {
	case l.q() <- r:
		return
	default:
	}
	l.mu.Lock()
	first := l.dropped == 0
	l.dropped++
	l.mu.Unlock()
	if first {
		l.log("telemetry: log queue is full, records are dropped")
	}
}

// Run sends queued records until ctx ends, and then sends what is still queued for up to 5 seconds.
func (l *Logs) Run(ctx context.Context) {
	q := l.q()
	for {
		select {
		case <-ctx.Done():
			l.flush()
			return
		case first := <-q:
			l.send(l.collect(first))
		}
	}
}

// collect returns first and the records that are queued behind it, up to a batch.
func (l *Logs) collect(first LogRecord) []LogRecord {
	batch := []LogRecord{first}
	for len(batch) < logBatchSize {
		select {
		case r := <-l.q():
			batch = append(batch, r)
		default:
			return batch
		}
	}
	return batch
}

// flush sends every queued record, for up to the shutdown time.
func (l *Logs) flush() {
	deadline := time.Now().Add(shutdownTimeout)
	for time.Now().Before(deadline) {
		select {
		case first := <-l.q():
			l.send(l.collect(first))
		default:
			return
		}
	}
}

// send posts one batch. A failure is logged once per kind and the batch is dropped.
func (l *Logs) send(batch []LogRecord) {
	cctx, cancel := context.WithTimeout(context.Background(), configTimeout)
	var cfg *Config
	var err error
	if l.Config != nil {
		cfg, err = l.Config(cctx)
	}
	cancel()
	if err != nil {
		l.failed(err.Error())
		return
	}
	if cfg == nil {
		return
	}
	url, err := LogsURL(cfg.Endpoint)
	if err != nil {
		l.failed(err.Error())
		return
	}
	secrets := slices.Collect(maps.Values(cfg.Headers))
	if err := l.post(url, cfg.Headers, l.request(batch)); err != nil {
		l.failed(redact(err.Error(), url, secrets))
		return
	}
	l.mu.Lock()
	recovered := l.lastErr != ""
	l.lastErr = ""
	l.mu.Unlock()
	if recovered {
		l.log("telemetry: log export succeeded again")
	}
}

// request builds the OTLP request that carries a batch.
func (l *Logs) request(batch []LogRecord) *collogspb.ExportLogsServiceRequest {
	host, _ := os.Hostname() //nolint:errcheck // Hostname is optional metadata on the exported logs.
	records := make([]*logspb.LogRecord, 0, len(batch))
	for _, r := range batch {
		ts := uint64(r.Time.UnixNano())
		rec := &logspb.LogRecord{
			TimeUnixNano:         ts,
			ObservedTimeUnixNano: ts,
			SeverityNumber:       logspb.SeverityNumber_SEVERITY_NUMBER_INFO,
			SeverityText:         "INFO",
			Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: r.Body}},
		}
		if r.Severity == SeverityWarn {
			rec.SeverityNumber, rec.SeverityText = logspb.SeverityNumber_SEVERITY_NUMBER_WARN, "WARN"
		}
		for _, a := range r.Attrs {
			rec.Attributes = append(rec.Attributes, attrValue(a))
		}
		records = append(records, rec)
	}
	return &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			attrValue(String("service.name", serviceName)),
			attrValue(String("deployment.environment.name", l.Env)),
			attrValue(String("host.name", host)),
		}},
		ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Name: meterName}, LogRecords: records}},
	}}}
}

func attrValue(a Attr) *commonpb.KeyValue {
	v := &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: a.Str}}
	if a.IsInt {
		v = &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: a.Int}}
	}
	return &commonpb.KeyValue{Key: a.Key, Value: v}
}

// post sends the request to url with the headers. Its errors may name the URL; the caller redacts them.
func (l *Logs) post(url string, headers map[string]string, req *collogspb.ExportLogsServiceRequest) error {
	body, err := proto.Marshal(req)
	if err != nil {
		return errors.New("log export could not be encoded")
	}
	ctx, cancel := context.WithTimeout(context.Background(), exportTimeout)
	defer cancel()
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return errors.New("log export request could not be created")
	}
	for k, v := range headers {
		hr.Header.Set(k, v)
	}
	hr.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) //nolint:errcheck // Draining only permits connection reuse; the status determines export success.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("log export answered with status %d", resp.StatusCode)
	}
	return nil
}

// failed logs a problem unless it is the one logged last.
func (l *Logs) failed(msg string) {
	l.mu.Lock()
	repeat := msg == l.lastErr
	l.lastErr = msg
	l.mu.Unlock()
	if !repeat {
		l.log("telemetry: log export failed: " + msg)
	}
}

func (l *Logs) log(msg string) {
	if l.Log != nil {
		l.Log(msg)
	}
}
