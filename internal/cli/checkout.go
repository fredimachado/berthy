package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"berthy/internal/agentclient"
	"berthy/internal/gitcheck"
)

// warnCheckout tells the operator, before a deploy is sent, what the checkout says: which files have local changes, and that
// --commit names another commit than HEAD. given is the --commit the operator gave, "" for none.
func warnCheckout(errout io.Writer, given string, co gitcheck.Checkout) {
	if len(co.Changed) > 0 {
		fmt.Fprintf(errout, "warning: the manifest has local changes: %s\n", terminalSafe(strings.Join(co.Changed, ", ")))
	}
	if given != "" && co.Head != "" && !strings.HasPrefix(co.Head, given) {
		fmt.Fprintf(errout, "warning: --commit %s is not HEAD (%s)\n", terminalSafe(given), co.Head)
	}
}

// refuseLocalChanges is the error of a deploy from a checkout with local changes to a protected environment, or nil. The
// environment is protected by its manifest or by its agent, and the error names the files, which the agent does not know.
func refuseLocalChanges(ctx context.Context, client *http.Client, target *agentclient.Target, manifestProtected bool, co gitcheck.Checkout) error {
	if len(co.Changed) == 0 || !manifestProtected && !agentclient.AgentProtected(ctx, client, target) {
		return nil
	}
	return fmt.Errorf("the manifest has local changes: commit them first: %s", terminalSafe(strings.Join(co.Changed, ", ")))
}

// printHistory prints the history of an app as text. The commit of a release from local changes shows that, as in
// "abc1234 (modified)"; the JSON output keeps the flag as its own key.
func printHistory(out, errout io.Writer, b []byte) int {
	var v any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	markModifiedRows(v)
	shape := shapeGeneric
	if _, ok := v.([]any); ok {
		shape = shapeHistory
	}
	printStyledText(out, v, "", styleFor(out, true), shape)
	return 0
}

// markModifiedRows writes the local-changes flag of each history row into its commit, for the text output.
func markModifiedRows(history any) {
	rows, _ := history.([]any)
	for _, r := range rows {
		row, ok := r.(map[string]any)
		if !ok || row["modified"] != true {
			continue
		}
		delete(row, "modified")
		commit, _ := row["commit"].(string)
		row["commit"] = strings.TrimSpace(commit + " (modified)")
	}
}
