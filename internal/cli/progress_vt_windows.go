//go:build windows

package cli

import (
	"io"
	"os"

	"golang.org/x/sys/windows"
)

var progressConsoleMode = windows.GetConsoleMode

func progressPlatformVT(out io.Writer, terminal string) bool {
	if terminal == "dumb" {
		return false
	}
	f, ok := out.(*os.File)
	if !ok {
		return false
	}
	var mode uint32
	return progressConsoleMode(windows.Handle(f.Fd()), &mode) == nil && mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0
}
