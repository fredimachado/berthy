package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestUICommandIsDispatched(t *testing.T) {
	dir := isolatedUserConfig(t)
	var out, errs bytes.Buffer
	want := filepath.Join(dir, "berthy", "config.json")
	if code := Run([]string{"ui", "-e", "dev"}, &out, &errs); code != 1 || !strings.Contains(errs.String(), want) || out.Len() != 0 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errs.String())
	}
	out.Reset()
	errs.Reset()
	if code := Run([]string{"ui", "-e", "dev", "--config", "x.json", "--json"}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "unknown argument --json") {
		t.Fatalf("exit %d, stderr %q", code, errs.String())
	}
}
