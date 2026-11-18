package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"berthy/internal/api"
)

// printKeyRotation prints what `key rotate` did, or with a dry run what it would do. It names keys by ID and prints the command
// that copies the new key off the server, never the key.
func printKeyRotation(out, errout io.Writer, data []byte, env string) int {
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
	fmt.Fprintf(out, "copy the new key off the server now: %s\n", keyCopyCommand("sudo cat "+shellWord(terminalSafe(rep.KeyFile)), env))
	return 0
}
