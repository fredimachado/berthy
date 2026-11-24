package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strings"
	"text/tabwriter"
	"time"

	"berthy/internal/agent"
	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/scope"
	"berthy/internal/setup"
)

// exitHeld is the exit code of a command whose change an AI agent's token asked for on a protected environment: the agent held
// it for a person to approve, so it is not done yet. It is neither success nor failure.
const exitHeld = 3

// approvalsNow is the clock of the approvals table. Tests replace it.
var approvalsNow = time.Now

// runAgentToken runs "agent token --project P -e ENV [--agent] [--name NAME] [--scope read|deploy|admin] [--config FILE]": it
// makes a token, prints it once with its digest and the object that goes into the agent's configuration, and a line that adds
// the environment to another machine's CLI configuration. The line never holds the token.
func runAgentToken(args []string, out, errout io.Writer) int {
	usage := func() int {
		fmt.Fprintln(errout, "usage: agent token --project PROJECT -e ENV [--agent] [--name NAME] [--scope read|deploy|admin] [--config FILE]")
		return 2
	}
	var project, env, name, config string
	tokenScope := "read"
	isAgent := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--agent":
			isAgent = true
		case "-e", "--project", "--name", "--scope", "--config":
			if i+1 == len(args) {
				return usage()
			}
			i++
			switch args[i-1] {
			case "-e":
				env = args[i]
			case "--project":
				project = args[i]
			case "--name":
				name = args[i]
			case "--config":
				config = args[i]
			default:
				tokenScope = args[i]
			}
		default:
			return usage()
		}
	}
	if !scope.ValidProject(project) {
		fmt.Fprintln(errout, "invalid project")
		return 2
	}
	if !scope.ValidEnvironment(env) {
		fmt.Fprintln(errout, "invalid environment")
		return 2
	}
	id := scope.ID{Project: project, Environment: env}
	if err := id.CheckLength(); err != nil {
		fmt.Fprintln(errout, err)
		return 2
	}
	if tokenScope != "read" && tokenScope != "deploy" && tokenScope != "admin" {
		fmt.Fprintln(errout, "invalid scope: use read, deploy or admin")
		return 2
	}
	if name != "" && !agent.ValidTokenName(name) {
		fmt.Fprintln(errout, "invalid name: use letters, digits, dots, dashes and underscores")
		return 2
	}
	urlHint := "http://ADDRESS:PORT"
	if config != "" {
		f, err := agent.LoadFileForCLI(config)
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		urlHint = "http://" + f.Listen
	}
	token, digest, err := agent.NewToken(id)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	entry, err := json.Marshal(agent.Token{Name: name, Digest: digest, Scope: tokenScope, Agent: isAgent})
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	fmt.Fprintf(out, "token: %s\nsha256: %s\n%s\non the machine that will use it: berthy context add --project %s -e %s --url %s --token-env %s\n",
		token, digest, entry, project, env, shellDoubleQuote(urlHint), setup.DefaultTokenEnv(project, env))
	return 0
}

// heldApproval is the answer of an agent that held a change instead of carrying it out.
type heldApproval struct {
	ID      string          `json:"approval_id"`
	Expires time.Time       `json:"expires"`
	Plan    json.RawMessage `json:"plan"`
}

// reportHeld handles the answer to a change when the agent held it for approval: it tells the person what is held and
// returns exitHeld. held is false when the answer is not that, and nothing is printed.
func reportHeld(data []byte, out, errout io.Writer, asJSON bool) (code int, held bool) {
	var h heldApproval
	if json.Unmarshal(data, &h) != nil || h.ID == "" {
		return 0, false
	}
	line := fmt.Sprintf("held for approval: %s (expires %s)", terminalSafe(h.ID), h.Expires.UTC().Format(time.RFC3339))
	if asJSON {
		var pretty bytes.Buffer
		if json.Indent(&pretty, data, "", "  ") != nil {
			fmt.Fprintln(errout, "invalid agent response")
			return 1, true
		}
		fmt.Fprintln(out, pretty.String())
		fmt.Fprintln(errout, line)
		return exitHeld, true
	}
	fmt.Fprintln(out, line)
	printPlan(out, h.Plan)
	return exitHeld, true
}

// printPlan prints a plan under a "plan:" heading.
func printPlan(out io.Writer, plan json.RawMessage) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(plan))
	dec.UseNumber()
	if len(plan) == 0 || dec.Decode(&v) != nil {
		return
	}
	fmt.Fprintln(out, "plan:")
	if m, ok := v.(map[string]any); ok {
		if lines := takeDiffLines(m); lines != nil {
			fmt.Fprintln(out, "  diff:")
			printLines(out, "    ", lines)
		}
	}
	printText(out, v, "  ")
}

// approvalRow is one approval as the agent lists it.
type approvalRow struct {
	ID        string          `json:"id"`
	Actor     string          `json:"actor"`
	Kind      string          `json:"kind"`
	Summary   string          `json:"summary"`
	Plan      json.RawMessage `json:"plan"`
	Created   time.Time       `json:"created"`
	Expires   time.Time       `json:"expires"`
	Status    string          `json:"status"`
	DecidedBy string          `json:"decided_by"`
	Result    string          `json:"result"`
}

