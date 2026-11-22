package tui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/logs"

	tea "charm.land/bubbletea/v2"
)

const testToken = "bty.prod.0123456789abcdef"

// agentServer is a fake agent that checks the token and answers from a table of paths.
func agentServer(t *testing.T, routes map[string]http.HandlerFunc) (*client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(w, `{"code":"unauthorized","message":"unauthorized"}`, 401)
			return
		}
		h, ok := routes[r.URL.Path]
		if !ok {
			http.Error(w, `{"code":"not_found","message":"not found"}`, 404)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	target := &agentclient.Target{Project: "demo", Env: "prod", Base: base, Prefix: "/v1/projects/demo/environments/prod/", Token: testToken}
	return newClient(ctx, target), srv
}

func jsonReply(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}
}

func TestFetchesDecodeTypedMessages(t *testing.T) {
	c, _ := agentServer(t, map[string]http.HandlerFunc{
		"/v1/projects/demo/environments/prod/agent":       jsonReply(`{"environment":"prod","hostname":"lab","protected":true}`),
		"/v1/projects/demo/environments/prod/apps":        jsonReply(`[{"ref":{"kind":"image","name":"api"},"roles":[{"name":"web","desired":2,"running":2,"replicas":[]}]}]`),
		"/v1/projects/demo/environments/prod/lock":        jsonReply(`{"locked":true,"holder":"ci","reason":"x"}`),
		"/v1/projects/demo/environments/prod/approvals":   jsonReply(`[{"id":"a","status":"pending","summary":"deploy api v1"}]`),
		"/v1/projects/demo/environments/prod/history/api": jsonReply(`[{"id":1,"digest":"sha256:a","created":"2026-11-16T10:00:00Z"},{"operation_id":"o","state":"failed","reason":"boom","created":"2026-11-16T11:00:00Z"}]`),
		"/v1/projects/demo/environments/prod/df":          jsonReply(`{"host":[{"path":"/","total":10,"available":5}],"host_status":"available","docker_status":"available","docker":{"LayersSize":5}}`),
		"/v1/projects/demo/environments/prod/stats":       jsonReply(`[{"time":"2026-11-16T11:59:00Z","host":{"cpu_percent":5.5},"containers":[]}]`),
	})
	if m, ok := c.fetchInfo()().(infoMsg); !ok || !m.Protected || m.Hostname != "lab" {
		t.Errorf("info: %#v", m)
	}
	if m, ok := c.fetchApps()().(appsMsg); !ok || len(m.apps) != 1 || m.apps[0].Ref.Name != "api" {
		t.Errorf("apps: %#v", m)
	}
	if m, ok := c.fetchLock()().(lockMsg); !ok || !m.Locked || m.Holder != "ci" {
		t.Errorf("lock: %#v", m)
	}
	if m, ok := c.fetchPending()().(pendingMsg); !ok || m.n != 1 {
		t.Errorf("pending: %#v", m)
	}
	if m, ok := c.fetchApprovals()().(approvalsMsg); !ok || len(m.rows) != 1 {
		t.Errorf("approvals: %#v", m)
	}
	h, ok := c.fetchHistory("api")().(historyMsg)
	if !ok || len(h.rows) != 2 || !h.rows[0].failed() || h.rows[1].failed() {
		t.Errorf("history (newest first, failed flagged): %#v", h)
	}
	if m, ok := c.fetchDF()().(dfMsg); !ok || m.Docker.LayersSize != 5 {
		t.Errorf("df: %#v", m)
	}
	if m, ok := c.fetchStatsSeed()().(statsSeedMsg); !ok || len(m.samples) != 1 {
		t.Errorf("stats seed: %#v", m)
	}
}

