package agent

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// WriteStartupResult writes the final status of every hosted environment for this systemd invocation.
func (h *Host) WriteStartupResult(w io.Writer, invocation string) error {
	if len(invocation) != 32 || strings.Trim(invocation, "0") == "" ||
		strings.ContainsFunc(invocation, func(c rune) bool { return !strings.ContainsRune("0123456789abcdef", c) }) {
		return errors.New("invalid systemd invocation ID")
	}
	if len(h.startupRows) == 0 || len(h.startupRows) != len(h.ids) {
		return errors.New("startup result is incomplete")
	}
	var result strings.Builder
	fmt.Fprintf(&result, "berthy-startup-v1\t%s\t%d\n", invocation, len(h.ids))
	for i, id := range h.ids {
		row := h.startupRows[i]
		status := "started"
		if !row.OK {
			status = "failed"
		}
		fmt.Fprintf(&result, "%s\t%s\t%s\n", id, status, row.Detail)
	}
	result.WriteString("complete\n")
	n, err := io.WriteString(w, result.String())
	if err == nil && n != result.Len() {
		return io.ErrShortWrite
	}
	return err
}
