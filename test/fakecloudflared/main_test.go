package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseAcceptsBerthyInvocations(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want invocation
	}{
		{"connector", []string{"tunnel", "--no-autoupdate", "--metrics", "0.0.0.0:20241", "run"}, invocation{verb: "run", metrics: "0.0.0.0:20241"}},
		{"readiness", []string{"tunnel", "--metrics", "127.0.0.1:20241", "ready"}, invocation{verb: "ready", metrics: "127.0.0.1:20241"}},
		{"flags before tunnel", []string{"--no-autoupdate", "--metrics", "0.0.0.0:20241", "tunnel", "run"}, invocation{verb: "run", metrics: "0.0.0.0:20241"}},
		{"flag after the subcommand word", []string{"tunnel", "--metrics", "127.0.0.1:20241", "--no-autoupdate", "run"}, invocation{verb: "run", metrics: "127.0.0.1:20241"}},
		{"equals form", []string{"--metrics=127.0.0.1:9", "tunnel", "ready"}, invocation{verb: "ready", metrics: "127.0.0.1:9"}},
		{"default metrics", []string{"tunnel", "run"}, invocation{verb: "run", metrics: defaultMetrics}},
	}
	for _, c := range cases {
		got, err := parse(c.args)
		if err != nil {
			t.Errorf("%s: parse(%q) = %v", c.name, c.args, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: parse(%q) = %+v, want %+v", c.name, c.args, got, c.want)
		}
	}
}

func TestParseRefusesEveryOtherInvocation(t *testing.T) {
	cases := map[string][]string{
		"no arguments":          nil,
		"version":               {"version"},
		"bare run":              {"run"},
		"tunnel alone":          {"tunnel"},
		"unknown subcommand":    {"tunnel", "delete"},
		"extra word":            {"tunnel", "run", "extra"},
		"two subcommands":       {"tunnel", "ready", "run"},
		"unknown flag":          {"tunnel", "--bogus", "run"},
		"metrics without value": {"tunnel", "--metrics"},
		"metrics given a flag":  {"tunnel", "--metrics", "--no-autoupdate", "run"},
		"empty equals form":     {"tunnel", "--metrics=", "run"},
		"empty metrics value":   {"tunnel", "--metrics", "", "run"},
	}
	for name, args := range cases {
		if _, err := parse(args); err == nil {
			t.Errorf("%s: parse(%q) accepted an invocation Berthy does not make", name, args)
		}
	}
}

func TestRunExitsTwoForAnOtherInvocation(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"version"}, func(string) string { return "token" }, io.Discard, &stderr)
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunWithoutTokenExitsOneAndNamesOnlyTheVariable(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"tunnel", "--no-autoupdate", "--metrics", "127.0.0.1:0", "run"}, func(string) string { return "" }, io.Discard, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "TUNNEL_TOKEN") {
		t.Fatalf("stderr = %q, want the variable name", stderr.String())
	}
}

func TestRunDoesNotEchoTheTokenWhenTheConnectorCannotStart(t *testing.T) {
	const token = "SECRET-TUNNEL-TOKEN-VALUE"
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	var stderr bytes.Buffer
	args := []string{"tunnel", "--no-autoupdate", "--metrics", busy.Addr().String(), "run"}
	code := run(args, func(name string) string {
		if name == "TUNNEL_TOKEN" {
			return token
		}
		return ""
	}, io.Discard, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, want 1 when the metrics address is taken", code)
	}
	if strings.Contains(stderr.String(), token) {
		t.Fatal("stderr echoes the tunnel token")
	}
}

// fakeClock is a clock a test moves by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// lockedBuffer is a log writer a test can read while the connector is still writing to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startConnector serves a connector on a loopback port and returns its address, a cancel function and a channel that
// receives serveConnector's result.
func startConnector(t *testing.T, conn connector, every time.Duration, logw io.Writer) (string, context.CancelFunc, chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveConnector(ctx, ln, conn, every, logw) }()
	return ln.Addr().String(), cancel, done
}

func readyStatus(t *testing.T, addr string) int {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/ready")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestReadyIsUnavailableUntilTheReadinessDelayPasses(t *testing.T) {
	start := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: start}
	conn := connector{started: start, readyAfter: readyAfter, now: clock.Now}
	addr, cancel, done := startConnector(t, conn, time.Hour, io.Discard)

	if got := readyStatus(t, addr); got != http.StatusServiceUnavailable {
		t.Fatalf("ready at start: HTTP %d, want 503", got)
	}
	clock.Advance(readyAfter - time.Second)
	if got := readyStatus(t, addr); got != http.StatusServiceUnavailable {
		t.Fatalf("ready before the delay: HTTP %d, want 503", got)
	}
	clock.Advance(time.Second)
	if got := readyStatus(t, addr); got != http.StatusOK {
		t.Fatalf("ready after the delay: HTTP %d, want 200", got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serveConnector after cancel: %v", err)
	}
}

func TestConnectorLogsRunningPeriodicallyAndStopsOnCancel(t *testing.T) {
	logw := &lockedBuffer{}
	start := time.Now()
	conn := connector{started: start, readyAfter: readyAfter, now: time.Now}
	_, cancel, done := startConnector(t, conn, 10*time.Millisecond, logw)

	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(logw.String(), "connector running") < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := strings.Count(logw.String(), "connector running"); n < 2 {
		t.Fatalf("connector running logged %d times, want at least 2:\n%s", n, logw.String())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serveConnector: %v", err)
	}
	if !strings.Contains(logw.String(), "connector stopped") {
		t.Fatalf("log lacks the stop line:\n%s", logw.String())
	}
}

func TestCheckReadyExitsZeroOnlyOnHTTP200(t *testing.T) {
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer ready.Close()
	notReady := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer notReady.Close()
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedAddr := strings.TrimPrefix(closed.URL, "http://")
	closed.Close()

	var stderr bytes.Buffer
	if code := checkReady(strings.TrimPrefix(ready.URL, "http://"), &stderr); code != 0 {
		t.Errorf("ready: exit %d, want 0 (%s)", code, stderr.String())
	}
	stderr.Reset()
	if code := checkReady(strings.TrimPrefix(notReady.URL, "http://"), &stderr); code != 1 {
		t.Errorf("not ready: exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "503") {
		t.Errorf("not ready message = %q, want the status", stderr.String())
	}
	stderr.Reset()
	if code := checkReady(closedAddr, &stderr); code != 1 {
		t.Errorf("unreachable: exit %d, want 1", code)
	}
}
