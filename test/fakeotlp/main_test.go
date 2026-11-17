package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

// testExpected is an Authorization value of the shape the agent's headers secret produces, made up for these tests.
const testExpected = "Bearer TEST-ONLY-FAKEOTLP-7"

func newTestReceiver(t *testing.T) (*receiver, string) {
	t.Helper()
	summaryPath := filepath.Join(t.TempDir(), "summary.json")
	rc := &receiver{
		expected:    testExpected,
		summaryPath: summaryPath,
		logw:        io.Discard,
		now:         func() time.Time { return time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC) },
		sum:         summary{ResourceAttributes: map[string]string{}, MetricNames: []string{}, HeaderNames: []string{}},
		names:       map[string]bool{},
	}
	if err := rc.writeSummary(); err != nil {
		t.Fatal(err)
	}
	return rc, summaryPath
}

// exportBody returns an OTLP export request with a resource and two metrics, gzip-compressed when gz is set.
func exportBody(t *testing.T, gz bool) []byte {
	t.Helper()
	req := &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "berthy-agent"}}}}},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
			{Name: "system.cpu.utilization", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: 0.25}}}}}},
			{Name: "system.disk.io", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{DataPoints: []*metricspb.NumberDataPoint{{Value: &metricspb.NumberDataPoint_AsInt{AsInt: 4096}}, {Value: &metricspb.NumberDataPoint_AsInt{AsInt: 2048}}}}}},
		}}},
	}}}
	b, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if !gz {
		return b
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func readSummary(t *testing.T, path string) summary {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s summary
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAcceptsOTLPAndRecordsOnlyNames(t *testing.T) {
	rc, summaryPath := newTestReceiver(t)
	srv := httptest.NewServer(rc)
	defer srv.Close()

	for _, gz := range []bool{false, true} {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/metrics", bytes.NewReader(exportBody(t, gz)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Authorization", testExpected)
		if gz {
			req.Header.Set("Content-Encoding", "gzip")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("gzip %v: status %d", gz, resp.StatusCode)
		}
	}

	s := readSummary(t, summaryPath)
	if s.Requests != 2 || !s.AuthorizationMatched {
		t.Fatalf("requests %d, authorization matched %v", s.Requests, s.AuthorizationMatched)
	}
	if !slices.Contains(s.HeaderNames, "Authorization") {
		t.Fatalf("header names %v do not list Authorization", s.HeaderNames)
	}
	if got := s.ResourceAttributes["service.name"]; got != "berthy-agent" {
		t.Fatalf("resource service.name %q", got)
	}
	if !slices.Equal(s.MetricNames, []string{"system.cpu.utilization", "system.disk.io"}) {
		t.Fatalf("metric names %v", s.MetricNames)
	}
	if s.DataPoints != 6 {
		t.Fatalf("data points %d, want 6 over two requests", s.DataPoints)
	}
	raw, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "TEST-ONLY-FAKEOTLP-7") || strings.Contains(string(raw), "Bearer") {
		t.Fatal("the summary holds the authorization value")
	}
}

func TestMismatchedAuthorizationIsRecordedAsNotMatched(t *testing.T) {
	rc, summaryPath := newTestReceiver(t)
	srv := httptest.NewServer(rc)
	defer srv.Close()
	for _, auth := range []string{"", "Bearer wrong"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/metrics", bytes.NewReader(exportBody(t, false)))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if s := readSummary(t, summaryPath); s.AuthorizationMatched {
			t.Fatalf("authorization %q counted as matched", auth)
		}
	}
}

func TestRefusesWhatIsNotAnExport(t *testing.T) {
	rc, summaryPath := newTestReceiver(t)
	srv := httptest.NewServer(rc)
	defer srv.Close()
	cases := []struct {
		method, path string
		body         []byte
		gzip         bool
		status       int
	}{
		{http.MethodGet, "/v1/metrics", nil, false, http.StatusNotFound},
		{http.MethodPost, "/v1/traces", exportBody(t, false), false, http.StatusNotFound},
		{http.MethodPost, "/v1/metrics", []byte("not protobuf \xff\xff"), false, http.StatusBadRequest},
		{http.MethodPost, "/v1/metrics", []byte{0x1f, 0x8b, 0x00}, true, http.StatusBadRequest},
	}
	for _, tc := range cases {
		req, _ := http.NewRequest(tc.method, srv.URL+tc.path, bytes.NewReader(tc.body))
		if tc.gzip {
			req.Header.Set("Content-Encoding", "gzip")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("%s %s: status %d, want %d", tc.method, tc.path, resp.StatusCode, tc.status)
		}
	}
	if s := readSummary(t, summaryPath); s.Requests != 0 {
		t.Fatalf("refused requests were counted: %d", s.Requests)
	}
}