func TestFetchErrors(t *testing.T) {
	c, srv := agentServer(t, map[string]http.HandlerFunc{
		"/v1/projects/demo/environments/prod/apps": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"code":"forbidden","message":"forbidden `+testToken+`"}`, 403)
		},
		"/v1/projects/demo/environments/prod/lock": jsonReply(`not json`),
		"/v1/projects/demo/environments/prod/df":   func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) },
	})
	e, ok := c.fetchApps()().(errMsg)
	if !ok || e.network || !strings.Contains(e.text, "HTTP 403") {
		t.Fatalf("403: %#v", e)
	}
	if strings.Contains(e.text, testToken) {
		t.Fatal("the token is in the error text")
	}
	if e, ok := c.fetchLock()().(errMsg); !ok || e.network || e.text != "invalid agent response" {
		t.Fatalf("invalid JSON: %#v", e)
	}
	if e, ok := c.fetchDF()().(errMsg); !ok || e.network || e.text != "agent returned HTTP 500" {
		t.Fatalf("500: %#v", e)
	}
	srv.Close()
	if e, ok := c.fetchApps()().(errMsg); !ok || !e.network || e.text != "agent unreachable" {
		t.Fatalf("closed server: %#v", e)
	}
}

func TestWrongTokenIsRefusalNotOutage(t *testing.T) {
	c, _ := agentServer(t, nil)
	c.target.Token = "wrong"
	if e, ok := c.fetchApps()().(errMsg); !ok || e.network || !strings.Contains(e.text, "401") {
		t.Fatalf("%#v", e)
	}
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 15 * time.Second}
	for i, w := range want {
		if got := backoff(i + 1); got != w {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}

// collect reads messages from a stream until n of them satisfy stop.
func collect(t *testing.T, ch <-chan tea.Msg, stop func(tea.Msg) bool) []tea.Msg {
	t.Helper()
	var out []tea.Msg
	timeout := time.After(10 * time.Second)
	for {
		select {
		case m := <-ch:
			out = append(out, m)
			if stop(m) {
				return out
			}
		case <-timeout:
			t.Fatalf("timed out; got %d messages: %#v", len(out), out)
		}
	}
}

func TestLogStreamReconnectsAndSkipsSeenLines(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	c, _ := agentServer(t, map[string]http.HandlerFunc{
		"/v1/projects/demo/environments/prod/apps/api/logs": func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			queries = append(queries, r.URL.RawQuery)
			n := len(queries)
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			if n == 1 {
				fmt.Fprint(w, ": hello\n\nevent: log\ndata: {\"role\":\"web\",\"replica\":\"1\",\"timestamp\":\"2026-11-16T12:00:00Z\",\"stream\":\"stdout\",\"message\":\"one\"}\n\n")
				return // the stream drops
			}
			fmt.Fprint(w, "event: log\ndata: {\"role\":\"web\",\"replica\":\"1\",\"timestamp\":\"2026-11-16T12:00:00Z\",\"stream\":\"stdout\",\"message\":\"one\"}\n\n"+
				"event: log\ndata: {\"role\":\"web\",\"replica\":\"1\",\"timestamp\":\"2026-11-16T12:00:05Z\",\"stream\":\"stdout\",\"message\":\"two\"}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan tea.Msg, 64)
	go func() { defer close(ch); c.streamLogs(ctx, "api", 7, channelSender(ctx, ch)) }()
	msgs := collect(t, ch, func(m tea.Msg) bool { l, ok := m.(logMsg); return ok && l.rec.Message == "two" })
	var lines []string
	var states []streamState
	for _, m := range msgs {
		switch x := m.(type) {
		case logMsg:
			lines = append(lines, x.rec.Message)
			if x.gen != 7 {
				t.Errorf("gen %d", x.gen)
			}
		case streamStateMsg:
			states = append(states, x.state)
		}
	}
	if strings.Join(lines, ",") != "one,two" {
		t.Fatalf("lines %v (a line seen before the drop must not repeat)", lines)
	}
	sawRetry := false
	for _, s := range states {
		sawRetry = sawRetry || s == streamRetrying
	}
	if !sawRetry {
		t.Fatalf("no retry state: %v", states)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(queries) < 2 || !strings.Contains(queries[0], "tail=200") || !strings.Contains(queries[0], "follow=1") || strings.Contains(queries[0], "since") {
		t.Fatalf("first query %v", queries)
	}
	if !strings.Contains(queries[1], "since=2026-11-16T11%3A59%3A59Z") {
		t.Fatalf("reconnect did not ask for the lines since the last one: %v", queries[1])
	}
}

func TestStatsStreamAndCancel(t *testing.T) {
	c, _ := agentServer(t, map[string]http.HandlerFunc{
		"/v1/projects/demo/environments/prod/stats": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("watch") != "1" {
				http.Error(w, "no", 400)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: stats\ndata: {\"time\":\"2026-11-16T12:00:00Z\",\"host\":{\"cpu_percent\":12},\"containers\":[],\"containers_fresh\":true}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan tea.Msg, 16)
	done := make(chan struct{})
	go func() { defer close(done); c.streamStats(ctx, 1, channelSender(ctx, ch)) }()
	msgs := collect(t, ch, func(m tea.Msg) bool { _, ok := m.(statsMsg); return ok })
	if s := msgs[len(msgs)-1].(statsMsg); *s.sample.Host.CPUPercent != 12 {
		t.Fatalf("sample %#v", s)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not stop when cancelled")
	}
}

func TestStreamRefusalIsReportedAndRetried(t *testing.T) {
	c, _ := agentServer(t, map[string]http.HandlerFunc{
		"/v1/projects/demo/environments/prod/apps/api/logs": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"code":"too_many_followers","message":"too many followers"}`, 503)
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan tea.Msg, 16)
	go c.streamLogs(ctx, "api", 1, channelSender(ctx, ch))
	msgs := collect(t, ch, func(m tea.Msg) bool { s, ok := m.(streamStateMsg); return ok && s.state == streamRetrying })
	s := msgs[len(msgs)-1].(streamStateMsg)
	if !strings.Contains(s.text, "HTTP 503") || s.retry != time.Second {
		t.Fatalf("%#v", s)
	}
}

