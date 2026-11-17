package tui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/logs"
	"berthy/internal/metrics"

	tea "charm.land/bubbletea/v2"
)

// The data the views show, as the agent API sends it.

type agentInfo struct {
	Environment string `json:"environment"`
	Hostname    string `json:"hostname"`
	Protected   bool   `json:"protected"`
}

type lockInfo struct {
	Locked   bool      `json:"locked"`
	Holder   string    `json:"holder"`
	Reason   string    `json:"reason"`
	Acquired time.Time `json:"acquired"`
}

// historyRow is one line of an app's deploy history: a release that was deployed, or a deploy that failed with its reason.
type historyRow struct {
	ID          int64     `json:"id"`
	Tag         string    `json:"tag"`
	Digest      string    `json:"digest"`
	Commit      string    `json:"commit"`
	Created     time.Time `json:"created"`
	OperationID string    `json:"operation_id"`
	Kind        string    `json:"kind"`
	State       string    `json:"state"`
	Reason      string    `json:"reason"`
	// Modified is set when the deploy came from a checkout with local changes, so Commit does not hold what was deployed.
	Modified bool `json:"modified"`
}

// failed is true for a deploy that did not end well.
func (h historyRow) failed() bool {
	switch h.State {
	case "", "succeeded", "completed", "done":
		return h.Reason != ""
	}
	return true
}

// modifiedText marks the commit of a deploy that came from local changes.
const modifiedText = "(modified)"

// commitText is the commit of the row clipped to n characters, with modifiedText after it when the deploy came from local
// changes. A row without a commit shows a dash.
func (h historyRow) commitText(n int) string {
	text := sanitize(clip(orDash(h.Commit), n))
	if h.Modified {
		text += " " + modifiedText
	}
	return text
}

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

type dfInfo struct {
	Host         []metrics.Mount `json:"host"`
	HostStatus   string          `json:"host_status"`
	DockerStatus string          `json:"docker_status"`
	Docker       struct {
		LayersSize int64 `json:"LayersSize"`
		Containers []struct {
			SizeRw int64 `json:"SizeRw"`
		} `json:"Containers"`
		Volumes []struct {
			UsageData struct {
				Size int64 `json:"Size"`
			} `json:"UsageData"`
		} `json:"Volumes"`
		BuildCache []struct {
			Size int64 `json:"Size"`
		} `json:"BuildCache"`
	} `json:"docker"`
}

// Messages. Every fetch ends in one of these, or in an errMsg.

type (
	infoMsg      agentInfo
	appsMsg      struct{ apps []api.App }
	lockMsg      lockInfo
	pendingMsg   struct{ n int }
	approvalsMsg struct{ rows []approvalRow }
	historyMsg   struct {
		app  string
		rows []historyRow
	}
	dfMsg        dfInfo
	statsSeedMsg struct{ samples []metrics.Sample }
	// errMsg is a failed fetch. network is true when the agent could not be reached at all.
	errMsg struct {
		src     string
		text    string
		network bool
	}
	tickMsg time.Time
	// logMsg and statsMsg are what the streams deliver; streamStateMsg is a change of a stream's state. gen tells which run of
	// the stream sent it, so what a cancelled stream still delivers is ignored.
	logMsg struct {
		gen int
		rec logs.Record
	}
	statsMsg struct {
		gen    int
		sample metrics.Sample
	}
	streamStateMsg struct {
		gen   int
		kind  string // "logs" or "stats"
		state streamState
		text  string
		retry time.Duration
	}
	// batchMsg carries what a stream delivered together. closed is true when the stream has ended for good.
	batchMsg struct {
		kind   string // "logs" or "stats"
		gen    int
		ch     <-chan tea.Msg
		msgs   []tea.Msg
		closed bool
	}
)

type streamState int

const (
	streamConnecting streamState = iota
	streamLive
	streamRetrying
)

// apiError is an answer of the agent that is not a success.
type apiError struct {
	status int
	text   string
}

func (e *apiError) Error() string { return e.text }

// client is the interface's access to the agent: the CLI's configuration and token, the shared HTTP client and the requests the
// views make. It never shows the token: every text it makes passes through the target's redaction.
type client struct {
	target *agentclient.Target
	http   *http.Client
	stream *http.Client
	ctx    context.Context
}

func newClient(ctx context.Context, target *agentclient.Target) *client {
	return &client{target: target, http: agentclient.NewHTTPClient(), stream: agentclient.WithTimeout(agentclient.NewHTTPClient(), 0), ctx: ctx}
}