// runApprovalCommand runs "approvals", "approve ID" and "reject ID".
func runApprovalCommand(cmd string, f commandFlags, base *url.URL, prefix, token string, out, errout io.Writer) int {
	want := 1
	if cmd == "approvals" {
		want = 0
	}
	if len(f.pos) != want || (cmd != "approvals" && f.pos[0] == "") {
		fmt.Fprintln(errout, "usage: approvals [--all] | approve ID | reject ID [-m REASON]")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	client := agentclient.NewHTTPClient()
	call := func(method, endpoint, query string, body []byte) ([]byte, int, error) {
		u := *base
		u.Path, u.RawQuery = prefix+endpoint, query
		resp, err := agentclient.Send(ctx, client, method, u.String(), token, body)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		if err != nil || len(data) > 1<<20 {
			return nil, 0, fmt.Errorf("invalid agent response")
		}
		return data, resp.StatusCode, nil
	}
	switch cmd {
	case "approvals":
		query := "status=pending"
		if f.all {
			query = ""
		}
		data, status, err := call("GET", "approvals", query, nil)
		if err != nil {
			fmt.Fprintln(errout, "agent connection failed")
			return 1
		}
		if status != http.StatusOK {
			fmt.Fprintln(errout, agentclient.Message(status, data))
			return 1
		}
		return printApprovals(out, errout, data, f.all, f.json)
	case "reject":
		var body []byte
		if f.message != "" {
			body, _ = json.Marshal(map[string]string{"reason": f.message})
		}
		data, status, err := call("POST", "approvals/"+url.PathEscape(f.pos[0])+"/reject", "", body)
		if err != nil {
			fmt.Fprintln(errout, "agent connection failed")
			return 1
		}
		if status != http.StatusOK {
			fmt.Fprintln(errout, agentclient.Message(status, data))
			return 1
		}
		if f.json {
			return printResponse(out, errout, data, true)
		}
		fmt.Fprintf(out, "rejected: %s\n", terminalSafe(f.pos[0]))
		return 0
	}
	return runApprove(ctx, call, client, f, base, prefix, token, out, errout)
}

// runApprove shows what is approved, then approves it and follows what the agent does with it.
func runApprove(ctx context.Context, call func(method, endpoint, query string, body []byte) ([]byte, int, error), client *http.Client, f commandFlags, base *url.URL, prefix, token string, out, errout io.Writer) int {
	id := f.pos[0]
	kind := ""
	if data, status, err := call("GET", "approvals", "", nil); err == nil && status == http.StatusOK {
		var rows []approvalRow
		if json.Unmarshal(data, &rows) == nil {
			for _, r := range rows {
				if r.ID != id {
					continue
				}
				kind = r.Kind
				if !f.json {
					fmt.Fprintf(out, "approval: %s\nsummary: %s\nrequested by: %s\n", terminalSafe(r.ID), terminalSafe(r.Summary), terminalSafe(r.Actor))
					printPlan(out, r.Plan)
				}
			}
		}
	}
	progress := newProgressLine(out, !f.json && stderrTerminal(out))
	if progress.enabled {
		progress.Start("approving")
	}
	data, status, err := call("POST", "approvals/"+url.PathEscape(id)+"/approve", "", nil)
	progress.Close()
	if err != nil {
		fmt.Fprintln(errout, "agent connection failed")
		return 1
	}
	var accepted struct {
		ID     string `json:"operation_id"`
		Events string `json:"events_url"`
	}
	if status == http.StatusAccepted && json.Unmarshal(data, &accepted) == nil && accepted.ID != "" {
		expected := prefix + "operations/" + url.PathEscape(accepted.ID) + "/events"
		if accepted.Events != expected || strings.HasPrefix(accepted.Events, "//") || path.Clean(accepted.Events) != accepted.Events {
			fmt.Fprintln(errout, "invalid operation events URL")
			return 1
		}
		cmd, _, _ := strings.Cut(kind, " ")
		return watchOperation(ctx, client, base, token, prefix, accepted.ID, accepted.Events, out, errout, f.json, cmd, f.env, f.project, operationWatchOptions{config: f.config})
	}
	if status != http.StatusOK && status != http.StatusAccepted {
		fmt.Fprintln(errout, safeSecretText(agentclient.Message(status, data), token))
		return 1
	}
	return printResponse(out, errout, data, f.json)
}

// printApprovals prints the approvals as a table, or as indented JSON. all adds the status and decider columns.
func printApprovals(out, errout io.Writer, data []byte, all, asJSON bool) int {
	if asJSON {
		return printResponse(out, errout, data, true)
	}
	var rows []approvalRow
	if json.Unmarshal(data, &rows) != nil {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	if len(rows) == 0 {
		if all {
			fmt.Fprintln(out, "no approvals")
		} else {
			fmt.Fprintln(out, "no pending approvals")
		}
		return 0
	}
	now := approvalsNow()
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if all {
		fmt.Fprintln(tw, "ID\tAGE\tEXPIRES IN\tACTOR\tSTATUS\tDECIDED BY\tSUMMARY")
	} else {
		fmt.Fprintln(tw, "ID\tAGE\tEXPIRES IN\tACTOR\tSUMMARY")
	}
	for _, r := range rows {
		expires := "-"
		if r.Status == "pending" {
			expires = api.HumanAge(max(r.Expires.Sub(now), 0))
		}
		age := api.HumanAge(max(now.Sub(r.Created), 0))
		if all {
			decided := "-"
			if r.DecidedBy != "" {
				decided = terminalSafe(r.DecidedBy)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", terminalSafe(r.ID), age, expires, terminalSafe(r.Actor), terminalSafe(r.Status), decided, terminalSafe(r.Summary))
		} else {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", terminalSafe(r.ID), age, expires, terminalSafe(r.Actor), terminalSafe(r.Summary))
		}
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	for _, r := range rows {
		var plan map[string]any
		if json.Unmarshal(r.Plan, &plan) != nil {
			continue
		}
		app, _ := plan["app"].(string)
		if lines := takeDiffLines(plan); lines != nil {
			fmt.Fprintf(out, "\n%s: changes of %s\n", terminalSafe(r.ID), terminalSafe(app))
			printLines(out, "  ", lines)
		}
	}
	return 0
}
