package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/agent"
)

func TestAgentTokenPrintsTheTokenItsDigestAndAConfigObject(t *testing.T) {
	run := func(args ...string) (int, string, string) {
		var out, errout bytes.Buffer
		code := Run(append([]string{"agent", "token"}, args...), &out, &errout)
		return code, out.String(), errout.String()
	}
	code, out, errout := run("-e", "lab")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 3 || !strings.HasPrefix(lines[0], "token: bty.lab.") || !strings.HasPrefix(lines[1], "sha256: ") {
		t.Fatalf("default: %d %q %q", code, out, errout)
	}
	token := strings.TrimPrefix(lines[0], "token: ")
	if lines[1] != "sha256: "+agent.Digest(token) {
		t.Fatalf("digest does not match the token: %s", lines[1])
	}
	var obj agent.Token
	if err := json.Unmarshal([]byte(lines[2]), &obj); err != nil || obj.Digest != agent.Digest(token) || obj.Scope != "read" || obj.Agent || obj.Name != "" {
		t.Fatalf("object: %s %v", lines[2], err)
	}
	code, out, _ = run("-e", "lab", "--agent", "--name", "claude", "--scope", "deploy")
	lines = strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 3 {
		t.Fatalf("agent: %d %q", code, out)
	}
	token = strings.TrimPrefix(lines[0], "token: ")
	if lines[2] != fmt.Sprintf(`{"name":"claude","digest":%q,"scope":"deploy","agent":true}`, agent.Digest(token)) {
		t.Fatalf("object: %s", lines[2])
	}
	for _, bad := range [][]string{{}, {"-e", "Bad Env"}, {"-e", "lab", "--scope", "root"}, {"-e", "lab", "--name", "bad name"}, {"-e", "lab", "--name"}, {"-e", "lab", "extra"}, {"-e", "lab", "--bogus"}} {
		if code, out, _ := run(bad...); code != 2 || out != "" {
			t.Errorf("%v: exit %d, output %q", bad, code, out)
		}
	}
}

// heldAgent is an agent that holds every change: it answers a change with a pending approval, and lists it.
type heldAgent struct {
	mu      sync.Mutex
	paths   []string
	bodies  []string
	approve func(w http.ResponseWriter, r *http.Request)
}

const heldBody = `{"approval_id":"ap1","expires":"2026-11-16T12:15:00Z","plan":{"app":"api","image":"ghcr.io/acme/api@sha256:aa","roles":["web"]}}`

func (h *heldAgent) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.paths = append(h.paths, r.Method+" "+r.URL.RequestURI())
		h.bodies = append(h.bodies, string(body))
		h.mu.Unlock()
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/approve") && h.approve != nil:
			h.approve(w, r)
		case r.Method != "GET":
			w.Header().Set("Berthy-Approval", "pending")
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, heldBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestChangesHeldForApprovalExitThreeAndShowThePlan(t *testing.T) {
	h := &heldAgent{}
	server := httptest.NewServer(h.handler())
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	for name, args := range map[string][]string{
		"lock":         {"lock", "acquire", "-m", "x"},
		"secret":       {"secret", "delete", "OLD"},
		"restart":      {"restart", "api"},
		"backup run":   {"backup", "run", "main"},
		"job run":      {"job", "run", "nightly"},
		"state-backup": {"state-backup"},
		"alerts":       {"alerts", "test"},
	} {
		var out, errout bytes.Buffer
		code := Run(append(args, "-e", "dev", "--config", cfg), &out, &errout)
		if code != 3 {
			t.Errorf("%s: exit %d, stdout %q stderr %q", name, code, out.String(), errout.String())
			continue
		}
		for _, want := range []string{"held for approval: ap1 (expires 2026-11-16T12:15:00Z)", "plan:", "app: api", "ghcr.io/acme/api@sha256:aa"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%s: output lacks %q:\n%s", name, want, out.String())
			}
		}
	}
	var out, errout bytes.Buffer
	if code := Run([]string{"lock", "acquire", "-m", "x", "-e", "dev", "--config", cfg, "--json"}, &out, &errout); code != 3 {
		t.Fatalf("json: exit %d", code)
	}
	var shown struct {
		ID string `json:"approval_id"`
	}
	if err := json.Unmarshal(out.Bytes(), &shown); err != nil || shown.ID != "ap1" {
		t.Fatalf("stdout is not the held answer: %q %v", out.String(), err)
	}
}