// get reads one answer of the agent. A refusal is an *apiError with the agent's printable message.
func (c *client) get(path, query string) ([]byte, error) {
	data, status, err := agentclient.Do(c.ctx, c.http, c.target.Base, c.target.Prefix, c.target.Token, "GET", path, query, nil)
	if err != nil {
		if errors.Is(err, agentclient.ErrInvalidResponse) {
			return nil, &apiError{text: "invalid agent response"}
		}
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &apiError{status: status, text: c.target.Redact(agentclient.Message(status, data))}
	}
	return data, nil
}

// fail turns an error of a fetch into the message of the status line.
func (c *client) fail(src string, err error) tea.Msg {
	var refused *apiError
	if errors.As(err, &refused) {
		return errMsg{src: src, text: refused.text}
	}
	if c.ctx.Err() != nil {
		return errMsg{src: src, text: "stopped"}
	}
	return errMsg{src: src, text: "agent unreachable", network: true}
}

// fetch runs one request off the interface's loop and decodes its answer with decode, which returns the message to send.
func (c *client) fetch(src, path, query string, decode func([]byte) (tea.Msg, error)) tea.Cmd {
	return func() tea.Msg {
		data, err := c.get(path, query)
		if err != nil {
			return c.fail(src, err)
		}
		msg, err := decode(data)
		if err != nil {
			return errMsg{src: src, text: "invalid agent response"}
		}
		return msg
	}
}

func decodeJSON[T any](data []byte) (T, error) {
	var v T
	err := json.Unmarshal(data, &v)
	return v, err
}

func (c *client) fetchInfo() tea.Cmd {
	return c.fetch("info", "agent", "", func(b []byte) (tea.Msg, error) {
		v, err := decodeJSON[agentInfo](b)
		return infoMsg(v), err
	})
}

func (c *client) fetchApps() tea.Cmd {
	return c.fetch("apps", "apps", "", func(b []byte) (tea.Msg, error) {
		v, err := decodeJSON[[]api.App](b)
		return appsMsg{apps: v}, err
	})
}

func (c *client) fetchLock() tea.Cmd {
	return c.fetch("lock", "lock", "", func(b []byte) (tea.Msg, error) {
		v, err := decodeJSON[lockInfo](b)
		return lockMsg(v), err
	})
}

func (c *client) fetchPending() tea.Cmd {
	return c.fetch("pending", "approvals", "status=pending", func(b []byte) (tea.Msg, error) {
		v, err := decodeJSON[[]json.RawMessage](b)
		return pendingMsg{n: len(v)}, err
	})
}

func (c *client) fetchApprovals() tea.Cmd {
	return c.fetch("approvals", "approvals", "", func(b []byte) (tea.Msg, error) {
		v, err := decodeJSON[[]approvalRow](b)
		return approvalsMsg{rows: v}, err
	})
}

func (c *client) fetchHistory(app string) tea.Cmd {
	return c.fetch("history", "history/"+url.PathEscape(app), "", func(b []byte) (tea.Msg, error) {
		v, err := decodeJSON[[]historyRow](b)
		sort.SliceStable(v, func(i, j int) bool { return v[i].Created.After(v[j].Created) })
		return historyMsg{app: app, rows: v}, err
	})
}

func (c *client) fetchDF() tea.Cmd {
	return c.fetch("df", "df", "", func(b []byte) (tea.Msg, error) {
		v, err := decodeJSON[dfInfo](b)
		return dfMsg(v), err
	})
}

// fetchStatsSeed reads the last ten minutes of samples, which seed the CPU sparkline.
func (c *client) fetchStatsSeed() tea.Cmd {
	return c.fetch("stats", "stats", "since=10m", func(b []byte) (tea.Msg, error) {
		v, err := decodeJSON[[]metrics.Sample](b)
		return statsSeedMsg{samples: v}, err
	})
}

// listen waits for what a stream delivers, together with everything else that is already waiting. It is the usual Bubble Tea
// way to turn a channel into messages: the update that handles the result calls listen again.
func listen(ch <-chan tea.Msg, kind string, gen int) tea.Cmd {
	return func() tea.Msg {
		batch := batchMsg{kind: kind, gen: gen, ch: ch}
		first, ok := <-ch
		if !ok {
			batch.closed = true
			return batch
		}
		batch.msgs = append(batch.msgs, first)
		for len(batch.msgs) < 256 {
			select {
			case next, ok := <-ch:
				if !ok {
					batch.closed = true
					return batch
				}
				batch.msgs = append(batch.msgs, next)
			default:
				return batch
			}
		}
		return batch
	}
}