func TestListenBatches(t *testing.T) {
	ch := make(chan tea.Msg, 10)
	for i := 0; i < 5; i++ {
		ch <- logMsg{gen: 1, rec: logs.Record{Message: fmt.Sprint(i)}}
	}
	b := listen(ch, "logs", 1)().(batchMsg)
	if len(b.msgs) != 5 || b.closed {
		t.Fatalf("batch %#v", b)
	}
	close(ch)
	if b = listen(ch, "logs", 1)().(batchMsg); !b.closed {
		t.Fatal("a closed channel is not reported")
	}
}

// expand runs a command and the commands of a batch, and returns what they produced. Commands that wait are given a limit.
func expand(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if b, ok := msg.(tea.BatchMsg); ok {
			var out []tea.Msg
			for _, c := range b {
				out = append(out, expand(c)...)
			}
			return out
		}
		return []tea.Msg{msg}
	case <-time.After(3 * time.Second):
		return nil
	}
}

func TestModelFollowsLogsThroughTheStream(t *testing.T) {
	c, _ := agentServer(t, map[string]http.HandlerFunc{
		"/v1/projects/demo/environments/prod/apps/api/logs": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: log\ndata: {\"role\":\"web\",\"replica\":\"1\",\"timestamp\":\"2026-11-16T12:00:00Z\",\"stream\":\"stdout\",\"message\":\"from the stream\"}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		},
	})
	m := NewModel(c, "prod", func() time.Time { return testNow })
	m.noAnim = true
	send(m, tea.WindowSizeMsg{Width: 120, Height: 40}, appsMsg{apps: fixtureApps()})
	m.Update(keypress("2"))
	_, cmd := m.Update(keypress("enter"))
	if m.logs.app != "api" {
		t.Fatalf("app %q", m.logs.app)
	}
	deadline := time.Now().Add(10 * time.Second)
	pending := []tea.Cmd{cmd}
	for !strings.Contains(plain(m), "from the stream") {
		if time.Now().After(deadline) || len(pending) == 0 {
			t.Fatalf("the line never arrived: %s", plain(m))
		}
		var next []tea.Cmd
		for _, c := range pending {
			for _, msg := range expand(c) {
				if _, nc := m.Update(msg); nc != nil {
					next = append(next, nc)
				}
			}
		}
		pending = next
	}
	if !m.logs.live {
		t.Fatal("the stream is not marked live")
	}
	m.shutdown()
}

// twoProjectUIConfig writes a CLI configuration that points prod of cardiotennis and sliceup at two agents, and returns the
// path and a counter for each agent's requests.
func twoProjectUIConfig(t *testing.T) (cfg string, cardio, slice *atomic.Int32) {
	t.Helper()
	serve := func(n *atomic.Int32) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			fmt.Fprint(w, `[]`)
		}))
		t.Cleanup(s.Close)
		return s
	}
	cardio, slice = new(atomic.Int32), new(atomic.Int32)
	a, b := serve(cardio), serve(slice)
	t.Setenv("BERTHY_TEST_UI_CT", testToken)
	t.Setenv("BERTHY_TEST_UI_SU", testToken)
	cfg = filepath.Join(t.TempDir(), "config.json")
	body := fmt.Sprintf(`{"projects":{"cardiotennis":{"environments":{"prod":{"url":%q,"token_env":"BERTHY_TEST_UI_CT"}}},"sliceup":{"environments":{"prod":{"url":%q,"token_env":"BERTHY_TEST_UI_SU"}}}}}`, a.URL, b.URL)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, cardio, slice
}

