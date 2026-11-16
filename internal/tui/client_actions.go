package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	pathpkg "path"
	"sort"
	"strings"
	"time"

	"berthy/internal/agentclient"

	tea "charm.land/bubbletea/v2"
)

// The requests of the actions: the changes the person confirms in the overlay, and the reads they need.

// post sends a change to the agent and reads the answer with its headers, at most 1 MiB of it. A refusal is not an error here: the
// caller reads the status. An error is an agent that could not be reached or an answer that cannot be used.
func (c *client) post(path string, body any) (data []byte, status int, header http.Header, err error) {
	var payload []byte
	if body != nil {
		if payload, err = json.Marshal(body); err != nil {
			return nil, 0, nil, err
		}
	}
	u := *c.target.Base
	u.Path, u.RawQuery = c.target.Prefix+path, ""
	resp, err := agentclient.Send(c.ctx, c.http, "POST", u.String(), c.target.Token, payload)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, 0, nil, agentclient.ErrInvalidResponse
	}
	return data, resp.StatusCode, resp.Header, nil
}

// refusal is the printable text of an answer that is not a success, with the token removed.
func (c *client) refusal(status int, data []byte) string {
	return c.target.Redact(agentclient.Message(status, data))
}

// actionFail turns an error of a request of an action into its message: a lost agent says so.
func (c *client) actionFail(id int, err error) tea.Msg {
	if errors.Is(err, agentclient.ErrInvalidResponse) {
		return actionErrMsg{id: id, text: "invalid agent response"}
	}
	if c.ctx.Err() != nil {
		return actionErrMsg{id: id, text: "stopped"}
	}
	return actionErrMsg{id: id, text: "the agent cannot be reached", network: true}
}

// readFail turns the error of a read into the message of an action.
func (c *client) readFail(id int, err error) tea.Msg {
	var refused *apiError
	if errors.As(err, &refused) {
		return actionErrMsg{id: id, status: refused.status, text: refused.text}
	}
	return c.actionFail(id, err)
}

// dryRun asks the agent for the plan of a change: the same request with dry_run set.
func (c *client) dryRun(id int, path string, body map[string]any) tea.Cmd {
	return func() tea.Msg {
		req := map[string]any{"dry_run": true}
		for k, v := range body {
			req[k] = v
		}
		data, status, _, err := c.post(path, req)
		if err != nil {
			return c.actionFail(id, err)
		}
		if status != http.StatusOK {
			return actionErrMsg{id: id, status: status, text: c.refusal(status, data)}
		}
		return planMsg{id: id, plan: json.RawMessage(data)}
	}
}

// fetchApprovalRows reads the approvals again, for an action on one of them.
func (c *client) fetchApprovalRows(id int) tea.Cmd {
	return func() tea.Msg {
		data, err := c.get("approvals", "")
		if err != nil {
			return c.readFail(id, err)
		}
		rows, err := decodeJSON[[]approvalRow](data)
		if err != nil {
			return actionErrMsg{id: id, text: "invalid agent response"}
		}
		return approvalRowsMsg{id: id, rows: rows}
	}
}

// fetchReleases reads the deploy history of an app, for the choice of a release to roll back to.
func (c *client) fetchReleases(id int, app string) tea.Cmd {
	return func() tea.Msg {
		data, err := c.get("history/"+url.PathEscape(app), "")
		if err != nil {
			return c.readFail(id, err)
		}
		rows, err := decodeJSON[[]historyRow](data)
		if err != nil {
			return actionErrMsg{id: id, text: "invalid agent response"}
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Created.After(rows[j].Created) })
		return releasesMsg{id: id, rows: rows}
	}
}

