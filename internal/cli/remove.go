package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"berthy/internal/deploy"
)

// printRemovePlan shows what removing an app would delete and what it would leave. Every name comes from the agent, so it
// is printed with control characters made visible.
func printRemovePlan(out, errout io.Writer, data []byte) int {
	var plan struct {
		Remove *deploy.RemovePlan `json:"remove"`
	}
	if json.Unmarshal(data, &plan) != nil || plan.Remove == nil {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	r := plan.Remove
	if r.Kind == "" {
		fmt.Fprintf(out, "remove %s: already removed, nothing to do\n", terminalSafe(r.App))
		return 0
	}
	fmt.Fprintf(out, "remove %s (%s)\n", terminalSafe(r.App), terminalSafe(r.Kind))
	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintln(out, title)
		for _, item := range items {
			fmt.Fprintln(out, "  "+terminalSafe(item))
		}
	}
	if r.Project != "" {
		fmt.Fprintf(out, "compose project: %s\n", terminalSafe(r.Project))
	}
	title := "services to delete:"
	if r.Kind == "compose" {
		title = "services to bring down:"
	}
	section(title, r.Services)
	var routes []string
	for _, ch := range r.Ingress {
		line := ch.Hostname + ": route " + ch.Route
		if ch.DNS != "" {
			line += ", DNS " + ch.DNS
		}
		routes = append(routes, line)
	}
	section("routes and DNS records:", routes)
	section("volumes to delete:", r.Volumes)
	section("volumes kept (use --volumes to delete them):", r.KeptVolumes)
	section("blockers:", r.Blockers)
	return 0
}
