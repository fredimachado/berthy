package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"berthy/internal/api"
	"berthy/internal/scope"
)

// printKeyRotation prints what `key rotate` did, or with a dry run what it would do. It names keys by ID and prints the command
// that copies the new key off the server, never the key.
func printKeyRotation(out, errout io.Writer, data []byte, id scope.ID) int {
	var rep api.KeyRotation
	if json.Unmarshal(data, &rep) != nil || rep.OldKeyID == "" || rep.KeyFile == "" || (!rep.DryRun && rep.NewKeyID == "") {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	if rep.DryRun {
		fmt.Fprintf(out, "dry run: a rotation would seal %d values again with a new key; nothing changed\n", rep.Values)
		return 0
	}
	fmt.Fprintf(out, "rotated the state key: %d values resealed, key %s replaces %s\n", rep.Values, terminalSafe(rep.NewKeyID), terminalSafe(rep.OldKeyID))
	fmt.Fprintln(out, "the previous key is kept on the server until the next state snapshot succeeds")
	keyFile := terminalSafe(rep.KeyFile)
	posix, powershell := keyCopyForms("sudo cat "+shellWord(keyFile), keyFile, "", id, true)
	fmt.Fprintf(out, "copy the new key off the server now: macOS/Linux: %s Windows PowerShell (replace <user>@<server>): %s\n", posix, powershell)
	return 0
}
