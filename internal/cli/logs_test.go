package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"berthy/internal/logs"
)

func TestMalformedResponseDoesNotRevealToken(t *testing.T) {
	token := "bty.lab." + strings.Repeat("a", 64)
	t.Setenv("BERTHY_TEST_SECRET", token)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, e := ln.Accept()
		if e == nil {
			defer c.Close()
			fmt.Fprintf(c, "%s\r\n\r\n", token)
		}
	}()
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(fmt.Sprintf(`{"environments":{"lab":{"url":"http://%s","token_env":"BERTHY_TEST_SECRET"}}}`, ln.Addr())), 0600)
	var out, errout bytes.Buffer
	if Run([]string{"ps", "-e", "lab", "--config", path}, &out, &errout) == 0 {
		t.Fatal("expected error")
	}
	if strings.Contains(errout.String(), token) {
		t.Fatal("credential leaked")
	}
}
func TestLogStreamRequiresCompletion(t *testing.T) {
	var out, errs bytes.Buffer
	if readStream(context.Background(), strings.NewReader("event: log\ndata: {\"message\":\"hi\"}\n\n"), &out, &errs, true) == 0 {
		t.Fatal("abrupt EOF accepted")
	}
	out.Reset()
	errs.Reset()
	if readStream(context.Background(), strings.NewReader("event: done\ndata: {\"complete\":true}\n\n"), &out, &errs, true) != 0 {
		t.Fatal(errs.String())
	}
}
func TestIncompleteAndMalformedSSE(t *testing.T) {
	for _, input := range []string{"event: done\ndata: {\"complete\":true}", "event: done\ndata: {\"complete\":false}\n\n", "event: done\ndata: {\"complete\":true} junk\n\n", "event: log\ndata: {\"message\":\"x\"} junk\n\n"} {
		var out, errout bytes.Buffer
		if readStream(context.Background(), strings.NewReader(input), &out, &errout, true) == 0 {
			t.Fatalf("accepted %q", input)
		}
	}
}
func TestMultilineSSEOutputsOneJSONLine(t *testing.T) {
	var out, errs bytes.Buffer
	input := "event: log\ndata: {\"message\":\"hello\",\ndata: \"container_id\":\"abc\"}\n\nevent: done\ndata: {\"complete\":true}\n\n"
	if readStream(context.Background(), strings.NewReader(input), &out, &errs, true) != 0 {
		t.Fatal(errs.String())
	}
	if strings.Count(out.String(), "\n") != 1 || !strings.Contains(out.String(), `"message":"hello","container_id":"abc"`) {
		t.Fatalf("not one compact line: %q", out.String())
	}
}
func TestReadStreamInterruptedExitsZero(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte("event: log\ndata: {\"role\":\"web\",\"replica\":\"r\",\"container_id\":\"c\",\"timestamp\":\"2026-01-01T00:00:00Z\",\"stream\":\"stdout\",\"message\":\"hi\"}\n\n"))
		cancel()
		pw.CloseWithError(context.Canceled)
	}()
	var out, errs bytes.Buffer
	if code := readStream(ctx, pr, &out, &errs, true); code != 0 {
		t.Fatalf("interrupted stream exited %d", code)
	}
	if errs.Len() != 0 {
		t.Fatalf("interrupted stream wrote to stderr: %q", errs.String())
	}
}
func TestReadStreamEOFBeforeDoneReportsUnexpectedEnd(t *testing.T) {
	var out, errs bytes.Buffer
	if code := readStream(context.Background(), strings.NewReader("event: log\ndata: {\"message\":\"hi\"}\n\n"), &out, &errs, true); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errs.String(), "agent stream ended unexpectedly") {
		t.Fatalf("stderr %q", errs.String())
	}
}

