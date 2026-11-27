package agent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUptimeSourceMustMatchRunningExecutable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "berthy")
	if err := os.WriteFile(path, []byte("old executable"), 0755); err != nil {
		t.Fatal(err)
	}
	running, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(dir, "replacement")
	if err := os.WriteFile(replacement, []byte("new executable"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if err := verifyUptimeProbeSource(path, running); !errors.Is(err, errUptimeExecutableChanged) {
		t.Fatalf("replaced source returned %v", err)
	}
}
