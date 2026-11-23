package cli

import (
	"fmt"
	"io"
	"os"

	"berthy/internal/scope"

	"github.com/charmbracelet/x/term"
)

// readOnlyCommands are the keys of changeFlags that change nothing. Every other key gets the banner, so a command added
// later gets it unless it is listed here.
var readOnlyCommands = []string{"wait", "history", "diagnostics", "audit", "lock status", "ingress status", "secret list",
	"job logs", "backup list", "approvals", "token list", "config show", "config validate"}

// stderrTerminal reports whether w is a terminal. Tests replace it.
var stderrTerminal = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

// printBanner prints "berthy: <id> <where>" to errout when errout is a terminal, with the BERTHY_ENV note when
// fromVariable. where is "at <agent URL>" for a remote command and "on <target>" for setup.
func printBanner(errout io.Writer, id scope.ID, where string, fromVariable bool) {
	if !stderrTerminal(errout) {
		return
	}
	line := "berthy: " + terminalSafe(id.String()) + " " + terminalSafe(where)
	if fromVariable {
		line += " (environment from BERTHY_ENV)"
	}
	fmt.Fprintln(errout, line)
}
