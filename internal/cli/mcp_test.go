package cli

import (
	"bytes"
	"strings"
	"testing"
)

// The mcp command is served by the mcp package; here it only has to be reachable, and it must keep stdout for the protocol.
func TestMCPCommandIsDispatchedAndKeepsStdoutClean(t *testing.T) {
	var out, errs bytes.Buffer
	if code := Run([]string{"mcp", "-e", "dev"}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "requires --config") || out.Len() != 0 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errs.String())
	}
	out.Reset()
	errs.Reset()
	if code := Run([]string{"mcp", "-e", "dev", "--config", "x.json", "--json"}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "unknown argument --json") || out.Len() != 0 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errs.String())
	}
}