// send makes the change and tells what the agent did with it: it started an operation, held the change for approval, answered
// with a result, or refused.
func (c *client) send(id int, path string, body any) tea.Cmd {
	return func() tea.Msg {
		data, status, header, err := c.post(path, body)
		if err != nil {
			return c.actionFail(id, err)
		}
		switch {
		case status == http.StatusAccepted && (header.Get("Berthy-Approval") == "pending" || bytes.Contains(data, []byte(`"approval_id"`))):
			var held struct {
				ID      string    `json:"approval_id"`
				Expires time.Time `json:"expires"`
			}
			if json.Unmarshal(data, &held) != nil || held.ID == "" {
				return actionErrMsg{id: id, text: "invalid agent response"}
			}
			return sentMsg{id: id, held: held.ID, expires: held.Expires}
		case status == http.StatusAccepted:
			var accepted struct {
				ID     string `json:"operation_id"`
				Events string `json:"events_url"`
			}
			if json.Unmarshal(data, &accepted) != nil || accepted.ID == "" {
				return actionErrMsg{id: id, text: "invalid operation response"}
			}
			expected := c.target.Prefix + "operations/" + url.PathEscape(accepted.ID) + "/events"
			if accepted.Events != expected || strings.HasPrefix(accepted.Events, "//") || pathpkg.Clean(accepted.Events) != accepted.Events {
				return actionErrMsg{id: id, text: "invalid operation events URL"}
			}
			return sentMsg{id: id, op: accepted.ID, events: accepted.Events}
		case status == http.StatusOK:
			return sentMsg{id: id, done: true, body: data}
		}
		return actionErrMsg{id: id, status: status, text: c.refusal(status, data)}
	}
}

// followOperation follows an operation until it ends and sends what happens: opEventMsg for each event and opDoneMsg at the
// end. It reads the event stream and reconnects when it drops, skipping the events it has already shown; when a stream ends
// without the operation's last event it asks for the operation's state, as the CLI does.
func (c *client) followOperation(ctx context.Context, id int, opID, eventsURL string, send func(tea.Msg)) {
	seen, failures := 0, 0
	for ctx.Err() == nil {
		finished := false
		u := *c.target.Base
		u.Path, u.RawQuery = eventsURL, ""
		streamCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		resp, err := agentclient.Send(streamCtx, c.stream, "GET", u.String(), c.target.Token, nil)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				n := 0
				_ = agentclient.ReadEvents(streamCtx, resp.Body, func(event, data string) error {
					n++
					if n <= seen {
						return nil
					}
					seen = n
					if event == "terminal" {
						finished = true
						send(c.doneMsg(id, data))
						return agentclient.ErrStopEvents
					}
					send(opEventMsg{id: id, text: c.target.Redact(eventText(event, data))})
					return nil
				})
			}
			resp.Body.Close()
		}
		cancel()
		if finished {
			return
		}
		data, status, e := agentclient.Do(ctx, c.http, c.target.Base, c.target.Prefix, c.target.Token, "GET", "operations/"+url.PathEscape(opID), "", nil)
		if e == nil && status == http.StatusOK {
			failures = 0
			var op struct {
				State string `json:"state"`
			}
			if json.Unmarshal(data, &op) == nil && op.State != "" && op.State != "running" {
				send(c.doneMsg(id, `{"operation":`+string(data)+`}`))
				return
			}
		} else if failures++; failures >= 3 {
			send(actionErrMsg{id: id, text: "the operation's state is unavailable; it continues on the agent", lost: true})
			return
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
		}
	}
}

// doneMsg reads the end of an operation from the terminal event, or from the operation itself.
func (c *client) doneMsg(id int, data string) tea.Msg {
	var v struct {
		Status    string `json:"status"`
		Operation struct {
			State  string `json:"state"`
			Reason string `json:"reason"`
			Error  string `json:"error"`
		} `json:"operation"`
	}
	_ = json.Unmarshal([]byte(data), &v)
	state := v.Operation.State
	if state == "" {
		state = v.Status
	}
	reason := v.Operation.Reason
	if reason == "" {
		reason = v.Operation.Error
	}
	return opDoneMsg{id: id, state: sanitize(state), reason: c.target.Redact(sanitize(reason))}
}

// eventText is one line for an event of an operation: its name, and the step or reason it carries.
func eventText(event, data string) string {
	var v map[string]any
	if json.Unmarshal([]byte(data), &v) != nil {
		return sanitize(event)
	}
	for _, k := range []string{"step", "message", "reason", "detail"} {
		if s, ok := v[k].(string); ok && s != "" {
			return sanitize(s)
		}
	}
	text := sanitize(strings.ReplaceAll(event, "_", " "))
	for _, k := range []string{"role", "name", "state"} {
		if s, ok := v[k].(string); ok && s != "" {
			text += " " + sanitize(s)
		}
	}
	return text
}
