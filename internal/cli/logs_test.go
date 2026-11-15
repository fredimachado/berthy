package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
