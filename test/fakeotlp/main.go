// Command fakeotlp is a loopback OTLP/HTTP metrics and logs receiver for end-to-end checks of the agent's telemetry export. It
// is a test tool. The agent and the CLI never import it, so it is not part of the production binaries.
//
// It accepts POST /v1/metrics and POST /v1/logs, decodes the protobuf request (gzip is accepted), and rewrites a JSON summary
// after every accepted request. The summary holds counts, names, resource attributes, the body, severity and attributes of
// each log record, the names of the request headers, and whether the Authorization header matched the value in the file given
// by -expect-authorization-file. It never holds a header value, and it never logs one. The file is read once at start and is never printed. There is no control plane: the summary is the record,
// and stopping the process simulates an outage. The listen address must be loopback.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// maxBody bounds one request. An agent sends one sample per export, far below it.
const maxBody = 16 << 20

// summary is what the receiver has seen. It holds names, counts and booleans, never a header value.
type summary struct {
	Requests             int               `json:"requests"`
	LastRequest          time.Time         `json:"last_request"`
	HeaderNames          []string          `json:"header_names"`
	AuthorizationMatched bool              `json:"authorization_matched"`
	ResourceAttributes   map[string]string `json:"resource_attributes"`
	MetricNames          []string          `json:"metric_names"`
	DataPoints           int               `json:"data_points"`
	// Logs are the log records received, in order, up to maxLogs.
	Logs []logEntry `json:"logs"`
}

// maxLogs bounds the log records the summary keeps.
const maxLogs = 1000

// logEntry is one log record: its body and severity text, and its attributes with integer values written in decimal.
type logEntry struct {
	Body       string            `json:"body"`
	Severity   string            `json:"severity"`
	Attributes map[string]string `json:"attributes"`
}

// receiver serves the metrics endpoint and keeps its summary.
type receiver struct {
	expected    string // the Authorization value the agent must send
	summaryPath string // optional; rewritten after every accepted request
	logw        io.Writer
	now         func() time.Time

	mu    sync.Mutex
	sum   summary
	names map[string]bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run starts the receiver and serves until interrupted. It returns the process exit code: 2 for invalid flags, 1 for a failure
// to start or serve.
func run(args []string, logw io.Writer) int {
	fs := flag.NewFlagSet("fakeotlp", flag.ContinueOnError)
	fs.SetOutput(logw)
	listen := fs.String("listen", "127.0.0.1:18790", "loopback address to serve on")
	summaryPath := fs.String("summary", "", "optional JSON file rewritten after every accepted request")
	expectFile := fs.String("expect-authorization-file", "", "file holding the Authorization value to expect; read once and never printed")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(logw, "unexpected arguments")
		return 2
	}
	if err := checkLoopback(*listen); err != nil {
		fmt.Fprintln(logw, err)
		return 2
	}
	if *expectFile == "" {
		fmt.Fprintln(logw, "-expect-authorization-file is required")
		return 2
	}
	expected, err := readExpected(*expectFile)
	if err != nil {
		fmt.Fprintln(logw, err)
		return 2
	}
	rc := &receiver{
		expected:    expected,
		summaryPath: *summaryPath,
		logw:        logw,
		now:         time.Now,
		sum:         summary{ResourceAttributes: map[string]string{}, MetricNames: []string{}, HeaderNames: []string{}, Logs: []logEntry{}},
		names:       map[string]bool{},
	}
	if err := rc.writeSummary(); err != nil {
		fmt.Fprintln(logw, "summary write failed:", err)
		return 1
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(logw, "listen failed:", err)
		return 1
	}
	srv := &http.Server{Handler: rc, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	fmt.Fprintf(logw, "fakeotlp serving %s\n", ln.Addr())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(logw, "serve failed:", err)
		return 1
	}
	return 0
}

// checkLoopback refuses any listen address that is not a loopback address.
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid -listen address: %w", err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("-listen must be a loopback address")
	}
	return nil
}

