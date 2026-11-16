package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestUICommandIsDispatched(t *testing.T) {
	var out, errs bytes.Buffer
	if code := Run([]string{"ui", "-e", "dev"}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "requires --config") || out.Len() != 0 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errs.String())
	}
	out.Reset()
	errs.Reset()
	if code := Run([]string{"ui", "-e", "dev", "--config", "x.json", "--json"}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "unknown argument --json") {
		t.Fatalf("exit %d, stderr %q", code, errs.String())
	}
}
