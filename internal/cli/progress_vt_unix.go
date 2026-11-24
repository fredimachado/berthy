//go:build !windows

package cli

import (
	"io"
	"strings"
)

func progressPlatformVT(_ io.Writer, terminal string) bool {
	return terminal == "linux" || strings.HasPrefix(terminal, "xterm") || strings.HasPrefix(terminal, "screen") || strings.HasPrefix(terminal, "tmux") || strings.HasPrefix(terminal, "rxvt")
}