// readExpected returns the Authorization value in the file, without its trailing line break.
func readExpected(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("cannot read the expected authorization file")
	}
	v := strings.TrimRight(string(b), "\r\n")
	if v == "" {
		return "", errors.New("the expected authorization file is empty")
	}
	return v, nil
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || (r.URL.Path != "/v1/metrics" && r.URL.Path != "/v1/logs") {
		http.NotFound(w, r)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		http.Error(w, "unreadable request", http.StatusBadRequest)
		return
	}
	if r.URL.Path == "/v1/logs" {
		var req collogspb.ExportLogsServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			http.Error(w, "not OTLP protobuf", http.StatusBadRequest)
			return
		}
		if err := rc.recordLogs(r, &req); err != nil {
			fmt.Fprintln(rc.logw, "summary write failed:", err)
			http.Error(w, "summary unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
		return
	}
	var req colmetricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		http.Error(w, "not OTLP protobuf", http.StatusBadRequest)
		return
	}
	if err := rc.record(r, &req); err != nil {
		fmt.Fprintln(rc.logw, "summary write failed:", err)
		http.Error(w, "summary unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

// readBody reads the request body, decompressing it when it is gzip.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if r.Header.Get("Content-Encoding") != "gzip" {
		return body, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(io.LimitReader(zr, maxBody))
}

// record adds one accepted request to the summary and rewrites the summary file.
func (rc *receiver) record(r *http.Request, req *colmetricspb.ExportMetricsServiceRequest) error {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.touch(r)
	for _, rm := range req.GetResourceMetrics() {
		for _, kv := range rm.GetResource().GetAttributes() {
			rc.sum.ResourceAttributes[kv.GetKey()] = kv.GetValue().GetStringValue()
		}
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				rc.names[m.GetName()] = true
				rc.sum.DataPoints += dataPoints(m)
			}
		}
	}
	rc.sum.MetricNames = slices.Sorted(maps.Keys(rc.names))
	fmt.Fprintf(rc.logw, "%s accepted request %d\n", rc.sum.LastRequest.Format(time.RFC3339Nano), rc.sum.Requests)
	return rc.writeSummary()
}

// touch counts a request and notes its header names and whether its Authorization value matched. The caller holds rc.mu.
func (rc *receiver) touch(r *http.Request) {
	rc.sum.Requests++
	rc.sum.LastRequest = rc.now().UTC()
	rc.sum.HeaderNames = headerNames(r.Header)
	got := r.Header.Get("Authorization")
	rc.sum.AuthorizationMatched = got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(rc.expected)) == 1
}

// recordLogs adds one accepted logs request to the summary and rewrites the summary file.
func (rc *receiver) recordLogs(r *http.Request, req *collogspb.ExportLogsServiceRequest) error {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.touch(r)
	for _, rl := range req.GetResourceLogs() {
		for _, kv := range rl.GetResource().GetAttributes() {
			rc.sum.ResourceAttributes[kv.GetKey()] = kv.GetValue().GetStringValue()
		}
		for _, sl := range rl.GetScopeLogs() {
			for _, rec := range sl.GetLogRecords() {
				if len(rc.sum.Logs) >= maxLogs {
					continue
				}
				attrs := map[string]string{}
				for _, kv := range rec.GetAttributes() {
					attrs[kv.GetKey()] = attributeText(kv.GetValue())
				}
				rc.sum.Logs = append(rc.sum.Logs, logEntry{Body: rec.GetBody().GetStringValue(), Severity: rec.GetSeverityText(), Attributes: attrs})
			}
		}
	}
	fmt.Fprintf(rc.logw, "%s accepted request %d\n", rc.sum.LastRequest.Format(time.RFC3339Nano), rc.sum.Requests)
	return rc.writeSummary()
}

// attributeText writes a string or integer attribute value as text. Any other kind gives an empty text.
func attributeText(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	}
	return ""
}

// headerNames returns the sorted names of the headers, never their values.
func headerNames(h http.Header) []string {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// dataPoints counts the data points of a metric of any kind.
func dataPoints(m *metricspb.Metric) int {
	if g := m.GetGauge(); g != nil {
		return len(g.GetDataPoints())
	}
	if s := m.GetSum(); s != nil {
		return len(s.GetDataPoints())
	}
	return 0
}

// writeSummary rewrites the summary file through a temporary file, so a reader never sees a partial file.
func (rc *receiver) writeSummary() error {
	if rc.summaryPath == "" {
		return nil
	}
	b, err := json.MarshalIndent(rc.sum, "", "  ")
	if err != nil {
		return err
	}
	tmp := rc.summaryPath + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, rc.summaryPath)
}
