package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"berthy/internal/deploy"
)

// printApply prints what berthy apply changed in the environment's settings, or with a dry run would change, one line each.
// Nothing to change prints "no changes".
func printApply(out, errout io.Writer, data []byte, dry bool) int {
	var result deploy.ApplyResult
	if json.Unmarshal(data, &result) != nil {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	if len(result.Changes) == 0 {
		fmt.Fprintln(out, "no changes")
		return 0
	}
	if dry {
		fmt.Fprintln(out, "would change the environment settings:")
	} else {
		fmt.Fprintln(out, "changed the environment settings:")
	}
	for _, line := range result.Changes {
		fmt.Fprintln(out, "  "+terminalSafe(line))
	}
	return 0
}
