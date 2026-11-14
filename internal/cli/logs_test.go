package cli

import (
	"bytes"
	"fmt"
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
	if readStream(strings.NewReader("event: log\ndata: {\"message\":\"hi\"}\n\n"), &out, &errs, true) == 0 {
		t.Fatal("abrupt EOF accepted")
	}
	out.Reset()
	errs.Reset()
	if readStream(strings.NewReader("event: done\ndata: {\"complete\":true}\n\n"), &out, &errs, true) != 0 {
		t.Fatal(errs.String())
	}
}
func TestIncompleteAndMalformedSSE(t *testing.T) {
	for _, input := range []string{"event: done\ndata: {\"complete\":true}", "event: done\ndata: {\"complete\":false}\n\n", "event: done\ndata: {\"complete\":true} junk\n\n", "event: log\ndata: {\"message\":\"x\"} junk\n\n"} {
		var out, errout bytes.Buffer
		if readStream(strings.NewReader(input), &out, &errout, true) == 0 {
			t.Fatalf("accepted %q", input)
		}
	}
}
func TestMultilineSSEOutputsOneJSONLine(t *testing.T) {
	var out, errs bytes.Buffer
	input := "event: log\ndata: {\"message\":\"hello\",\ndata: \"container_id\":\"abc\"}\n\nevent: done\ndata: {\"complete\":true}\n\n"
	if readStream(strings.NewReader(input), &out, &errs, true) != 0 {
		t.Fatal(errs.String())
	}
	if strings.Count(out.String(), "\n") != 1 || !strings.Contains(out.String(), `"message":"hello","container_id":"abc"`) {
		t.Fatalf("not one compact line: %q", out.String())
	}
}
