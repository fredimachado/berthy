package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"text/tabwriter"

	"berthy/internal/agentclient"
	"berthy/internal/credstore"
)

const contextUsage = "usage: berthy context add --project PROJECT -e ENV --url URL --token-env VAR [--save-token] [--config FILE]\n" +
	"       berthy context list [--config FILE] [--json]\n" +
	"       berthy context remove --project PROJECT -e ENV [--config FILE] [--delete-token]"

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
	saveToken := fset.Bool("save-token", false, "save the token in the credential store")
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
	entry := agentclient.Environment{URL: *rawURL, TokenEnv: *tokenEnv}
	_, existed := c.Projects[*project].Environments[*env]
	changed, err := c.Put(*project, *env, entry)
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
	stored := false
	if *saveToken {
		if code := saveContextToken(*project, *env, entry, out, errout); code != 0 {
			return code
		}
		stored = true
	}
	if !stored && os.Getenv(*tokenEnv) == "" {
		fmt.Fprintf(errout, "$%s is not set in this shell\n", *tokenEnv)
	}
	return 0
}

// parseToken returns the token of what was typed or piped: one line. One trailing LF or CRLF is dropped (PowerShell ends
// a piped string with CRLF). Anything else that holds a CR or LF, and an empty token, is refused.
func parseToken(data []byte) (string, error) {
	switch {
	case len(data) >= 2 && data[len(data)-2] == '\r' && data[len(data)-1] == '\n':
		data = data[:len(data)-2]
	case len(data) >= 1 && data[len(data)-1] == '\n':
		data = data[:len(data)-1]
	}
	if len(data) == 0 {
		return "", errors.New("no token entered")
	}
	for _, b := range data {
		if b == '\n' || b == '\r' {
			return "", errors.New("the token must be one line")
		}
	}
	return string(data), nil
}

// saveContextToken reads a token from the terminal or standard input and stores it under the entry's key. The configuration
// entry is already written; a refusal or store failure leaves it and returns a non-zero exit.
func saveContextToken(project, env string, entry agentclient.Environment, out, errout io.Writer) int {
	read, interactive := hiddenInput()
	var data []byte
	var err error
	if interactive {
		fmt.Fprintf(errout, "Token of %s/%s (hidden): ", project, env)
		data, err = read()
		fmt.Fprintln(errout)
	} else {
		data, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		fmt.Fprintln(errout, "no token entered")
		return 1
	}
	token, err := parseToken(data)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	store := agentclient.Tokens(context.Background())
	if err := store.Set(agentclient.StoreKey(project, env, entry), token); err != nil {
		if errors.Is(err, credstore.ErrGivenUp) {
			fmt.Fprintf(errout, "could not confirm the save in the %s; set $%s instead\n", store.Name(), entry.TokenEnv)
		} else {
			fmt.Fprintf(errout, "not saved: no credential store on this machine; set $%s instead\n", entry.TokenEnv)
		}
		return 1
	}
	fmt.Fprintf(out, "saved the token of %s/%s in the %s\n", project, env, store.Name())
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
		Project      string `json:"project"`
		Environment  string `json:"environment"`
		URL          string `json:"url"`
		TokenEnv     string `json:"token_env"`
		TokenSet     bool   `json:"token_set"`
		TokenStored  bool   `json:"token_stored"`
		storedStatus string
	}
	var rows []row
	store := agentclient.Tokens(context.Background())
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
			status := "unknown"
			stored := false
			if agentclient.StoreUsable(project, env, e) {
				_, storeErr := store.Get(agentclient.StoreKey(project, env, e))
				if storeErr == nil {
					status = "stored"
					stored = true
				} else if errors.Is(storeErr, credstore.ErrNotFound) {
					status = "not stored"
				}
			}
			rows = append(rows, row{
				TokenStored: stored, storedStatus: status,
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", terminalSafe(r.Project), terminalSafe(r.Environment), terminalSafe(r.URL), terminalSafe(r.TokenEnv), set, r.storedStatus)
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
	deleteToken := fset.Bool("delete-token", false, "delete the stored token")
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
	entry, exists := c.Projects[*project].Environments[*env]
	if !exists {
		fmt.Fprintf(errout, "no environment %s for project %s in the CLI configuration\n", *env, *project)
		return 1
	}
	if *deleteToken {
		if !agentclient.StoreUsable(*project, *env, entry) {
			fmt.Fprintln(errout, "unknown environment or missing token_env")
			return 1
		}
		store := agentclient.Tokens(context.Background())
		removed, err := store.Delete(agentclient.StoreKey(*project, *env, entry))
		if err != nil {
			message := "the stored token was not deleted"
			if errors.Is(err, credstore.ErrGivenUp) {
				message = "the stored token could not be confirmed deleted"
			}
			fmt.Fprintf(errout, "%s (%v): the entry is kept, so the command can be run again\n", message, err)
			return 1
		}
		if removed {
			fmt.Fprintf(out, "deleted the token of %s/%s from the %s\n", *project, *env, store.Name())
		}
	}
	c.Remove(*project, *env)
	if err := agentclient.WriteConfig(path, c); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	fmt.Fprintf(out, "removed %s/%s from %s\n", *project, *env, path)
	return 0
}