const pendingRow = `{"id":"ap1","actor":"claude","kind":"deploy","summary":"deploy api sha256:aaaaaaaaaaaa","plan":{"app":"api","roles":["web"]},"created":"2026-11-16T11:58:30Z","expires":"2026-11-16T12:13:30Z","status":"pending"}`
const approvedRow = `{"id":"ap2","actor":"claude","kind":"secret set","summary":"secret set \u001b[31mDATABASE_URL","plan":{"action":"x"},"created":"2026-11-16T11:00:00Z","expires":"2026-11-16T11:15:00Z","status":"approved","decided_by":"alice","result":"done"}`
const approvalsListBody = "[" + pendingRow + "," + approvedRow + "]"

func TestApprovalsListsPendingOrAllAsATable(t *testing.T) {
	approvalsNow = func() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { approvalsNow = time.Now })
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization"))
		if r.URL.Query().Get("status") == "pending" {
			fmt.Fprint(w, "["+pendingRow+"]")
			return
		}
		fmt.Fprint(w, approvalsListBody)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	if code := Run([]string{"approvals", "-e", "dev", "--config", cfg}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if seen[0] != "GET /v1/projects/demo/environments/dev/approvals?status=pending Bearer test-token" {
		t.Fatalf("request: %q", seen[0])
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "ID") || !strings.Contains(lines[0], "AGE") || !strings.Contains(lines[0], "EXPIRES IN") || !strings.Contains(lines[0], "ACTOR") || !strings.Contains(lines[0], "SUMMARY") {
		t.Fatalf("table:\n%s", out.String())
	}
	for _, want := range []string{"ap1", "1m", "13m", "claude", "deploy api sha256:aaaaaaaaaaaa"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("row lacks %q: %q", want, lines[1])
		}
	}
	out.Reset()
	if code := Run([]string{"approvals", "-e", "dev", "--config", cfg, "--all"}, &out, &errout); code != 0 {
		t.Fatalf("all: exit %d: %s", code, errout.String())
	}
	if seen[1] != "GET /v1/projects/demo/environments/dev/approvals Bearer test-token" {
		t.Fatalf("all request: %q", seen[1])
	}
	for _, want := range []string{"STATUS", "approved", "alice", `secret set \x1b[31mDATABASE_URL`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("all lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if code := Run([]string{"approvals", "-e", "dev", "--config", cfg, "--json"}, &out, &errout); code != 0 || !strings.Contains(out.String(), `"id": "ap1"`) {
		t.Fatalf("json: %d %s", code, out.String())
	}
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `[]`) }))
	defer empty.Close()
	out.Reset()
	if code := Run([]string{"approvals", "-e", "dev", "--config", ingressCLIConfig(t, empty.URL)}, &out, &errout); code != 0 || strings.TrimSpace(out.String()) != "no pending approvals" {
		t.Fatalf("empty: %d %q", code, out.String())
	}
}

