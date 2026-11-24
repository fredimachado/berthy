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
	fmt.Fprintln(out, "run berthy setup for this environment again from your machine: it saves the new key in its key folder")
	fmt.Fprintf(out, "or, on the server, in a POSIX shell, copy it by hand and then move the file to another machine: %s\n", keyCopyCommand("sudo cat "+shellWord(keyFile), id))
	return 0
}
