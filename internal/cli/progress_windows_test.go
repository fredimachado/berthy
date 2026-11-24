//go:build windows

package cli

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestProgressWindowsConsoleMode(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "console")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := progressConsoleMode
	t.Cleanup(func() { progressConsoleMode = old })
	for _, tc := range []struct {
		name, terminal string
		mode           uint32
		err            error
		want           bool
	}{
		{"enabled without TERM", "", windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING, nil, true},
		{"disabled with TERM", "xterm", 0, nil, false},
		{"dumb with VT", "dumb", windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING, nil, false},
		{"mode unavailable", "xterm", 0, errors.New("not a console"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			progressConsoleMode = func(handle windows.Handle, mode *uint32) error {
				if uintptr(handle) != f.Fd() {
					t.Errorf("wrong output handle")
				}
				*mode = tc.mode
				return tc.err
			}
			if got := progressPlatformVT(f, tc.terminal); got != tc.want {
				t.Fatalf("VT = %v, want %v", got, tc.want)
			}
		})
	}
}
