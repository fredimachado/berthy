package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"time"

	"berthy/internal/agent"
	"berthy/internal/agentclient"
	"berthy/internal/api"
)

// tokensNow is the clock of the token commands. Tests replace it.
var tokensNow = time.Now

const tokenUsage = "usage: token create NAME [--scope read|deploy|admin] [--agent] [--expires 90d|36h|2027-02-16] [--dry-run] | token list | token revoke NAME [--dry-run]"

var expiresDays = regexp.MustCompile(`^[1-9][0-9]*d$`)

// parseExpires turns the value of --expires into the time the token stops working: a number of days (90d), a Go duration
// (36h), or a date (2027-02-16), which is the start of that day in UTC. The time must be after now.
func parseExpires(s string, now time.Time) (time.Time, error) {
	var at time.Time
	switch {
	case expiresDays.MatchString(s):
		days, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || days > 36500 {
			return time.Time{}, fmt.Errorf("invalid --expires: use 90d, 36h or a date such as 2027-02-16")
		}
		at = now.Add(time.Duration(days) * 24 * time.Hour)
	default:
		if d, err := time.ParseDuration(s); err == nil {
			at = now.Add(d)
		} else if day, err := time.Parse("2006-01-02", s); err == nil {
			at = day
		} else {
			return time.Time{}, fmt.Errorf("invalid --expires: use 90d, 36h or a date such as 2027-02-16")
		}
	}
	if !at.After(now) {
		return time.Time{}, fmt.Errorf("invalid --expires: it must be in the future")
	}
	return at.UTC(), nil
}

// tokenRow is one token as the agent lists it.
type tokenRow struct {
	Name     string     `json:"name"`
	Scope    string     `json:"scope"`
	Agent    bool       `json:"agent"`
	Origin   string     `json:"origin"`
	Created  *time.Time `json:"created"`
	Expires  *time.Time `json:"expires"`
	LastUsed *time.Time `json:"last_used"`
}

// runTokenCommand runs "token create NAME", "token list" and "token revoke NAME".
func runTokenCommand(f commandFlags, base *url.URL, prefix, token string, out, errout io.Writer) int {
	usage := func() int {
		fmt.Fprintln(errout, tokenUsage)
		return 2
	}
	if len(f.pos) == 0 {
		return usage()
	}
	sub, args := f.pos[0], f.pos[1:]
	var method, endpoint string
	var body any
	switch {
	case sub == "list" && len(args) == 0:
		method, endpoint = "GET", "tokens"
	case sub == "revoke" && len(args) == 1 && args[0] != "":
		method, endpoint = "DELETE", "tokens/"+url.PathEscape(args[0])
		body = map[string]any{"dry_run": f.dry}
	case sub == "create" && len(args) == 1:
		if !agent.ValidTokenName(args[0]) {
			fmt.Fprintln(errout, "invalid name: use letters, digits, dots, dashes and underscores")
			return 2
		}
		scope := f.scope
		if scope == "" {
			scope = "read"
		}
		if scope != "read" && scope != "deploy" && scope != "admin" {
			fmt.Fprintln(errout, "invalid scope: use read, deploy or admin")
			return 2
		}
		req := map[string]any{"name": args[0], "scope": scope, "agent": f.agent, "dry_run": f.dry}
		if f.expires != "" {
			at, err := parseExpires(f.expires, tokensNow())
			if err != nil {
				fmt.Fprintln(errout, err)
				return 2
			}
			req["expires"] = at.Format(time.RFC3339)
		}
		method, endpoint, body = "POST", "tokens", req
	default:
		return usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	status, data, ok := askAgent(ctx, agentclient.NewHTTPClient(), agentURL(base, prefix, endpoint), token, method, payload, errout)
	if !ok {
		return 1
	}
	if status != http.StatusOK {
		printRefusal(errout, safeSecretText(agentclient.Message(status, data), token), !f.json)
		return 1
	}
	if f.json {
		return printResponse(out, errout, data, true)
	}
	switch sub {
	case "list":
		return printTokens(out, errout, data)
	case "revoke":
		verb := "revoked"
		if f.dry {
			verb = "would revoke"
		}
		var plan struct {
			Name  string `json:"name"`
			Scope string `json:"scope"`
		}
		if json.Unmarshal(data, &plan) != nil {
			fmt.Fprintln(errout, "invalid agent response")
			return 1
		}
		if f.dry {
			fmt.Fprintf(out, "%s token %s (%s)\n", verb, terminalSafe(plan.Name), terminalSafe(plan.Scope))
		} else {
			fmt.Fprintf(out, "%s token %s\n", verb, terminalSafe(plan.Name))
		}
		return 0
	}
	var made struct {
		Name    string     `json:"name"`
		Scope   string     `json:"scope"`
		Agent   bool       `json:"agent"`
		Expires *time.Time `json:"expires"`
		Token   string     `json:"token"`
	}
	if json.Unmarshal(data, &made) != nil || f.dry == (made.Token != "") {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	facts := []string{terminalSafe(made.Scope)}
	if made.Agent {
		facts = append(facts, "agent")
	}
	if made.Expires != nil {
		facts = append(facts, "expires "+made.Expires.UTC().Format("2006-01-02"))
	}
	if f.dry {
		fmt.Fprintf(out, "would create token %s (%s)\n", terminalSafe(made.Name), strings.Join(facts, ", "))
		return 0
	}
	fmt.Fprintf(out, "token %s (%s), shown once, store it now: %s\n", terminalSafe(made.Name), strings.Join(facts, ", "), terminalSafe(made.Token))
	return 0
}

// printTokens prints the tokens as a table.
func printTokens(out, errout io.Writer, data []byte) int {
	var rows []tokenRow
	if json.Unmarshal(data, &rows) != nil {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	if len(rows) == 0 {
		fmt.Fprintln(out, "no tokens")
		return 0
	}
	now := tokensNow()
	date := func(t *time.Time, none string) string {
		if t == nil {
			return none
		}
		return t.UTC().Format("2006-01-02")
	}
	s := styleFor(out, true)
	var table cliTable
	table.header(s, "NAME", "SCOPE", "AGENT", "ORIGIN", "CREATED", "EXPIRES", "LAST USED")
	for _, r := range rows {
		agentFlag, expires, used := "no", date(r.Expires, "never"), "never"
		if r.Agent {
			agentFlag = "yes"
		}
		if r.Expires != nil && !r.Expires.After(now) {
			expires += " (expired)"
		}
		if r.LastUsed != nil {
			used = api.HumanAge(max(now.Sub(*r.LastUsed), 0)) + " ago"
		}
		table.row(terminalSafe(r.Name), terminalSafe(r.Scope), agentFlag, terminalSafe(r.Origin), s.paint(toneMuted, date(r.Created, "-")), s.paint(toneMuted, expires), s.paint(toneMuted, used))
	}
	if err := table.write(out, s); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	return 0
}
