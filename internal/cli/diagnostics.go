package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

type diagnosticReport struct {
	OperationID string `json:"operation_id"`
	Reason      string `json:"reason"`
	Diagnostics []struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Role      string `json:"role"`
		Index     int    `json:"index"`
		TaskID    string `json:"task_id"`
		Container string `json:"container"`
		State     string `json:"state"`
		Error     string `json:"error"`
		ExitCode  *int   `json:"exit_code"`
		OOMKilled bool   `json:"oom_killed"`
		Health    string `json:"health"`
		HealthLog []struct {
			Start    time.Time `json:"start"`
			ExitCode int       `json:"exit_code"`
			Output   string    `json:"output"`
		} `json:"health_log"`
		Logs      string `json:"logs"`
		Truncated bool   `json:"truncated"`
	} `json:"diagnostics"`
}

// printDiagnostics writes the evidence of a failed operation for a person: one header line per diagnostic, its health check
// results, and the tail of its output indented. Output of the app is untrusted, so control characters are made visible.
func printDiagnostics(out, errout io.Writer, body []byte) int {
	var r diagnosticReport
	if json.Unmarshal(body, &r) != nil {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	fmt.Fprintf(out, "operation: %s\n", terminalSafe(r.OperationID))
	if r.Reason != "" {
		fmt.Fprintf(out, "reason: %s\n", terminalSafe(r.Reason))
	}
	for i, d := range r.Diagnostics {
		fmt.Fprintln(out)
		header := []string{d.Kind}
		if d.Kind == "task" {
			header = append(header, terminalSafe(d.Role))
			if d.Name != "" {
				header = append(header, "("+terminalSafe(d.Name)+")")
			}
		} else {
			header = append(header, terminalSafe(d.Name))
			if d.Index > 0 {
				header = append(header, fmt.Sprintf("#%d", d.Index))
			}
		}
		fmt.Fprintf(out, "[%d] %s\n", i+1, strings.Join(header, " "))
		var facts []string
		add := func(k, v string) {
			if v != "" {
				facts = append(facts, k+"="+terminalSafe(v))
			}
		}
		add("task", d.TaskID)
		add("container", d.Container)
		add("state", d.State)
		if d.ExitCode != nil {
			facts = append(facts, fmt.Sprintf("exit=%d", *d.ExitCode))
		}
		if d.OOMKilled {
			facts = append(facts, "oom=true")
		}
		add("health", d.Health)
		if len(facts) > 0 {
			fmt.Fprintf(out, "    %s\n", strings.Join(facts, " "))
		}
		if d.Error != "" {
			fmt.Fprintf(out, "    error: %s\n", terminalSafe(d.Error))
		}
		for _, probe := range d.HealthLog {
			fmt.Fprintf(out, "    health check %s exit %d: %s\n", probe.Start.UTC().Format(time.RFC3339), probe.ExitCode, terminalSafe(strings.TrimSpace(probe.Output)))
		}
		if d.Logs != "" {
			fmt.Fprintln(out, "    logs:")
			for _, line := range strings.Split(strings.TrimRight(d.Logs, "\n"), "\n") {
				fmt.Fprintf(out, "    %s\n", terminalSafe(line))
			}
		}
		if d.Truncated {
			fmt.Fprintln(out, "    (logs truncated)")
		}
	}
	return 0
}

// terminalSafe replaces control characters, which an app's output could use to rewrite the terminal, with visible escapes.
// A line break in a single-line field becomes a space.
func terminalSafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteString("    ")
		case r == '\n' || r == '\r':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// failureHint is the pointer a failed deploy leaves for the person or agent that must find out why. The operation's own
// output already carries its reason.
func failureHint(env, id string) string {
	return fmt.Sprintf("diagnostics: berthy diagnostics %s -e %s", id, env)
}
