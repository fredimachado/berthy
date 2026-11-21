package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"text/tabwriter"

	"berthy/internal/agentclient"
)

const contextUsage = "usage: berthy context add --project PROJECT -e ENV --url URL --token-env VAR [--config FILE]\n" +
	"       berthy context list [--config FILE] [--json]\n" +
	"       berthy context remove --project PROJECT -e ENV [--config FILE]"

// runContext runs "context add", "context list" and "context remove": they edit the CLI configuration on this machine and
// never contact an agent.
func runContext(args []string, out, errout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errout, contextUsage)
		return 2
	}
	switch args[0] {
	case "add":
		return runContextAdd(args[1:], out, errout)
	case "list":
		return runContextList(args[1:], out, errout)
	case "remove":
		return runContextRemove(args[1:], out, errout)
	default:
		fmt.Fprintln(errout, contextUsage)
		return 2
	}
}

func runContextAdd(args []string, out, errout io.Writer) int {
	fset := flag.NewFlagSet("context add", flag.ContinueOnError)
	fset.SetOutput(errout)
	fset.Usage = func() { fmt.Fprintln(errout, contextUsage) }
	project := fset.String("project", "", "project")
	env := fset.String("e", "", "environment")
	rawURL := fset.String("url", "", "agent URL")
	tokenEnv := fset.String("token-env", "", "environment variable that holds the token")
	cliConfig := fset.String("config", "", "CLI configuration file")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	usage := func(format string, a ...any) int {
		fmt.Fprintf(errout, format+"\n", a...)
		fmt.Fprintln(errout, contextUsage)
		return 2
	}
	if fset.NArg() != 0 || *project == "" || *env == "" || *rawURL == "" || *tokenEnv == "" {
		return usage("context add needs --project, -e, --url and --token-env")
	}
	path, err := agentclient.ConfigPath(*cliConfig)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	c, err := agentclient.ReadConfig(path)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	_, existed := c.Projects[*project].Environments[*env]
	changed, err := c.Put(*project, *env, agentclient.Environment{URL: *rawURL, TokenEnv: *tokenEnv})
	if err != nil {
		return usage("%v", err)
	}
	switch {
	case !changed:
		fmt.Fprintf(out, "%s/%s is already at %s in %s (token in $%s)\n", *project, *env, *rawURL, path, *tokenEnv)
	default:
		if *cliConfig == "" {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				fmt.Fprintln(errout, err)
				return 1
			}
		}
		if err := agentclient.WriteConfig(path, c); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		verb := "added"
		if existed {
			verb = "updated"
		}
		fmt.Fprintf(out, "%s %s/%s at %s to %s (token in $%s)\n", verb, *project, *env, *rawURL, path, *tokenEnv)
	}
	if os.Getenv(*tokenEnv) == "" {
		fmt.Fprintf(errout, "$%s is not set in this shell\n", *tokenEnv)
	}
	return 0
}

func runContextList(args []string, out, errout io.Writer) int {
	fset := flag.NewFlagSet("context list", flag.ContinueOnError)
	fset.SetOutput(errout)
	fset.Usage = func() { fmt.Fprintln(errout, contextUsage) }
	cliConfig := fset.String("config", "", "CLI configuration file")
	asJSON := fset.Bool("json", false, "print JSON")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	if fset.NArg() != 0 {
		fmt.Fprintln(errout, "context list takes no arguments")
		fmt.Fprintln(errout, contextUsage)
		return 2
	}
	path, err := agentclient.ConfigPath(*cliConfig)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	c, err := agentclient.ReadConfig(path)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	type row struct {
		Project     string `json:"project"`
		Environment string `json:"environment"`
		URL         string `json:"url"`
		TokenEnv    string `json:"token_env"`
		TokenSet    bool   `json:"token_set"`
	}
	var rows []row
	projects := make([]string, 0, len(c.Projects))
	for name := range c.Projects {
		projects = append(projects, name)
	}
	slices.Sort(projects)
	for _, project := range projects {
		envs := make([]string, 0, len(c.Projects[project].Environments))
		for name := range c.Projects[project].Environments {
			envs = append(envs, name)
		}
		slices.Sort(envs)
		for _, env := range envs {
			e := c.Projects[project].Environments[env]
			rows = append(rows, row{
				Project: project, Environment: env, URL: e.URL, TokenEnv: e.TokenEnv,
				TokenSet: os.Getenv(e.TokenEnv) != "",
			})
		}
	}
	if len(rows) == 0 {
		return 0
	}
	if *asJSON {
		b, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		fmt.Fprintln(out, string(b))
		return 0
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		set := "no"
		if r.TokenSet {
			set = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", terminalSafe(r.Project), terminalSafe(r.Environment), terminalSafe(r.URL), terminalSafe(r.TokenEnv), set)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	return 0
}

func runContextRemove(args []string, out, errout io.Writer) int {
	fset := flag.NewFlagSet("context remove", flag.ContinueOnError)
	fset.SetOutput(errout)
	fset.Usage = func() { fmt.Fprintln(errout, contextUsage) }
	project := fset.String("project", "", "project")
	env := fset.String("e", "", "environment")
	cliConfig := fset.String("config", "", "CLI configuration file")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	usage := func(format string, a ...any) int {
		fmt.Fprintf(errout, format+"\n", a...)
		fmt.Fprintln(errout, contextUsage)
		return 2
	}
	if fset.NArg() != 0 || *project == "" || *env == "" {
		return usage("context remove needs --project and -e")
	}
	path, err := agentclient.ConfigPath(*cliConfig)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	c, err := agentclient.ReadConfig(path)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	if !c.Remove(*project, *env) {
		fmt.Fprintf(errout, "no environment %s for project %s in the CLI configuration\n", *env, *project)
		return 1
	}
	if err := agentclient.WriteConfig(path, c); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	fmt.Fprintf(out, "removed %s/%s from %s\n", *project, *env, path)
	return 0
}
