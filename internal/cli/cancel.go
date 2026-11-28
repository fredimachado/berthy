package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"berthy/internal/agentclient"
)

// cancelPoll is how often "cancel" asks whether the operation it cancelled has ended, and cancelWait how long it waits: taking a
// rollout back can take as long as the new version needed to start.
var (
	cancelPoll = 500 * time.Millisecond
	cancelWait = 5 * time.Minute
)

// reportCancel prints the agent's answer to a cancel. A dry run says whether the cancel would be accepted, and why not. An
// accepted cancel is followed to the end of the operation, which stops and ends as failed or rolled_back, so the command returns
// when the lock is free. It returns 0 when the cancel was accepted (or would be) and the operation ended as the cancel says.
func reportCancel(ctx context.Context, client *http.Client, base *url.URL, prefix, token string, data []byte, f commandFlags, out, errout io.Writer) int {
	var plan struct {
		ID       string `json:"operation_id"`
		Kind     string `json:"kind"`
		App      string `json:"app"`
		Accepted bool   `json:"accepted"`
		Reason   string `json:"reason"`
	}
	if json.Unmarshal(data, &plan) != nil || plan.ID == "" {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	what := terminalSafe(plan.Kind)
	if plan.App != "" {
		what += " " + terminalSafe(plan.App)
	}
	switch {
	case f.dry && plan.Accepted:
		fmt.Fprintf(out, "cancel of %s would be accepted: the operation stops and is taken back\n", what)
		return 0
	case f.dry:
		fmt.Fprintf(out, "cancel of %s would be refused: %s\n", what, terminalSafe(plan.Reason))
		return 1
	}
	fmt.Fprintf(out, "cancel accepted for %s\n", what)
	progress := startWait(errout, !f.json, "waiting for cancellation")
	defer progress.Close()
	call := agentclient.Caller(ctx, client, base, prefix, token)
	deadline := time.Now().Add(cancelWait)
	for {
		raw, status, err := call("GET", "operations/"+url.PathEscape(plan.ID), "")
		var op struct {
			State  string `json:"state"`
			Reason string `json:"reason"`
			Error  string `json:"error"`
		}
		if err == nil && status == http.StatusOK && json.Unmarshal(raw, &op) == nil && op.State != "" && op.State != "running" {
			progress.Close()
			line := fmt.Sprintf("operation %s ended: %s", terminalSafe(plan.ID), terminalSafe(op.State))
			if op.Reason != "" {
				line += " (" + terminalSafe(op.Reason) + ")"
			}
			fmt.Fprintln(out, line)
			if op.State == "failed" || op.State == "rolled_back" {
				return 0
			}
			return 1
		}
		if time.Now().After(deadline) {
			progress.Close()
			fmt.Fprintf(errout, "operation %s has not ended yet; see berthy lock status\n", terminalSafe(plan.ID))
			return 1
		}
		select {
		case <-time.After(cancelPoll):
		case <-ctx.Done():
			progress.Close()
			fmt.Fprintf(errout, "stopped waiting; operation %s goes on ending on the agent\n", terminalSafe(plan.ID))
			return 1
		}
	}
}

// runJobCancel runs "job cancel NAME": it asks the agent to stop the run of the job that is in progress. A job has at most one run
// at a time, so its name says which.
func runJobCancel(ctx context.Context, f commandFlags, base *url.URL, prefix, token string, out, errout io.Writer) int {
	result, err := agentclient.DoResult(ctx, agentclient.NewHTTPClient(), base, prefix, token, "POST", "jobs/"+url.PathEscape(f.pos[1])+"/cancel", "", nil)
	data, status := result.Data, result.Status
	if err != nil {
		printAgentRequestError(out, errout, agentclient.Target{Base: base, Token: token}, err, f.json)
		return 1
	}
	if status == http.StatusAccepted {
		if code, held := reportHeldMarkers(data, out, errout, f.json, nil, token); held {
			return code
		}
	}
	if status != http.StatusOK {
		printAgentRefusal(errout, safeSecretText(agentclient.Message(status, data), token), !f.json, result.Older)
		return 1
	}
	if f.json {
		return printResponse(out, errout, data, true)
	}
	fmt.Fprintf(out, "cancelled the run of job %s\n", terminalSafe(f.pos[1]))
	return 0
}