func TestRunValidatesFlagsBeforeListening(t *testing.T) {
	dir := t.TempDir()
	expect := filepath.Join(dir, "expect")
	if err := os.WriteFile(expect, []byte(testExpected+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]string{
		"public address":     {"-listen", "0.0.0.0:18790", "-expect-authorization-file", expect},
		"no expected value":  {"-listen", "127.0.0.1:0"},
		"missing file":       {"-listen", "127.0.0.1:0", "-expect-authorization-file", filepath.Join(dir, "absent")},
		"empty expected":     {"-listen", "127.0.0.1:0", "-expect-authorization-file", writeFile(t, filepath.Join(dir, "empty"), "\n")},
		"extra argument":     {"-listen", "127.0.0.1:0", "-expect-authorization-file", expect, "extra"},
		"malformed listener": {"-listen", "127.0.0.1", "-expect-authorization-file", expect},
	}
	for name, args := range cases {
		var logs bytes.Buffer
		if code := run(args, &logs); code != 2 {
			t.Fatalf("%s: exit %d, want 2 (%s)", name, code, logs.String())
		}
		if strings.Contains(logs.String(), testExpected) {
			t.Fatalf("%s: the expected value was printed", name)
		}
	}
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// logsBody returns an OTLP logs request with two records: a deploy start and a failed end.
func logsBody(t *testing.T) []byte {
	t.Helper()
	str := func(k, v string) *commonpb.KeyValue {
		return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
	}
	req := &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{str("service.name", "berthy-agent")}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{
			{Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "deploy started"}}, SeverityText: "INFO", Attributes: []*commonpb.KeyValue{str("berthy.operation.id", "op1"), str("berthy.operation.state", "running")}},
			{Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "deploy failed"}}, SeverityText: "WARN", Attributes: []*commonpb.KeyValue{str("berthy.operation.id", "op1"), {Key: "berthy.operation.duration_ms", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 48000}}}}},
		}}},
	}}}
	b, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAcceptsLogsAndRecordsTheirBodySeverityAndAttributes(t *testing.T) {
	rc, summaryPath := newTestReceiver(t)
	srv := httptest.NewServer(rc)
	defer srv.Close()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/logs", bytes.NewReader(logsBody(t)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", testExpected)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	s := readSummary(t, summaryPath)
	if s.Requests != 1 || !s.AuthorizationMatched || s.ResourceAttributes["service.name"] != "berthy-agent" {
		t.Fatalf("summary %+v", s)
	}
	if len(s.Logs) != 2 {
		t.Fatalf("logs %+v", s.Logs)
	}
	first, second := s.Logs[0], s.Logs[1]
	if first.Body != "deploy started" || first.Severity != "INFO" || first.Attributes["berthy.operation.id"] != "op1" || first.Attributes["berthy.operation.state"] != "running" {
		t.Fatalf("first %+v", first)
	}
	if second.Body != "deploy failed" || second.Severity != "WARN" || second.Attributes["berthy.operation.duration_ms"] != "48000" {
		t.Fatalf("second %+v", second)
	}
	if len(s.MetricNames) != 0 || s.DataPoints != 0 {
		t.Fatalf("a logs request counted as metrics: %+v", s)
	}
	raw, _ := os.ReadFile(summaryPath)
	if strings.Contains(string(raw), "TEST-ONLY-FAKEOTLP-7") {
		t.Fatal("the summary holds the authorization value")
	}

	bad, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/logs", strings.NewReader("not protobuf"))
	resp, err = http.DefaultClient.Do(bad)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("garbage answered %d", resp.StatusCode)
	}
}
