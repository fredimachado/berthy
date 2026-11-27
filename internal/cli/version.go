package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"

	"berthy/internal/api"
	"berthy/internal/version"
)

// runVersion prints the version of this CLI and its platform: "berthy <version> <os>/<arch>", or with --json an object that
// also says whether the build is a release. It takes no argument and no flag but --json. It needs no agent.
func runVersion(args []string, out, errout io.Writer) int {
	jsonOutput := false
	for _, a := range args {
		switch {
		case a == "--json":
			jsonOutput = true
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(errout, "unknown flag %s\n", a)
			return 2
		default:
			fmt.Fprintln(errout, "version takes no argument")
			return 2
		}
	}
	if !jsonOutput {
		fmt.Fprintf(out, "berthy %s %s/%s\n", version.String(), runtime.GOOS, runtime.GOARCH)
		return 0
	}
	b, err := json.MarshalIndent(struct {
		Version string `json:"version"`
		Release bool   `json:"release"`
		OS      string `json:"os"`
		Arch    string `json:"arch"`
	}{version.String(), version.Release(), runtime.GOOS, runtime.GOARCH}, "", "  ")
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	fmt.Fprintln(out, string(b))
	return 0
}

// doctorAgentVersion returns the agent's version as the doctor's agent check reports it. It is false when the agent reports
// none, as an agent older than that check does not.
func doctorAgentVersion(body []byte) (string, bool) {
	var d struct {
		Checks []api.Check `json:"checks"`
	}
	if json.Unmarshal(body, &d) != nil {
		return "", false
	}
	for _, c := range d.Checks {
		if c.Name == "agent" {
			return c.Detail, true
		}
	}
	return "", false
}

// versionNote is the doctor's note when the agent runs a different release than this CLI, and "" otherwise. Dev builds on
// either side never warn. Equal precedence (including tags that differ only in build metadata) prints no note. agent is
// the agent's version, or empty when it reports none.
func versionNote(agent string) string {
	cliVersion := version.String()
	if !version.Release() || !version.IsRelease(agent) || agent == cliVersion {
		return ""
	}
	if c, ok := version.Compare(agent, cliVersion); ok {
		if c == 0 {
			return ""
		}
		if c > 0 {
			return fmt.Sprintf("note: the agent runs %s, this CLI is %s; upgrade this CLI", agent, cliVersion)
		}
	}
	return fmt.Sprintf("note: the agent runs %s, this CLI is %s; berthy setup upgrades the agent", agent, cliVersion)
}
