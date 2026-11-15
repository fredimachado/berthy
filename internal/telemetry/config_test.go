package telemetry

import (
	"maps"
	"strings"
	"testing"
)

func TestParseHeaders(t *testing.T) {
	ok := map[string]struct {
		in   string
		want map[string]string
	}{
		"empty":            {"", map[string]string{}},
		"blank":            {"   ", map[string]string{}},
		"two pairs":        {"authorization=Bearer%20abc,x-team=ops", map[string]string{"authorization": "Bearer abc", "x-team": "ops"}},
		"spaces":           {" a = b ,  c=d ", map[string]string{"a": "b", "c": "d"}},
		"encoded plus":     {"k=a%2Bb", map[string]string{"k": "a+b"}},
		"literal plus":     {"k=a+b", map[string]string{"k": "a+b"}},
		"equals in value":  {"k=abc==", map[string]string{"k": "abc=="}},
		"empty entries":    {"a=1,,b=2,", map[string]string{"a": "1", "b": "2"}},
		"empty value":      {"x-empty=", map[string]string{"x-empty": ""}},
		"last name wins":   {"a=1,a=2", map[string]string{"a": "2"}},
		"token characters": {"x_y.z!=ok", map[string]string{"x_y.z!": "ok"}},
	}
	for name, tc := range ok {
		got, err := ParseHeaders(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !maps.Equal(got, tc.want) {
			t.Fatalf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

func TestParseHeadersRefusesMalformedEntriesWithoutEchoingValues(t *testing.T) {
	const secret = "SECRET-PART-7731"
	bad := []string{
		"authorization Bearer " + secret,      // no '='
		"bad name=" + secret,                  // space in the name
		"=" + secret,                          // empty name
		"k=%ZZ" + secret,                      // invalid percent-encoding
		"k=" + secret + "\nx-injected: 1",     // control character in the value
		"k=" + secret + "\x00",                // NUL in the value
		"ok=1," + "authorization " + secret,   // a later entry is malformed
		"k=" + secret + "\x7f",                // DEL in the value
		"a\x01b=" + secret,                    // control character in the name
		"valid=1,name\tvalue=" + secret + "%", // tab in the name, truncated escape
	}
	for _, in := range bad {
		headers, err := ParseHeaders(in)
		if err == nil {
			t.Fatalf("accepted %q as %v", in, headers)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error echoes a value: %v", err)
		}
	}
}

func TestMetricsURL(t *testing.T) {
	ok := map[string]string{
		"http://10.0.0.5:4318":             "http://10.0.0.5:4318/v1/metrics",
		"http://10.0.0.5:4318/":            "http://10.0.0.5:4318/v1/metrics",
		"http://10.0.0.5:4318//":           "http://10.0.0.5:4318/v1/metrics",
		"http://10.0.0.5:4318/v1/metrics":  "http://10.0.0.5:4318/v1/metrics",
		"http://10.0.0.5:4318/v1/metrics/": "http://10.0.0.5:4318/v1/metrics",
		"https://otel.example.com/otlp":    "https://otel.example.com/otlp/v1/metrics",
		"https://otel.example.com/otlp/":   "https://otel.example.com/otlp/v1/metrics",
		"http://collector:4318/v1/traces":  "http://collector:4318/v1/traces/v1/metrics",
		"HTTP://Collector:4318":            "http://Collector:4318/v1/metrics",
	}
	for in, want := range ok {
		got, err := MetricsURL(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != want {
			t.Fatalf("%s: got %s, want %s", in, got, want)
		}
	}
	bad := []string{"", "ftp://collector:4318", "http://", "http://user:pass@collector:4318", "http://collector:4318?token=1", "http://collector:4318#frag", "collector:4318", "/v1/metrics"}
	for _, in := range bad {
		if got, err := MetricsURL(in); err == nil {
			t.Fatalf("%q accepted as %s", in, got)
		}
	}
}

func TestEndpointTargetHasNoPath(t *testing.T) {
	if got := endpointTarget("https://otel.example.com:4318/otlp/v1/metrics"); got != "https://otel.example.com:4318" {
		t.Fatalf("target %s", got)
	}
}

func TestRedactRemovesURLPathAndHeaderValues(t *testing.T) {
	msg := `failed to send metrics to http://10.0.0.5:4318/v1/metrics: 500 Internal Server Error (body: rejected token SECRET-ABC and SECRET-ABCDEF)`
	got := redact(msg, "http://10.0.0.5:4318/v1/metrics", []string{"SECRET-ABC", "SECRET-ABCDEF"})
	want := `failed to send metrics to http://10.0.0.5:4318: 500 Internal Server Error (body: rejected token [redacted] and [redacted])`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if redact("plain", "", nil) != "plain" {
		t.Fatal("text without a URL or values changed")
	}
	long := redact("line one\nline two\tend "+strings.Repeat("a", 2000), "", nil)
	if len(long) > maxErrorText+len("...") || strings.ContainsAny(long, "\n\t") || !strings.HasSuffix(long, "...") {
		t.Fatalf("long or multi-line text kept: %d bytes, %q", len(long), long[:40])
	}
}

func TestCheckValues(t *testing.T) {
	want := map[string]int64{"ok": 0, "warning": 1, "critical": 2, "unknown": -1}
	for state, v := range want {
		if got, ok := checkValue(state); !ok || got != v {
			t.Fatalf("%s: got %d %v, want %d", state, got, ok, v)
		}
	}
	if _, ok := checkValue("paused"); ok {
		t.Fatal("unknown state has a value")
	}
}