func TestApproveShowsThePlanThenExecutesAndStreamsTheOperation(t *testing.T) {
	var mu sync.Mutex
	var order []string
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/projects/demo/environments/dev/approvals":
			note("list")
			fmt.Fprint(w, approvalsListBody)
		case "POST /v1/projects/demo/environments/dev/approvals/ap1/approve":
			note("approve")
			w.Header().Set("Location", "/v1/projects/demo/environments/dev/operations/op1/events")
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"operation_id":"op1","events_url":"/v1/projects/demo/environments/dev/operations/op1/events"}`)
		case "GET /v1/projects/demo/environments/dev/operations/op1/events":
			note("events")
			fmt.Fprint(w, "id: 1\nevent: progress\ndata: {}\n\n")
		case "GET /v1/projects/demo/environments/dev/operations/op1":
			fmt.Fprint(w, `{"state":"succeeded"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	if code := Run([]string{"approve", "ap1", "-e", "dev", "--config", cfg}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if strings.Join(order, ",") != "list,approve,events" {
		t.Fatalf("the plan is shown before the approval, and the operation is followed: %v", order)
	}
	text := out.String()
	for _, want := range []string{"approval: ap1", "deploy api sha256:aaaaaaaaaaaa", "requested by: claude", "plan:", "app: api", "progress", "succeeded"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if strings.Index(text, "plan:") > strings.Index(text, "progress") {
		t.Errorf("the plan comes after the operation:\n%s", text)
	}
}

func TestApproveFailureAndPlainAnswers(t *testing.T) {
	for name, tc := range map[string]struct {
		status   int
		body     string
		wantCode int
		wantErr  string
		wantOut  string
	}{
		"expired":          {409, `{"code":"approval_expired","message":"approval expired"}`, 1, "approval expired", ""},
		"plan changed":     {409, `{"code":"approval_failed","message":"the plan changed since it was requested; ask for a new request"}`, 1, "the plan changed since it was requested", ""},
		"forbidden":        {403, `{"code":"forbidden","message":"an agent token cannot approve or reject"}`, 1, "HTTP 403", ""},
		"not an operation": {200, `{"ok":true}`, 0, "", "ok: true"},
		"a run":            {202, `{"backup_run_id":4}`, 0, "", "backup_run_id: 4"},
	} {
		t.Run(name, func(t *testing.T) {
			h := &heldAgent{approve: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}}
			server := httptest.NewServer(h.handler())
			defer server.Close()
			var out, errout bytes.Buffer
			code := Run([]string{"approve", "ap1", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, &out, &errout)
			if code != tc.wantCode || !strings.Contains(errout.String(), tc.wantErr) || !strings.Contains(out.String(), tc.wantOut) {
				t.Fatalf("exit %d stdout %q stderr %q", code, out.String(), errout.String())
			}
		})
	}
}

func TestRejectSendsTheReasonAndReportsIt(t *testing.T) {
	h := &heldAgent{approve: func(w http.ResponseWriter, r *http.Request) {}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.paths = append(h.paths, r.Method+" "+r.URL.Path)
		h.bodies = append(h.bodies, string(body))
		h.mu.Unlock()
		fmt.Fprint(w, `{"id":"ap1","status":"rejected"}`)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	if code := Run([]string{"reject", "ap1", "-e", "dev", "--config", cfg, "-m", "not during the sale"}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if h.paths[0] != "POST /v1/projects/demo/environments/dev/approvals/ap1/reject" || h.bodies[0] != `{"reason":"not during the sale"}` || !strings.Contains(out.String(), "rejected: ap1") {
		t.Fatalf("request %v %v, output %q", h.paths, h.bodies, out.String())
	}
	out.Reset()
	if code := Run([]string{"reject", "ap1", "-e", "dev", "--config", cfg}, &out, &errout); code != 0 || h.bodies[1] != "" {
		t.Fatalf("without a reason: %d body %q", code, h.bodies[1])
	}
}

func TestApprovalCommandsNeedAnID(t *testing.T) {
	for _, args := range [][]string{{"approve"}, {"reject"}, {"approve", "a", "b"}, {"approvals", "extra"}} {
		var out, errout bytes.Buffer
		if code := Run(append(args, "-e", "dev", "--config", ingressCLIConfig(t, "http://127.0.0.1:1")), &out, &errout); code != 2 {
			t.Errorf("%v: exit %d %s", args, code, errout.String())
		}
	}
	var out, errout bytes.Buffer
	if code := Run([]string{"restart", "api", "--all", "-e", "dev", "--config", ingressCLIConfig(t, "http://127.0.0.1:1")}, &out, &errout); code != 2 {
		t.Errorf("--all outside approvals: exit %d", code)
	}
}
