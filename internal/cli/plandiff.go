package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"berthy/internal/deploy"
)

// takeDiffLines removes the diff from a decoded plan and returns it as text lines, made safe for a terminal: a plain value
// comes from a manifest, which an AI agent may have sent. It returns nil, and leaves the plan as it is, when the plan has no diff.
func takeDiffLines(plan map[string]any) []string {
	raw, ok := plan["diff"]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	var d deploy.Diff
	if err != nil || json.Unmarshal(b, &d) != nil {
		return nil
	}
	delete(plan, "diff")
	lines := d.Lines()
	for i, l := range lines {
		lines[i] = terminalSafe(l)
	}
	return lines
}

func printLines(out io.Writer, indent string, lines []string) {
	for _, l := range lines {
		fmt.Fprintln(out, indent+l)
	}
}

// printDryRun prints the plan of a dry-run deploy or rollback as text: the app, what it changes, then the rest of the plan.
// A plan without a diff, such as an adoption's, prints as any answer does.
func printDryRun(out, errout io.Writer, b []byte) int {
	var v any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	plan, ok := v.(map[string]any)
	if !ok {
		printText(out, v, "")
		return 0
	}
	lines := takeDiffLines(plan)
	if lines == nil {
		printText(out, plan, "")
		return 0
	}
	app, _ := plan["app"].(string)
	delete(plan, "app")
	fmt.Fprintln(out, terminalSafe(app))
	printLines(out, "  ", lines)
	if len(plan) > 0 {
		printText(out, plan, "")
	}
	return 0
}

// printDiff prints the answer of history --diff: the lines of a diff.
func printDiff(out, errout io.Writer, b []byte) int {
	var d deploy.Diff
	if json.Unmarshal(b, &d) != nil {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	for _, l := range d.Lines() {
		fmt.Fprintln(out, terminalSafe(l))
	}
	return 0
}

// releaseNumber reports whether s is the number of a release: a whole number from 1.
func releaseNumber(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n >= 1
}
