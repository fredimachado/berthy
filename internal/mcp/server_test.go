package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/agentclient"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const testToken = "bty.dev.0123456789abcdef-sentinel"

const imageManifest = `project: demo
network: demo
apps:
  api:
    kind: image
    image: alpine
    roles:
      web: {}
`

// request is one request the fake agent received.
type request struct {
	Method, Path, Query, Auth string
	Body                      map[string]any
}

// reply is what the fake agent answers.
type reply struct {
	Status int
	Header map[string]string
	Body   string
}

// fakeAgent is an httptest agent that records every request and answers from a table keyed "METHOD path".
type fakeAgent struct {
	t      *testing.T
	mu     sync.Mutex
	calls  []request
	routes map[string]func(r request) reply
}

const base = "/v1/projects/demo/environments/dev/"

func newFakeAgent(t *testing.T) (*fakeAgent, *url.URL) {
	t.Helper()
	f := &fakeAgent{t: t, routes: map[string]func(r request) reply{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		req := request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization")}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &req.Body); err != nil {
				t.Errorf("request body is not JSON: %q", raw)
			}
		}
		f.mu.Lock()
		f.calls = append(f.calls, req)
		route, ok := f.routes[r.Method+" "+r.URL.Path]
		f.mu.Unlock()
		if !ok {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, `{"code":"not_found","message":"no such route"}`, 404)
			return
		}
		rep := route(req)
		for k, v := range rep.Header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(rep.Status)
		io.WriteString(w, rep.Body)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return f, u
}

// on registers a fixed answer.
func (f *fakeAgent) on(method, path string, status int, body string) {
	f.routes[method+" "+base+path] = func(request) reply { return reply{Status: status, Body: body} }
}

func (f *fakeAgent) onFunc(method, path string, fn func(r request) reply) {
	f.routes[method+" "+base+path] = fn
}

func (f *fakeAgent) requests() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.calls...)
}

func (f *fakeAgent) only(t *testing.T) request {
	t.Helper()
	got := f.requests()
	if len(got) != 1 {
		t.Fatalf("want one agent request, got %+v", got)
	}
	return got[0]
}

func testConfig(u *url.URL, dir string) Config {
	return Config{
		Target:        &agentclient.Target{Env: "dev", Base: u, Prefix: base, Token: testToken},
		ManifestDir:   dir,
		Version:       "test",
		PollInterval:  2 * time.Millisecond,
		OperationWait: 3 * time.Second,
	}
}

// connect runs the server in process and returns a client session.
func connect(t *testing.T, cfg Config) *sdk.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st, ct := sdk.NewInMemoryTransports()
	server := NewServer(cfg)
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close(); ss.Wait() })
	return cs
}

func call(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	return res
}

func text(t *testing.T, res *sdk.CallToolResult) string {
	t.Helper()
	var b strings.Builder
	for _, c := range res.Content {
		tc, ok := c.(*sdk.TextContent)
		if !ok {
			t.Fatalf("content is %T", c)
		}
		b.WriteString(tc.Text)
		b.WriteString("\n")
	}
	return b.String()
}

func structured(t *testing.T, res *sdk.CallToolResult) map[string]any {
	t.Helper()
	m, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content is %T: %v", res.StructuredContent, res.StructuredContent)
	}
	return m
}

func okResult(t *testing.T, res *sdk.CallToolResult) *sdk.CallToolResult {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool error: %s", text(t, res))
	}
	return res
}

func manifestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(imageManifest), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestToolsListIsExactlyTheToolSet(t *testing.T) {
	_, u := newFakeAgent(t)
	cs := connect(t, testConfig(u, t.TempDir()))
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
		if tool.Annotations == nil {
			t.Errorf("%s has no annotations", tool.Name)
			continue
		}
		if readOnly := tool.Name != "execute_resource_command"; tool.Annotations.ReadOnlyHint != readOnly {
			t.Errorf("%s: ReadOnlyHint is %v", tool.Name, tool.Annotations.ReadOnlyHint)
		}
	}
	sort.Strings(names)
	want := []string{"describe_resource", "doctor", "execute_resource_command", "get_deploy_diagnostics", "get_stats", "list_approvals", "list_backups", "list_console_logs", "list_deploys", "list_resources", "wait_for_resources"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools %v", names)
	}
	for _, tool := range list.Tools {
		if tool.Name == "execute_resource_command" {
			if !strings.Contains(tool.Description, "held for approval") || !strings.Contains(tool.Description, "protected") {
				t.Errorf("execute_resource_command does not explain the approval: %q", tool.Description)
			}
			if tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
				t.Error("execute_resource_command is not marked destructive")
			}
		}
	}
}

func TestReadToolsMapToTheAgentRoutes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tool  string
		args  map[string]any
		path  string
		query string
		body  string
		key   string // the key the agent's array is wrapped in; empty when the agent's object is passed on
	}{
		{"ps", "list_resources", nil, "apps", "", `[{"ref":{"kind":"image","name":"api"}}]`, "resources"},
		{"describe", "describe_resource", map[string]any{"resource_name": "api"}, "apps/api", "", `{"name":"api","roles":[]}`, ""},
		{"describe qualified", "describe_resource", map[string]any{"resource_name": "compose:db"}, "apps/compose:db", "", `{"name":"db"}`, ""},
		{"history", "list_deploys", map[string]any{"resource_name": "api"}, "history/api", "", `[{"operation_id":"op1","state":"failed","reason":"task failed"}]`, "deploys"},
		{"diagnostics", "get_deploy_diagnostics", map[string]any{"operation_id": "op1"}, "operations/op1/diagnostics", "", `{"operation_id":"op1","reason":"x","diagnostics":[]}`, ""},
		{"backups", "list_backups", nil, "backups", "", `[{"backup":"main-db"}]`, "backups"},
		{"backups of one", "list_backups", map[string]any{"backup_name": "main-db"}, "backups", "backup=main-db", `[]`, "backups"},
		{"approvals", "list_approvals", nil, "approvals", "", `[{"id":"a1","status":"pending"}]`, "approvals"},
		{"doctor", "doctor", nil, "doctor", "", `{"checks":[{"name":"host","ok":true,"critical":true}]}`, ""},
		{"wait", "wait_for_resources", map[string]any{"resource_name": "api"}, "wait/api", "healthy=true", `{"status":"healthy"}`, ""},
		{"wait with role and timeout", "wait_for_resources", map[string]any{"resource_name": "api", "role": "web", "timeout": "90s"}, "wait/api", "healthy=true&role=web&timeout=90s", `{"status":"healthy"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent, u := newFakeAgent(t)
			agent.on("GET", tc.path, 200, tc.body)
			res := okResult(t, call(t, connect(t, testConfig(u, t.TempDir())), tc.tool, tc.args))
			req := agent.only(t)
			if req.Method != "GET" || req.Path != base+tc.path || req.Query != tc.query || req.Auth != "Bearer "+testToken {
				t.Fatalf("agent request %+v", req)
			}
			got := structured(t, res)
			var want any
			if err := json.Unmarshal([]byte(tc.body), &want); err != nil {
				t.Fatal(err)
			}
			if tc.key != "" {
				want = map[string]any{tc.key: want}
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("structured content %s, want %s", gotJSON, wantJSON)
			}
			if strings.TrimSpace(text(t, res)) == "" {
				t.Fatal("no text summary")
			}
		})
	}
}

func TestListConsoleLogsReturnsLinesAndBoundsTheTail(t *testing.T) {
	agent, u := newFakeAgent(t)
	agent.on("GET", "apps/api/logs", 200, `[{"role":"web","replica":"1","timestamp":"2026-01-02T03:04:05Z","stream":"stdout","message":"listening on :8080"},{"role":"web","replica":"1","timestamp":"2026-01-02T03:04:06Z","stream":"stderr","message":"boom"}]`)
	cs := connect(t, testConfig(u, t.TempDir()))

	res := okResult(t, call(t, cs, "list_console_logs", map[string]any{"resource_name": "api"}))
	req := agent.only(t)
	if req.Query != "tail=200" {
		t.Fatalf("default tail: %q", req.Query)
	}
	if out := text(t, res); !strings.Contains(out, "listening on :8080") || !strings.Contains(out, "boom") || !strings.Contains(out, "2026-01-02T03:04:05Z") {
		t.Fatalf("log lines missing from %q", out)
	}
	if records, ok := structured(t, res)["records"].([]any); !ok || len(records) != 2 {
		t.Fatalf("structured records: %v", res.StructuredContent)
	}

	okResult(t, call(t, cs, "list_console_logs", map[string]any{"resource_name": "api", "tail": 1000, "since": "10m", "grep": "err.*", "role": "web"}))
	q, _ := url.ParseQuery(agent.requests()[1].Query)
	if q.Get("tail") != "1000" || q.Get("since") != "10m" || q.Get("grep") != "err.*" || q.Get("role") != "web" || q.Get("follow") != "" {
		t.Fatalf("query %v", q)
	}

	for _, tail := range []int{1001, -1} {
		res := call(t, cs, "list_console_logs", map[string]any{"resource_name": "api", "tail": tail})
		if !res.IsError || !strings.Contains(text(t, res), "tail") {
			t.Fatalf("tail %d: %v %q", tail, res.IsError, text(t, res))
		}
	}
	if n := len(agent.requests()); n != 2 {
		t.Fatalf("an out-of-range tail reached the agent: %d requests", n)
	}
}

func TestGetStatsCombinesStatsAndDisk(t *testing.T) {
	agent, u := newFakeAgent(t)
	agent.on("GET", "stats", 200, `{"cpu":12.5}`)
	agent.on("GET", "df", 200, `{"host_status":"available"}`)
	res := okResult(t, call(t, connect(t, testConfig(u, t.TempDir())), "get_stats", nil))
	got := structured(t, res)
	if got["stats"].(map[string]any)["cpu"] != 12.5 || got["df"].(map[string]any)["host_status"] != "available" {
		t.Fatalf("combined %v", got)
	}
	agent.on("GET", "df", 503, `{"code":"x","message":"disk unavailable"}`)
	if res := call(t, connect(t, testConfig(u, t.TempDir())), "get_stats", nil); !res.IsError || !strings.Contains(text(t, res), "disk unavailable") {
		t.Fatalf("df failure: %v %q", res.IsError, text(t, res))
	}
}

func TestWaitTimeoutIsBounded(t *testing.T) {
	agent, u := newFakeAgent(t)
	agent.on("GET", "wait/api", 200, `{"status":"healthy"}`)
	cs := connect(t, testConfig(u, t.TempDir()))
	for _, timeout := range []string{"10m1s", "0s", "-5s", "soon"} {
		if res := call(t, cs, "wait_for_resources", map[string]any{"resource_name": "api", "timeout": timeout}); !res.IsError {
			t.Errorf("timeout %q accepted", timeout)
		}
	}
	if len(agent.requests()) != 0 {
		t.Fatal("a bad timeout reached the agent")
	}
	okResult(t, call(t, cs, "wait_for_resources", map[string]any{"resource_name": "api", "timeout": "10m"}))
	if q := agent.only(t).Query; q != "healthy=true&timeout=10m" {
		t.Fatalf("query %q", q)
	}
}

func TestAgentRefusalIsAToolErrorWithItsMessage(t *testing.T) {
	agent, u := newFakeAgent(t)
	agent.on("GET", "operations/op1/diagnostics", 403, `{"code":"forbidden","message":"token scope read cannot read diagnostics"}`)
	agent.on("GET", "apps/dup", 409, `{"code":"conflict","message":"ambiguous app; use image:, swarm: or compose:"}`)
	cs := connect(t, testConfig(u, t.TempDir()))
	res := call(t, cs, "get_deploy_diagnostics", map[string]any{"operation_id": "op1"})
	if !res.IsError || !strings.Contains(text(t, res), "HTTP 403: token scope read cannot read diagnostics") {
		t.Fatalf("403: %v %q", res.IsError, text(t, res))
	}
	res = call(t, cs, "describe_resource", map[string]any{"resource_name": "dup"})
	if !res.IsError || !strings.Contains(text(t, res), "HTTP 409: ambiguous app") {
		t.Fatalf("409: %v %q", res.IsError, text(t, res))
	}
}

func TestAgentUnreachableIsAToolError(t *testing.T) {
	agent, u := newFakeAgent(t)
	_ = agent
	srv := httptest.NewServer(http.NotFoundHandler())
	dead, _ := url.Parse(srv.URL)
	srv.Close()
	_ = u
	res := call(t, connect(t, testConfig(dead, t.TempDir())), "list_resources", nil)
	if !res.IsError || !strings.Contains(text(t, res), "agent connection failed") {
		t.Fatalf("%v %q", res.IsError, text(t, res))
	}
}

func TestExecuteDeployReadsTheManifestAndFollowsTheOperation(t *testing.T) {
	agent, u := newFakeAgent(t)
	agent.on("POST", "deploys", 202, `{"operation_id":"op1","events_url":"/v1/projects/demo/environments/dev/operations/op1/events"}`)
	agent.on("GET", "operations/op1/events", 200, "event: terminal\ndata: {}\n\n")
	var polls int
	agent.onFunc("GET", "operations/op1", func(request) reply {
		polls++
		if polls < 3 {
			return reply{Status: 200, Body: `{"id":"op1","state":"running"}`}
		}
		return reply{Status: 200, Body: `{"id":"op1","state":"succeeded"}`}
	})
	cs := connect(t, testConfig(u, manifestDir(t)))
	res := okResult(t, call(t, cs, "execute_resource_command", map[string]any{"resource_name": "api", "command": "deploy", "version": "1.2.3"}))
	got := structured(t, res)
	if got["operation_id"] != "op1" || got["state"] != "succeeded" {
		t.Fatalf("result %v", got)
	}
	post := agent.requests()[0]
	if post.Method != "POST" || post.Path != base+"deploys" || post.Body["app"] != "api" || post.Body["version"] != "1.2.3" || post.Body["request_id"] == "" || post.Body["dry_run"] != nil {
		t.Fatalf("deploy request %+v", post)
	}
	if m, _ := post.Body["manifest"].(map[string]any); m["project"] != "demo" {
		t.Fatalf("the manifest was not sent: %v", post.Body["manifest"])
	}
	if polls < 3 {
		t.Fatalf("operation was not followed to its end: %d polls", polls)
	}
}

func TestExecuteDeployReportsWhatTheChecksFound(t *testing.T) {
	const warning = "role web: healthy, but port 8080 does not answer on the network; is the app listening on 127.0.0.1?"
	agent, u := newFakeAgent(t)
	agent.on("POST", "deploys", 202, `{"operation_id":"op1","events_url":"/v1/projects/demo/environments/dev/operations/op1/events"}`)
	agent.on("GET", "operations/op1", 200, `{"id":"op1","state":"succeeded"}`)
	agent.on("GET", "operations/op1/events", 200, "id: 1\nevent: accepted\ndata: {}\n\nid: 2\nevent: verify\ndata: {\"role\":\"web\",\"check\":\"network\",\"status\":\"warn\",\"step\":\""+warning+"\"}\n\nevent: terminal\ndata: {}\n\n")
	res := okResult(t, call(t, connect(t, testConfig(u, manifestDir(t))), "execute_resource_command", map[string]any{"resource_name": "api", "command": "deploy", "version": "1.2.3"}))
	got := structured(t, res)
	checks, _ := got["checks"].([]any)
	if got["state"] != "succeeded" || len(checks) != 1 || checks[0] != warning || !strings.Contains(text(t, res), warning) {
		t.Fatalf("result %v, text %q", got, text(t, res))
	}
}

func TestExecuteReportsAFailedOperationWithItsReason(t *testing.T) {
	agent, u := newFakeAgent(t)
	agent.on("POST", "deploys", 202, `{"operation_id":"op9","events_url":"x"}`)
	agent.on("GET", "operations/op9/events", 200, "event: terminal\ndata: {}\n\n")
	agent.on("GET", "operations/op9", 200, `{"id":"op9","state":"rolled_back","reason":"task web.1 exited with code 1"}`)
	res := call(t, connect(t, testConfig(u, manifestDir(t))), "execute_resource_command", map[string]any{"resource_name": "api", "command": "deploy", "version": "1.2.3"})
	got := structured(t, res)
	if !res.IsError || got["state"] != "rolled_back" || got["reason"] != "task web.1 exited with code 1" || got["operation_id"] != "op9" {
		t.Fatalf("%v %v", res.IsError, got)
	}
	if !strings.Contains(text(t, res), "get_deploy_diagnostics") {
		t.Fatalf("no pointer to the diagnostics: %q", text(t, res))
	}
}

func TestExecuteStopsWaitingAfterTheBound(t *testing.T) {
	agent, u := newFakeAgent(t)
	agent.on("POST", "restart", 202, `{"operation_id":"op2","events_url":"x"}`)
	agent.on("GET", "operations/op2", 200, `{"id":"op2","state":"running"}`)
	cfg := testConfig(u, t.TempDir())
	cfg.OperationWait = 30 * time.Millisecond
	res := okResult(t, call(t, connect(t, cfg), "execute_resource_command", map[string]any{"resource_name": "api", "command": "restart", "role": "web"}))
	got := structured(t, res)
	if got["state"] != "running" || got["operation_id"] != "op2" || !strings.Contains(text(t, res), "still running") {
		t.Fatalf("%v %q", got, text(t, res))
	}
	if body := agent.requests()[0].Body; body["app"] != "api" || body["role"] != "web" {
		t.Fatalf("restart request %v", body)
	}
}

func TestExecuteHeldOnAProtectedEnvironment(t *testing.T) {
	agent, u := newFakeAgent(t)
	agent.onFunc("POST", "deploys", func(request) reply {
		return reply{Status: 202, Header: map[string]string{"Berthy-Approval": "pending"},
			Body: `{"approval_id":"apr-123","expires":"2026-11-16T12:00:00Z","plan":{"app":"api","version":"sha256:abc"}}`}
	})
	cs := connect(t, testConfig(u, manifestDir(t)))
	res := okResult(t, call(t, cs, "execute_resource_command", map[string]any{"resource_name": "api", "command": "deploy", "version": "1.2.3"}))
	got := structured(t, res)
	if got["approval_id"] != "apr-123" || got["status"] != "held for approval" || got["expires"] != "2026-11-16T12:00:00Z" || got["plan"].(map[string]any)["app"] != "api" {
		t.Fatalf("held result %v", got)
	}
	if !strings.Contains(text(t, res), "held for approval apr-123: a person must run berthy approve apr-123") {
		t.Fatalf("text %q", text(t, res))
	}
	if n := len(agent.requests()); n != 1 {
		t.Fatalf("a held request was followed: %d requests", n)
	}
}

func TestExecuteDryRunReturnsThePlan(t *testing.T) {
	agent, u := newFakeAgent(t)
	agent.on("POST", "deploys", 200, `{"app":"api","steps":["pull","update"]}`)
	cs := connect(t, testConfig(u, manifestDir(t)))
	res := okResult(t, call(t, cs, "execute_resource_command", map[string]any{"resource_name": "api", "command": "deploy", "version": "1.2.3", "dry_run": true}))
	plan, _ := structured(t, res)["plan"].(map[string]any)
	if plan["app"] != "api" || structured(t, res)["dry_run"] != true {
		t.Fatalf("result %v", res.StructuredContent)
	}
	if agent.only(t).Body["dry_run"] != true {
		t.Fatalf("dry_run did not reach the agent: %v", agent.only(t).Body)
	}
}

func TestExecuteRollbackRestoreAndBackupRun(t *testing.T) {
	agent, u := newFakeAgent(t)
	agent.on("POST", "rollback", 202, `{"operation_id":"op3","events_url":"x"}`)
	agent.on("POST", "restore", 202, `{"operation_id":"op4","events_url":"x"}`)
	agent.on("GET", "operations/op3/events", 200, "event: terminal\ndata: {}\n\n")
	agent.on("GET", "operations/op3", 200, `{"id":"op3","state":"succeeded"}`)
	agent.on("GET", "operations/op4", 200, `{"id":"op4","state":"succeeded"}`)
	agent.on("POST", "backups/main-db/run", 202, `{"backup_run_id":7}`)
	agent.on("GET", "backups/main-db/runs", 200, `[{"id":7,"backup":"main-db","status":"succeeded","key":"main-db/2026.tar","size":42,"sha256":"abc"}]`)
	cs := connect(t, testConfig(u, t.TempDir()))

	okResult(t, call(t, cs, "execute_resource_command", map[string]any{"resource_name": "api", "command": "rollback", "to": 4}))
	if b := agent.requests()[0].Body; b["app"] != "api" || b["target_release"] != float64(4) {
		t.Fatalf("rollback request %v", b)
	}

	okResult(t, call(t, cs, "execute_resource_command", map[string]any{"command": "restore", "backup_key": "main-db/2026.tar", "into_volume": "restored"}))
	var restore request
	for _, r := range agent.requests() {
		if r.Path == base+"restore" {
			restore = r
		}
	}
	if restore.Body["key"] != "main-db/2026.tar" || restore.Body["into"] != "restored" || restore.Body["request_id"] == "" {
		t.Fatalf("restore request %v", restore.Body)
	}

	res := okResult(t, call(t, cs, "execute_resource_command", map[string]any{"command": "backup_run", "backup_name": "main-db"}))
	got := structured(t, res)
	if got["backup_run_id"] != float64(7) || got["state"] != "succeeded" || got["key"] != "main-db/2026.tar" {
		t.Fatalf("backup result %v", got)
	}
}

func TestExecuteBackupRunHeld(t *testing.T) {
	agent, u := newFakeAgent(t)
	agent.onFunc("POST", "backups/main-db/run", func(request) reply {
		return reply{Status: 202, Header: map[string]string{"Berthy-Approval": "pending"}, Body: `{"approval_id":"apr-9","expires":"2026-11-16T12:00:00Z","plan":{"action":"backup run","target":"main-db"}}`}
	})
	res := okResult(t, call(t, connect(t, testConfig(u, t.TempDir())), "execute_resource_command", map[string]any{"command": "backup_run", "backup_name": "main-db"}))
	if structured(t, res)["approval_id"] != "apr-9" || len(agent.requests()) != 1 {
		t.Fatalf("%v %d", res.StructuredContent, len(agent.requests()))
	}
}

func TestExecuteRefusesBadArgumentsBeforeTheAgent(t *testing.T) {
	agent, u := newFakeAgent(t)
	cs := connect(t, testConfig(u, manifestDir(t)))
	for name, args := range map[string]map[string]any{
		"unknown command":          {"command": "destroy", "resource_name": "api"},
		"deploy without resource":  {"command": "deploy", "version": "1"},
		"deploy without version":   {"command": "deploy", "resource_name": "api"},
		"deploy with to":           {"command": "deploy", "resource_name": "api", "version": "1", "to": 2},
		"deploy of another image":  {"command": "deploy", "resource_name": "api", "version": "other/image:1"},
		"rollback with version":    {"command": "rollback", "resource_name": "api", "version": "1"},
		"rollback to zero":         {"command": "rollback", "resource_name": "api", "to": -1},
		"restart without resource": {"command": "restart"},
		"restart with backup name": {"command": "restart", "resource_name": "api", "backup_name": "x"},
		"backup_run without name":  {"command": "backup_run"},
		"backup_run dry run":       {"command": "backup_run", "backup_name": "x", "dry_run": true},
		"restore without key":      {"command": "restore", "into_volume": "v"},
		"restore without volume":   {"command": "restore", "backup_key": "k"},
		"restore with resource":    {"command": "restore", "backup_key": "k", "into_volume": "v", "resource_name": "api"},
		"deploy unknown manifest":  {"command": "deploy", "resource_name": "nope", "version": "1"},
	} {
		if res := call(t, cs, "execute_resource_command", args); !res.IsError {
			t.Errorf("%s: accepted", name)
		}
	}
	if n := len(agent.requests()); n != 0 {
		t.Fatalf("refused arguments reached the agent: %v", agent.requests())
	}
}

func TestExecuteRefusalFromTheAgentIsAToolError(t *testing.T) {
	agent, u := newFakeAgent(t)
	agent.on("POST", "restart", 403, `{"code":"forbidden","message":"token scope read cannot restart"}`)
	res := call(t, connect(t, testConfig(u, t.TempDir())), "execute_resource_command", map[string]any{"resource_name": "api", "command": "restart"})
	if !res.IsError || !strings.Contains(text(t, res), "HTTP 403: token scope read cannot restart") {
		t.Fatalf("%v %q", res.IsError, text(t, res))
	}
}

func TestTheTokenIsNeverInTheOutput(t *testing.T) {
	agent, u := newFakeAgent(t)
	// A careless agent that repeats the credential it was given in its own message and in its data.
	agent.onFunc("GET", "apps", func(r request) reply {
		return reply{Status: 200, Body: `[{"name":"api","note":"` + strings.TrimPrefix(r.Auth, "Bearer ") + `"}]`}
	})
	agent.onFunc("GET", "doctor", func(r request) reply {
		return reply{Status: 401, Body: `{"message":"bad credential ` + strings.TrimPrefix(r.Auth, "Bearer ") + `"}`}
	})
	cs := connect(t, testConfig(u, t.TempDir()))
	for _, res := range []*sdk.CallToolResult{call(t, cs, "list_resources", nil), call(t, cs, "doctor", nil)} {
		raw, _ := json.Marshal(res)
		if strings.Contains(string(raw), testToken) {
			t.Fatalf("token in output: %s", raw)
		}
	}
}

func TestDoctorSummaryNamesFailingChecks(t *testing.T) {
	agent, u := newFakeAgent(t)
	agent.on("GET", "doctor", 200, `{"checks":[{"name":"host","ok":true,"critical":true},{"name":"disk","ok":false,"critical":true,"detail":"95% used"}]}`)
	res := okResult(t, call(t, connect(t, testConfig(u, t.TempDir())), "doctor", nil))
	if out := text(t, res); !strings.Contains(out, "1 of 2 checks failing") || !strings.Contains(out, "disk") {
		t.Fatalf("summary %q", out)
	}
}

func TestDeployVersionErrorsAreWordedForMCPNotTheCLI(t *testing.T) {
	_, u := newFakeAgent(t)
	compose := t.TempDir()
	manifest := "project: demo\nnetwork: demo\napps:\n  db:\n    kind: compose\n    file: compose.yaml\n    stateful: true\n"
	if err := os.WriteFile(filepath.Join(compose, "berthy.yml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(compose, "compose.yaml"), []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, dir string
		args      map[string]any
		want      string
	}{
		{"image without version", manifestDir(t), map[string]any{"command": "deploy", "resource_name": "api"}, "deploy requires version (the image tag or digest)"},
		{"compose with version", compose, map[string]any{"command": "deploy", "resource_name": "db", "version": "1"}, "version does not apply to a compose app"},
	} {
		res := call(t, connect(t, testConfig(u, tc.dir)), "execute_resource_command", tc.args)
		if got := strings.TrimSpace(text(t, res)); !res.IsError || got != tc.want {
			t.Errorf("%s: %v %q", tc.name, res.IsError, got)
		}
	}
}