func TestTheUIReachesTheProjectOfTheFlag(t *testing.T) {
	cfg, cardio, slice := twoProjectUIConfig(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte("project: cardiotennis\nnetwork: demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	var out, errs strings.Builder
	code := Run([]string{"-e", "prod", "--project", "sliceup", "--config", cfg}, strings.NewReader(""), &out, &errs)
	if code != 1 || !strings.Contains(errs.String(), "berthy ui needs a terminal") {
		t.Fatalf("exit %d, stderr %q", code, errs.String())
	}
	if cardio.Load() != 0 || slice.Load() != 0 {
		t.Fatalf("Run reached the agent before the terminal check: cardio %d slice %d", cardio.Load(), slice.Load())
	}
	if code := Run([]string{"-e", "prod", "--project", "Bad_Name", "--config", cfg}, strings.NewReader(""), &out, &errs); code != 2 || !strings.Contains(errs.String(), "valid --project required") {
		t.Fatalf("invalid project: exit %d, stderr %q", code, errs.String())
	}

	target, err := agentclient.Resolve(cfg, agentclient.Selector{Project: "sliceup", Environment: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t.Context(), target)
	msg := c.fetchApps()()
	if e, ok := msg.(errMsg); ok {
		t.Fatalf("fetch: %#v", e)
	}
	if cardio.Load() != 0 || slice.Load() != 1 {
		t.Fatalf("requests: cardio %d slice %d", cardio.Load(), slice.Load())
	}
	if !strings.HasPrefix(target.Prefix, "/v1/projects/sliceup/environments/prod/") {
		t.Fatalf("prefix %q", target.Prefix)
	}
}

func TestTheUISaysToUpgradeAnAgentWithoutProjects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/environments/prod/agent" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	c := newClient(t.Context(), &agentclient.Target{Project: "demo", Env: "prod", Base: base, Prefix: "/v1/projects/demo/environments/prod/", Token: testToken})

	e, ok := c.fetchApps()().(errMsg)
	if !ok || !strings.Contains(e.text, agentclient.OlderAgentMessage) {
		t.Fatalf("get: %#v", e)
	}

	reason := c.readStream(t.Context(), "apps/api/logs", "", func(string, string) error { return nil })
	if !strings.Contains(reason, agentclient.OlderAgentMessage) {
		t.Fatalf("stream: %q", reason)
	}

	data, status, _, err := c.post("restart", map[string]any{"app": "api"})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.refusal(status, data); !strings.Contains(got, agentclient.OlderAgentMessage) {
		t.Fatalf("post: status %d %q", status, got)
	}
}
