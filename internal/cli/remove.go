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
		routes = append(routes, routeLine(ch))
	}
	section("routes and DNS records:", routes)
	section("volumes to delete:", r.Volumes)
	section("volumes kept (use --volumes to delete them):", r.KeptVolumes)
	section("blockers:", r.Blockers)
	return 0
}

// printDestroyPlan shows what destroying an environment would remove and what it would keep. Every name comes from the
// agent, so it is printed with control characters made visible.
func printDestroyPlan(out, errout io.Writer, data []byte, env string) int {
	var plan struct {
		Destroy *deploy.DestroyPlan `json:"destroy"`
	}
	if json.Unmarshal(data, &plan) != nil || plan.Destroy == nil {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	d := plan.Destroy
	fmt.Fprintf(out, "destroy %s\n", terminalSafe(env))
	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintln(out, title)
		for _, item := range items {
			fmt.Fprintln(out, "  "+terminalSafe(item))
		}
	}
	if len(d.Apps) == 0 {
		fmt.Fprintln(out, "no apps")
	}
	for _, a := range d.Apps {
		fmt.Fprintf(out, "app %s (%s)\n", terminalSafe(a.App), terminalSafe(a.Kind))
		if a.Project != "" {
			fmt.Fprintf(out, "  compose project: %s\n", terminalSafe(a.Project))
		}
		for _, s := range a.Services {
			fmt.Fprintln(out, "  service "+terminalSafe(s))
		}
		for _, ch := range a.Ingress {
			fmt.Fprintln(out, "  "+terminalSafe(routeLine(ch)))
		}
		for _, v := range a.Volumes {
			fmt.Fprintln(out, "  volume to delete: "+terminalSafe(v))
		}
		for _, v := range a.KeptVolumes {
			fmt.Fprintln(out, "  volume kept: "+terminalSafe(v))
		}
	}
	if d.RemoveConnector || len(d.Ingress) > 0 {
		fmt.Fprintln(out, "managed ingress")
		if d.RemoveConnector {
			fmt.Fprintln(out, "  the connector and the stored ingress state")
		}
		for _, ch := range d.Ingress {
			fmt.Fprintln(out, "  "+terminalSafe(routeLine(ch)))
		}
	}
	section("helper containers to remove:", d.Helpers)
	section("helper images to remove unless a container uses them:", d.HelperImages)
	section("hook and migration containers to remove:", d.OneOffs)
	section("images to remove:", d.Images)
	if t := d.Tunnel; t != nil {
		switch {
		case t.Delete:
			fmt.Fprintf(out, "tunnel %s: delete\n", terminalSafe(t.Name))
		default:
			fmt.Fprintf(out, "tunnel %s: keep\n", terminalSafe(t.Name))
			for _, r := range t.Reasons {
				fmt.Fprintln(out, "  "+terminalSafe(r))
			}
		}
	}
	fmt.Fprintln(out, "kept: secrets, the state database and history, the shared network, Docker objects Berthy did not create")
	section("blockers:", d.Blockers)
	return 0
}

func routeLine(ch deploy.IngressChange) string {
	line := ch.Hostname + ": route " + ch.Route
	if ch.DNS != "" {
		line += ", DNS " + ch.DNS
	}
	return line
}