// TestLogLineIsOneReadableLine: time, source and message. The source is the role and the container's short id, so a line
// can be matched to the replica that answered; a stderr line is marked; control characters are made safe.
func TestLogLineIsOneReadableLine(t *testing.T) {
	at := time.Date(2026, 11, 17, 2, 7, 50, 0, time.UTC)
	for _, tc := range []struct {
		name string
		rec  logs.Record
		want string
	}{
		{"stdout", logs.Record{Identity: logs.Identity{Role: "web", Replica: "k2x8pq7vbn3rwm5ts6yhl4jdc", ContainerID: "f77449eeec32abcdef0123456789abcdef0123456789abcdef0123456789ab"}, Timestamp: at, Stream: "stdout", Message: "GET /version 200"},
			"2026-11-17T02:07:50Z web.f77449eeec32 GET /version 200"},
		{"stderr", logs.Record{Identity: logs.Identity{Role: "web", ContainerID: "f77449eeec32abcdef"}, Timestamp: at.Add(time.Second), Stream: "stderr", Message: "panic: something"},
			"2026-11-17T02:07:51Z web.f77449eeec32 [stderr] panic: something"},
		{"no container id falls back to the task id", logs.Record{Identity: logs.Identity{Role: "web", Replica: "k2x8pq7vbn3rwm5ts6yhl4jdc"}, Timestamp: at, Stream: "stdout", Message: "hi"},
			"2026-11-17T02:07:50Z web.k2x8pq7vbn3r hi"},
		{"no role", logs.Record{Identity: logs.Identity{ContainerID: "f77449eeec32abcdef"}, Timestamp: at, Stream: "stdout", Message: "hi"},
			"2026-11-17T02:07:50Z app.f77449eeec32 hi"},
		{"time in UTC", logs.Record{Identity: logs.Identity{Role: "web", ContainerID: "f77449eeec32abcdef"}, Timestamp: time.Date(2026, 11, 17, 4, 7, 50, 0, time.FixedZone("UTC+2", 2*3600)), Stream: "stdout", Message: "hi"},
			"2026-11-17T02:07:50Z web.f77449eeec32 hi"},
		{"control characters are made safe", logs.Record{Identity: logs.Identity{Role: "web", ContainerID: "f77449eeec32abcdef"}, Timestamp: at, Stream: "stdout", Message: "warn\x1b]0;pwned\x07\tslow\nnext"},
			`2026-11-17T02:07:50Z web.f77449eeec32 warn\x1b]0;pwned\x07    slow next`},
	} {
		if got := logLine(tc.rec); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// TestLogStreamPrintsLogLines: the follow path prints each record as a line, and --json keeps one JSON object per record.
func TestLogStreamPrintsLogLines(t *testing.T) {
	input := "event: log\ndata: {\"role\":\"web\",\"replica\":\"r1\",\"container_id\":\"f77449eeec32abcdef\",\"timestamp\":\"2026-11-17T02:07:50Z\",\"stream\":\"stdout\",\"message\":\"GET /version 200\"}\n\n" +
		"event: log\ndata: {\"role\":\"web\",\"replica\":\"r2\",\"container_id\":\"a1b2c3d4e5f6abcdef\",\"timestamp\":\"2026-11-17T02:07:51Z\",\"stream\":\"stderr\",\"message\":\"panic: something\"}\n\n" +
		"event: done\ndata: {\"complete\":true}\n\n"
	var out, errs bytes.Buffer
	if code := readStream(context.Background(), strings.NewReader(input), &out, &errs, false); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	want := "2026-11-17T02:07:50Z web.f77449eeec32 GET /version 200\n2026-11-17T02:07:51Z web.a1b2c3d4e5f6 [stderr] panic: something\n"
	if out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
	out.Reset()
	errs.Reset()
	if code := readStream(context.Background(), strings.NewReader(input), &out, &errs, true); code != 0 {
		t.Fatalf("json exit %d: %s", code, errs.String())
	}
	if strings.Count(out.String(), "\n") != 2 || !strings.Contains(out.String(), `"container_id":"f77449eeec32abcdef"`) {
		t.Fatalf("json output changed: %q", out.String())
	}
}

// TestLogsPrintsLogLinesUnlessJSON: logs without --follow reads the agent's list and prints a line per record; --json still
// prints the agent's JSON.
func TestLogsPrintsLogLinesUnlessJSON(t *testing.T) {
	body := `[{"role":"web","replica":"task-1","container_id":"f77449eeec32abcdef","timestamp":"2026-11-17T02:07:50Z","stream":"stdout","message":"GET /version 200"},` +
		`{"role":"web","replica":"task-2","container_id":"a1b2c3d4e5f6abcdef","timestamp":"2026-11-17T02:07:51Z","stream":"stderr","message":"panic: something"}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	t.Setenv("BERTHY_TEST_LOGS_TOKEN", "bty.lab."+strings.Repeat("a", 64))
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(fmt.Sprintf(`{"environments":{"lab":{"url":%q,"token_env":"BERTHY_TEST_LOGS_TOKEN"}}}`, srv.URL)), 0600)
	var out, errout bytes.Buffer
	if code := Run([]string{"logs", "api", "-e", "lab", "--config", path}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	want := "2026-11-17T02:07:50Z web.f77449eeec32 GET /version 200\n2026-11-17T02:07:51Z web.a1b2c3d4e5f6 [stderr] panic: something\n"
	if out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
	out.Reset()
	if code := Run([]string{"logs", "api", "-e", "lab", "--config", path, "--json"}, &out, &errout); code != 0 {
		t.Fatalf("json exit %d: %s", code, errout.String())
	}
	var records []logs.Record
	if err := json.Unmarshal(out.Bytes(), &records); err != nil || len(records) != 2 || records[1].ContainerID != "a1b2c3d4e5f6abcdef" {
		t.Fatalf("json output changed: %v %q", err, out.String())
	}
}

// TestPrintTextListItems: a scalar in a list is an indented item; a map in a list keeps its block.
func TestPrintTextListItems(t *testing.T) {
	var out bytes.Buffer
	printText(&out, map[string]any{"app": "api", "roles": []any{"web", "worker"}}, "")
	got := out.String()
	if !strings.Contains(got, "\n  - web\n  - worker\n") || strings.Contains(got, "\nweb\n") {
		t.Fatalf("scalars in a list not indented as items:\n%s", got)
	}
	out.Reset()
	printText(&out, []any{map[string]any{"name": "a"}}, "")
	if out.String() != "name: a\n\n" {
		t.Fatalf("map in a list changed: %q", out.String())
	}
	out.Reset()
	printText(&out, map[string]any{"plan": map[string]any{"app": "api"}}, "")
	if out.String() != "plan: \n  app: api\n" {
		t.Fatalf("nested map changed: %q", out.String())
	}
}