// backoff is the wait before retry number n (1 is the first): 1s, 2s, 4s, 8s, then 15s.
func backoff(n int) time.Duration {
	d := time.Second << min(max(n-1, 0), 4)
	return min(d, 15*time.Second)
}

// follow reads a stream of events from the agent until ctx ends, and reconnects with a growing wait when it drops. query is
// asked for on each connection, so a reconnection can ask for what it missed. on handles one event.
func (c *client) follow(ctx context.Context, kind string, gen int, path string, query func() string, on func(event, data string) error, send func(tea.Msg)) {
	state := func(s streamState, text string, retry time.Duration) {
		send(streamStateMsg{gen: gen, kind: kind, state: s, text: text, retry: retry})
	}
	failures := 0
	for ctx.Err() == nil {
		state(streamConnecting, "", 0)
		live := false
		text := c.readStream(ctx, path, query(), func(event, data string) error {
			if !live {
				live = true
				failures = 0
				state(streamLive, "", 0)
			}
			return on(event, data)
		})
		if ctx.Err() != nil {
			return
		}
		failures++
		wait := backoff(failures)
		state(streamRetrying, c.target.Redact(text), wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// readStream opens the stream once and reads it to its end. It returns the reason it ended, for the status line.
func (c *client) readStream(ctx context.Context, path, query string, on func(event, data string) error) string {
	u := *c.target.Base
	u.Path, u.RawQuery = c.target.Prefix+path, query
	resp, err := agentclient.Send(ctx, c.stream, "GET", u.String(), c.target.Token, nil)
	if err != nil {
		return "agent unreachable"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return agentclient.Message(resp.StatusCode, body)
	}
	err = agentclient.ReadEvents(ctx, resp.Body, on)
	switch {
	case err == nil:
		return "stream finished"
	case errors.Is(err, agentclient.ErrInvalidEvent):
		return "invalid stream event"
	case ctx.Err() != nil:
		return ""
	}
	return "stream interrupted"
}

// streamLogs follows the logs of an app: the last 200 lines, then what comes. After a reconnection it asks for the lines since
// the last one it has.
func (c *client) streamLogs(ctx context.Context, app string, gen int, send func(tea.Msg)) {
	filter := newReplayFilter()
	// query is asked for on each connection. A reconnection goes back a little before its newest line, and the filter keeps what
	// it replays from showing twice.
	query := func() string {
		q := url.Values{"follow": {"1"}, "tail": {"200"}}
		if floor := filter.reconnect(); !floor.IsZero() {
			q.Set("tail", "1000")
			q.Set("since", floor.UTC().Format(time.RFC3339Nano))
		}
		return q.Encode()
	}
	c.follow(ctx, "logs", gen, "apps/"+url.PathEscape(app)+"/logs", query, func(event, data string) error {
		switch event {
		case "log":
			var rec logs.Record
			if json.Unmarshal([]byte(data), &rec) != nil {
				return agentclient.ErrInvalidEvent
			}
			if filter.accept(rec) {
				send(logMsg{gen: gen, rec: rec})
			}
		case "done":
			return agentclient.ErrStopEvents
		case "error":
			return errors.New("agent stream failed")
		}
		return nil
	}, send)
}

// streamStats follows the host and container samples the agent takes.
func (c *client) streamStats(ctx context.Context, gen int, send func(tea.Msg)) {
	c.follow(ctx, "stats", gen, "stats", func() string { return "watch=1" }, func(event, data string) error {
		switch event {
		case "stats":
			var s metrics.Sample
			if json.Unmarshal([]byte(data), &s) != nil {
				return agentclient.ErrInvalidEvent
			}
			send(statsMsg{gen: gen, sample: s})
		case "done":
			return agentclient.ErrStopEvents
		case "error":
			return errors.New("agent stream failed")
		}
		return nil
	}, send)
}

// channelSender returns a send function that puts messages on ch, and gives up when ctx ends.
func channelSender(ctx context.Context, ch chan<- tea.Msg) func(tea.Msg) {
	return func(m tea.Msg) {
		select {
		case ch <- m:
		case <-ctx.Done():
		}
	}
}
