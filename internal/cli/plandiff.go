package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"berthy/internal/deploy"
)

func takeDiff(plan map[string]any) (deploy.Diff, bool) {
	raw, ok := plan["diff"]
	if !ok {
		return deploy.Diff{}, false
	}
	b, err := json.Marshal(raw)
	var d deploy.Diff
	if err != nil || json.Unmarshal(b, &d) != nil {
		return deploy.Diff{}, false
	}
	delete(plan, "diff")
	return d, true
}

func diffMarker(change string) cliTone {
	switch {
	case strings.HasPrefix(change, "+ "):
		return toneOK
	case strings.HasPrefix(change, "- "):
		return toneError
	case strings.HasPrefix(change, "~ "):
		return toneWarning
	default:
		return tonePlain
	}
}

// diffMarkers retains only classification from redacted, unsanitized text.
func diffMarkers(data []byte, held bool, tokens ...string) []cliTone {
	var plan map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if dec.Decode(&plan) != nil || dec.Decode(new(any)) != io.EOF {
		return []cliTone{}
	}
	if held {
		plan, _ = plan["plan"].(map[string]any)
	}
	d, ok := takeDiff(plan)
	if !ok {
		return []cliTone{}
	}
	tones := make([]cliTone, len(d.Changes))
	for i, c := range d.Changes {
		tones[i] = diffMarker(redactSecretText(c.Change, tokens...))
	}
	return tones
}

func printStyledDiff(w io.Writer, indent string, d deploy.Diff, s cliStyle) {
	printStyledDiffMarkers(w, indent, d, s, nil)
}

func printStyledDiffMarkers(w io.Writer, indent string, d deploy.Diff, s cliStyle, markers []cliTone) {
	lines := d.Lines()
	for i, line := range lines {
		if !d.New && len(d.Changes) > 0 && i < len(d.Changes) && strings.HasSuffix(line, d.Changes[i].Change) {
			change := d.Changes[i].Change
			area := line[:len(line)-len(change)]
			tone := diffMarker(change)
			if markers != nil {
				tone = tonePlain
				if i < len(markers) {
					tone = markers[i]
				}
			}
			if tone != tonePlain && len(change) >= 2 {
				change = s.paint(tone, terminalSafe(change[:2])) + terminalSafe(change[2:])
			} else {
				change = terminalSafe(change)
			}
			fmt.Fprintln(w, indent+s.paint(toneMuted, terminalSafe(area))+change)
		} else {
			fmt.Fprintln(w, indent+terminalSafe(line))
		}
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
	diff, ok := takeDiff(plan)
	if !ok {
		printText(out, plan, "")
		return 0
	}
	app, _ := plan["app"].(string)
	delete(plan, "app")
	fmt.Fprintln(out, terminalSafe(app))
	printStyledDiff(out, "  ", diff, styleFor(out, true))
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
	printStyledDiff(out, "", d, styleFor(out, true))
	return 0
}

// releaseNumber reports whether s is the number of a release: a whole number from 1.
func releaseNumber(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n >= 1
}
