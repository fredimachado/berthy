package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/api"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/stopwatch"
	"charm.land/bubbles/v2/timer"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const actionToken = "bty.prod.fedcba9876543210"

// request is one request the fake agent got.
type request struct {
	method, path string
	body         map[string]any
}

// fakeAgent answers the requests of the actions and keeps what it was asked.
type fakeAgent struct {
	mu        sync.Mutex
	reqs      []request
	approvals []approvalRow
	history   []historyRow
	// hold answers a change with a held approval; refuse answers it with that status; failOp ends the operation as failed.
	hold      bool
	refuse    int
	refuseMsg string
	failOp    bool
	plan      string
}

func (f *fakeAgent) requests() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.reqs...)
}

// changes are the requests that are not dry runs and not reads.
func (f *fakeAgent) changes() []request {
	var out []request
	for _, r := range f.requests() {
		if r.method == "POST" && r.body["dry_run"] != true {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeAgent) handler(prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		data, _ := io.ReadAll(r.Body)
		if len(data) > 0 {
			_ = json.Unmarshal(data, &body)
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, request{r.Method, strings.TrimPrefix(r.URL.Path, prefix), body})
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+actionToken {
			http.Error(w, `{"message":"unauthorized"}`, 401)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, prefix)
		w.Header().Set("Content-Type", "application/json")
		accepted := func() {
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprintf(w, `{"operation_id":"op1","events_url":"%soperations/op1/events"}`, prefix)
		}
		switch {
		case path == "apps":
			_ = json.NewEncoder(w).Encode(fixtureApps())
		case path == "lock":
			fmt.Fprint(w, `{"locked":false}`)
		case path == "approvals" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(f.approvals)
		case strings.HasPrefix(path, "history/"):
			_ = json.NewEncoder(w).Encode(f.history)
		case r.Method == "POST" && (path == "restart" || path == "rollback"):
			switch {
			case body["dry_run"] == true && f.refuse != 0:
				w.WriteHeader(f.refuse)
				fmt.Fprintf(w, `{"message":%q}`, f.refuseMsg)
			case body["dry_run"] == true:
				plan := f.plan
				if plan == "" {
					plan = `{"app":"api","image":"127.0.0.1:15001/app@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789","roles":["web"]}`
				}
				fmt.Fprint(w, plan)
			case f.hold:
				w.Header().Set("Berthy-Approval", "pending")
				w.WriteHeader(http.StatusAccepted)
				fmt.Fprint(w, `{"approval_id":"held0001","expires":"2026-11-16T12:15:00Z","plan":{}}`)
			case f.refuse != 0:
				w.WriteHeader(f.refuse)
				fmt.Fprintf(w, `{"message":%q}`, f.refuseMsg)
			default:
				accepted()
			}
		case r.Method == "POST" && strings.HasSuffix(path, "/approve"):
			if f.refuse != 0 {
				w.WriteHeader(f.refuse)
				fmt.Fprintf(w, `{"message":%q}`, f.refuseMsg)
				return
			}
			accepted()
		case r.Method == "POST" && strings.HasSuffix(path, "/reject"):
			fmt.Fprint(w, `{"id":"a1b2c3d4e5f6","status":"rejected"}`)
		case path == "operations/op1/events":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "id: 1\nevent: step\ndata: {\"step\":\"updating web\"}\n\n")
			fmt.Fprint(w, "id: 2\nevent: progress\ndata: {\"role\":\"web\"}\n\n")
			if f.failOp {
				fmt.Fprint(w, "event: terminal\ndata: {\"status\":\"rolled_back\",\"operation\":{\"state\":\"rolled_back\",\"reason\":\"replica web became unhealthy\"}}\n\n")
			} else {
				fmt.Fprint(w, "event: terminal\ndata: {\"status\":\"succeeded\",\"operation\":{\"state\":\"succeeded\"}}\n\n")
			}
		default:
			http.Error(w, `{"message":"not found"}`, 404)
		}
	})
}

// actionModel is a model that talks to a fake agent, with the apps, the approvals and a protected environment.
func actionModel(t *testing.T, w, h int) (*Model, *fakeAgent) {
	t.Helper()
	agent := &fakeAgent{approvals: fixtureApprovals(), history: fixtureHistory()}
	agent.approvals[0].Plan = []byte(`{"action":"deploy","app":"api","image":"127.0.0.1:15001/app@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789","roles":["web"]}`)
	agent.history = []historyRow{
		{ID: 3, Digest: "sha256:abcdef0123456789", Commit: "c3", Created: testNow.Add(-10 * time.Minute)},
		{OperationID: "op1", Kind: "deploy", State: "rolled_back", Commit: "cbad", Created: testNow.Add(-40 * time.Minute), Reason: "unhealthy"},
		{ID: 2, Digest: "sha256:0123456789abcdef", Commit: "c2", Created: testNow.Add(-2 * time.Hour)},
		{ID: 1, Digest: "sha256:fedcba9876543210", Commit: "c1", Created: testNow.Add(-26 * time.Hour)},
	}
	prefix := "/v1/environments/prod/"
	srv := httptest.NewServer(agent.handler(prefix))
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	target := &agentclient.Target{Env: "prod", Base: base, Prefix: prefix, Token: actionToken}
	m := NewModel(newClient(ctx, target), "prod", func() time.Time { return testNow })
	m.noAnim = true
	send(m, tea.WindowSizeMsg{Width: w, Height: h}, infoMsg{Environment: "prod", Hostname: "lab-1", Protected: true},
		appsMsg{apps: fixtureApps()}, lockMsg{}, pendingMsg{n: 1}, approvalsMsg{rows: agent.approvals})
	return m, agent
}

// expand lists the commands inside a batch or a sequence.
func expandCmds(msg tea.Msg) []tea.Cmd {
	if b, ok := msg.(tea.BatchMsg); ok {
		return b
	}
	v := reflect.ValueOf(msg)
	if v.Kind() == reflect.Slice && v.Type().Elem() == reflect.TypeOf((tea.Cmd)(nil)) {
		out := make([]tea.Cmd, v.Len())
		for i := range out {
			out[i], _ = v.Index(i).Interface().(tea.Cmd)
		}
		return out
	}
	return nil
}

// settle runs commands and feeds their messages to the model until done says so. The ticks of the animations are dropped, so
// nothing waits for a clock.
func settle(t *testing.T, m *Model, done func() bool, cmds ...tea.Cmd) {
	t.Helper()
	results := make(chan tea.Msg, 256)
	var launch func(c tea.Cmd)
	launch = func(c tea.Cmd) {
		if c != nil {
			go func() { results <- c() }()
		}
	}
	for _, c := range cmds {
		launch(c)
	}
	deadline := time.After(10 * time.Second)
	for {
		wait := time.After(time.Hour)
		if done() {
			// What is still on its way is handled before the test goes on.
			wait = time.After(40 * time.Millisecond)
		}
		select {
		case <-wait:
			return
		case msg := <-results:
			switch msg.(type) {
			case nil, spinner.TickMsg, stopwatch.TickMsg, timer.TickMsg, tickMsg:
				continue
			}
			if inner := expandCmds(msg); inner != nil {
				for _, c := range inner {
					launch(c)
				}
				continue
			}
			_, c := m.Update(msg)
			launch(c)
		case <-deadline:
			t.Fatalf("timed out; overlay %+v\n%s", m.overlay, plain(m))
		}
	}
}

// press sends a key and runs what it starts until done.
func press(t *testing.T, m *Model, done func() bool, name string) {
	t.Helper()
	_, cmd := m.Update(keypress(name))
	settle(t, m, done, cmd)
}

// step sends a key and runs what it starts.
func step(t *testing.T, m *Model, name string) {
	t.Helper()
	press(t, m, func() bool { return true }, name)
}

func typeText(t *testing.T, m *Model, text string) {
	t.Helper()
	for _, r := range text {
		step(t, m, string(r))
	}
}

// pickReady is true when the choice of a release or a role is on screen.
func pickReady(m *Model) func() bool {
	return func() bool {
		o, ok := m.overlay.(*actionOverlay)
		return ok && o.stage == stagePick && o.form != nil
	}
}

func stageIs(m *Model, s actionStage) func() bool {
	return func() bool {
		o, ok := m.overlay.(*actionOverlay)
		return ok && o.stage == s
	}
}

func closed(m *Model) func() bool { return func() bool { return m.overlay == nil } }

func overlayOf(t *testing.T, m *Model) *actionOverlay {
	t.Helper()
	o, ok := m.overlay.(*actionOverlay)
	if !ok {
		t.Fatalf("no action overlay:\n%s", plain(m))
	}
	return o
}

func wantBody(t *testing.T, r request, want map[string]any) {
	t.Helper()
	if !reflect.DeepEqual(r.body, want) {
		t.Fatalf("%s %s body = %v, want %v", r.method, r.path, r.body, want)
	}
}

// noRequestID drops the request id, which is random, and checks that there is one.
func noRequestID(t *testing.T, r request) map[string]any {
	t.Helper()
	body := map[string]any{}
	for k, v := range r.body {
		body[k] = v
	}
	if id, _ := body["request_id"].(string); len(id) != 32 {
		t.Fatalf("request id %v", body["request_id"])
	}
	delete(body, "request_id")
	return body
}

func TestRestartFlow(t *testing.T) {
	m, agent := actionModel(t, 120, 40)
	press(t, m, stageIs(m, stageConfirm), "R")
	out := plain(m)
	for _, want := range []string{"Restart api (web) on prod", "App", "api", "Roles", "web", "Full plan", `"roles"`, "Restart", "Cancel"} {
		if !strings.Contains(out, want) {
			t.Errorf("confirm lacks %q:\n%s", want, out)
		}
	}
	if got := agent.changes(); len(got) != 0 {
		t.Fatalf("a change was sent before the confirmation: %v", got)
	}
	// Cancel is the default: enter alone sends nothing.
	press(t, m, closed(m), "enter")
	if got := agent.changes(); len(got) != 0 {
		t.Fatalf("a change was sent by cancelling: %v", got)
	}

	press(t, m, stageIs(m, stageConfirm), "R")
	press(t, m, stageIs(m, stageDone), "y")
	reqs := agent.requests()
	var posts []request
	for _, r := range reqs {
		if r.method == "POST" {
			posts = append(posts, r)
		}
	}
	if len(posts) != 3 {
		t.Fatalf("posts: %v", posts)
	}
	wantBody(t, posts[0], map[string]any{"app": "api", "dry_run": true, "request_id": posts[0].body["request_id"]})
	wantBody(t, posts[2], map[string]any{"app": "api", "dry_run": false, "request_id": posts[2].body["request_id"]})
	if posts[1].path != "restart" || posts[2].path != "restart" {
		t.Fatalf("paths %v", posts)
	}
	if posts[2].body["request_id"] == posts[0].body["request_id"] || len(posts[2].body["request_id"].(string)) != 32 {
		t.Fatalf("the change reuses the id of the dry run: %v", posts)
	}
	o := overlayOf(t, m)
	if o.result.kind != outOK || !strings.HasPrefix(o.result.title, "Restarted api") {
		t.Fatalf("result %+v", o.result)
	}
	out = plain(m)
	for _, want := range []string{"Restarted api", "updating web", "any key close"} {
		if !strings.Contains(out, want) {
			t.Errorf("done lacks %q:\n%s", want, out)
		}
	}
	press(t, m, closed(m), "x")
	// The model reads everything again: the finished action asked for the apps, lock, pending and approvals.
	seen := map[string]bool{}
	for _, r := range agent.requests() {
		if r.method == "GET" {
			seen[r.path] = true
		}
	}
	for _, p := range []string{"apps", "lock", "approvals"} {
		if !seen[p] {
			t.Errorf("%s was not read again after the action: %v", p, seen)
		}
	}
}

func TestEscSendsNothing(t *testing.T) {
	m, agent := actionModel(t, 120, 40)
	m.Update(keypress("R"))
	// At the plan stage: esc closes before the plan arrives.
	m.Update(keypress("esc"))
	if m.overlay != nil {
		t.Fatal("esc did not close the overlay at the plan")
	}
	press(t, m, stageIs(m, stageConfirm), "R")
	press(t, m, closed(m), "esc")
	for _, r := range agent.requests() {
		if r.method == "POST" && r.body["dry_run"] != true {
			t.Fatalf("a change was sent: %v", r)
		}
	}
}

func TestRestartPicksARole(t *testing.T) {
	m, agent := actionModel(t, 120, 40)
	apps := fixtureApps()
	apps[0].Roles = append(apps[0].Roles, api.Role{Name: "worker", Desired: u64(1), Running: 1})
	send(m, appsMsg{apps: apps})
	press(t, m, pickReady(m), "R")
	if out := plain(m); !strings.Contains(out, "Restart which role?") || !strings.Contains(out, "web") || !strings.Contains(out, "worker") {
		t.Fatalf("no role picker:\n%s", out)
	}
	step(t, m, "down")
	press(t, m, stageIs(m, stageConfirm), "enter")
	press(t, m, stageIs(m, stageDone), "y")
	for _, r := range agent.requests() {
		if r.method == "POST" && r.body["role"] != "worker" {
			t.Fatalf("request without the chosen role: %v", r)
		}
	}
	if !strings.Contains(overlayOf(t, m).title(), "(worker)") {
		t.Fatalf("title %q", overlayOf(t, m).title())
	}
}

func TestRollbackListsEarlierSuccessfulReleases(t *testing.T) {
	m, agent := actionModel(t, 120, 40)
	step(t, m, "enter")
	if m.detail == nil {
		t.Fatal("no detail")
	}
	press(t, m, pickReady(m), "b")
	out := plain(m)
	// Only the earlier successful releases: not the current one (3) and not the failed deploy.
	if !strings.Contains(out, "release 2") || !strings.Contains(out, "release 1") || strings.Contains(out, "release 3") || strings.Contains(out, "unhealthy") {
		t.Fatalf("releases:\n%s", out)
	}
	step(t, m, "down")
	press(t, m, stageIs(m, stageConfirm), "enter")
	o := overlayOf(t, m)
	if o.release != 1 {
		t.Fatalf("release %d", o.release)
	}
	// The environment is protected: confirming needs its name.
	if out = plain(m); !strings.Contains(out, "Type the environment name") {
		t.Fatalf("no typed name:\n%s", out)
	}
	for _, r := range agent.requests() {
		if r.method == "POST" {
			wantBody(t, r, map[string]any{"app": "api", "target_release": float64(1), "dry_run": true, "request_id": r.body["request_id"]})
		}
	}
	// A wrong name does not get to the confirmation.
	typeText(t, m, "pro")
	step(t, m, "enter")
	if f := o.form.GetFocusedField(); f == nil || f.GetKey() != "env" {
		t.Fatalf("a wrong name moved on")
	}
	if !strings.Contains(plain(m), "must match exactly") {
		t.Fatalf("no error:\n%s", plain(m))
	}
	// Neither enter nor a confirmation key has sent anything.
	if o.stage != stageConfirm || len(agent.changes()) != 0 {
		t.Fatalf("a wrong name confirmed: stage %v, changes %v", o.stage, agent.changes())
	}
	typeText(t, m, "d")
	step(t, m, "enter")
	if f := o.form.GetFocusedField(); f.GetKey() != "sure" {
		t.Fatalf("the right name did not move to the confirmation")
	}
	press(t, m, stageIs(m, stageDone), "y")
	ch := agent.changes()
	if len(ch) != 1 || ch[0].path != "rollback" {
		t.Fatalf("changes %v", ch)
	}
	wantBody(t, ch[0], map[string]any{"app": "api", "target_release": float64(1), "dry_run": false, "request_id": ch[0].body["request_id"]})
	if o.result.kind != outOK || !strings.HasPrefix(o.result.title, "Rolled back api to release 1") {
		t.Fatalf("result %+v", o.result)
	}
}

func TestTypedNameIsCaseSensitive(t *testing.T) {
	m, agent := actionModel(t, 120, 40)
	step(t, m, "enter")
	press(t, m, pickReady(m), "b")
	press(t, m, stageIs(m, stageConfirm), "enter")
	typeText(t, m, "Prod")
	step(t, m, "enter")
	o := overlayOf(t, m)
	if f := o.form.GetFocusedField(); f.GetKey() != "env" {
		t.Fatal("Prod accepted for prod")
	}
	if len(agent.changes()) != 0 {
		t.Fatal("a change was sent")
	}
}

func TestHeldRequestShowsTheApprovalID(t *testing.T) {
	m, agent := actionModel(t, 120, 40)
	agent.hold = true
	press(t, m, stageIs(m, stageConfirm), "R")
	press(t, m, stageIs(m, stageDone), "y")
	o := overlayOf(t, m)
	if o.result.kind != outHeld {
		t.Fatalf("result %+v", o.result)
	}
	out := plain(m)
	if !strings.Contains(out, "Held for approval held0001") || !strings.Contains(out, "a person must approve it") {
		t.Fatalf("held:\n%s", out)
	}
	press(t, m, closed(m), "q")
}

func TestForbiddenShowsTheScopeHint(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"restart", "this token needs the deploy scope"},
		{"approve", "this token needs the admin scope"},
	} {
		m, agent := actionModel(t, 120, 40)
		agent.refuse, agent.refuseMsg = 403, "forbidden"
		if tc.name == "restart" {
			press(t, m, stageIs(m, stageDone), "R")
		} else {
			m.Update(keypress("4"))
			press(t, m, stageIs(m, stageConfirm), "a")
			typeText(t, m, "prod")
			step(t, m, "enter")
			press(t, m, stageIs(m, stageDone), "y")
		}
		out := plain(m)
		if !strings.Contains(out, tc.want) || !strings.Contains(out, "agent returned HTTP 403: forbidden") {
			t.Errorf("%s: no hint:\n%s", tc.name, out)
		}
	}
}

func TestRefusalIsRedacted(t *testing.T) {
	m, agent := actionModel(t, 120, 40)
	agent.refuse, agent.refuseMsg = 409, "cannot use "+actionToken+" here"
	press(t, m, stageIs(m, stageDone), "R")
	out := plain(m)
	if strings.Contains(out, actionToken) || !strings.Contains(out, "[redacted]") {
		t.Fatalf("token in the refusal:\n%s", out)
	}
}

func TestFailedOperationShowsReasonAndDiagnostics(t *testing.T) {
	m, agent := actionModel(t, 120, 40)
	agent.failOp = true
	press(t, m, stageIs(m, stageConfirm), "R")
	press(t, m, stageIs(m, stageDone), "y")
	o := overlayOf(t, m)
	if o.result.kind != outFailed {
		t.Fatalf("result %+v", o.result)
	}
	out := plain(m)
	for _, want := range []string{"rolled back", "replica web became unhealthy", "berthy diagnostics op1 -e prod", "d open api"} {
		if !strings.Contains(out, want) {
			t.Errorf("failure lacks %q:\n%s", want, out)
		}
	}
	// d opens the app.
	press(t, m, func() bool { return m.overlay == nil && m.detail != nil }, "d")
	if m.detail.app.Ref.Name != "api" {
		t.Fatalf("detail of %s", m.detail.app.Ref.Name)
	}
}

func TestEscDuringFollowLeavesANote(t *testing.T) {
	m, _ := actionModel(t, 120, 40)
	press(t, m, stageIs(m, stageConfirm), "R")
	o := overlayOf(t, m)
	// Follow the operation by hand, so the events do not end it.
	o.stage, o.opID = stageFollow, "op1"
	_, cmd := m.Update(keypress("esc"))
	settle(t, m, func() bool { return m.status.note != "" }, cmd)
	if m.overlay != nil || !strings.Contains(m.status.note, "operation op1 continues on the agent") {
		t.Fatalf("overlay %v note %q", m.overlay, m.status.note)
	}
	if out := plain(m); !strings.Contains(out, "operation op1 continues on the agent") {
		t.Fatalf("no note on the status line:\n%s", out)
	}
}

func TestApproveFollowsTheOperation(t *testing.T) {
	m, agent := actionModel(t, 120, 40)
	m.Update(keypress("4"))
	if m.active != tabApprovals {
		t.Fatal("not on the approvals tab")
	}
	press(t, m, stageIs(m, stageConfirm), "a")
	out := plain(m)
	for _, want := range []string{"Approve deploy api", "requested by claude", "12:00 left to decide", "Full plan", "Type the environment name"} {
		if !strings.Contains(out, want) {
			t.Errorf("approve lacks %q:\n%s", want, out)
		}
	}
	typeText(t, m, "prod")
	step(t, m, "enter")
	press(t, m, stageIs(m, stageDone), "y")
	ch := agent.changes()
	if len(ch) != 1 || ch[0].path != "approvals/a1b2c3d4e5f6/approve" {
		t.Fatalf("changes %v", ch)
	}
	if o := overlayOf(t, m); o.result.kind != outOK || !strings.HasPrefix(o.result.title, "Deployed api") {
		t.Fatalf("result %+v", o.result)
	}
	// Once the approval is sent there is nothing left to decide: no countdown, but who asked stays.
	out = plain(m)
	if strings.Contains(out, "left to decide") || !strings.Contains(out, "requested by claude") {
		t.Fatalf("countdown after the approval was sent:\n%s", out)
	}
	o := overlayOf(t, m)
	o.stage = stageFollow
	if out = plain(m); strings.Contains(out, "left to decide") {
		t.Fatalf("countdown while following:\n%s", out)
	}
}

func TestApproveRefusedWhenNoLongerPending(t *testing.T) {
	m, agent := actionModel(t, 120, 40)
	m.Update(keypress("4"))
	agent.approvals[0].Status = "approved"
	press(t, m, stageIs(m, stageDone), "a")
	out := plain(m)
	if !strings.Contains(out, "No longer pending") || !strings.Contains(out, "already approved") {
		t.Fatalf("not refused:\n%s", out)
	}
	if len(agent.changes()) != 0 {
		t.Fatalf("a change was sent: %v", agent.changes())
	}
}

func TestRejectSendsTheReason(t *testing.T) {
	m, agent := actionModel(t, 120, 40)
	m.Update(keypress("4"))
	press(t, m, stageIs(m, stageConfirm), "x")
	if out := plain(m); !strings.Contains(out, "Reason (optional)") || strings.Contains(out, "Type the environment name") {
		t.Fatalf("reject form:\n%s", out)
	}
	typeText(t, m, "not now, freeze")
	step(t, m, "enter")
	press(t, m, stageIs(m, stageDone), "y")
	ch := agent.changes()
	if len(ch) != 1 || ch[0].path != "approvals/a1b2c3d4e5f6/reject" {
		t.Fatalf("changes %v", ch)
	}
	wantBody(t, ch[0], map[string]any{"reason": "not now, freeze"})
	if o := overlayOf(t, m); o.result.kind != outOK || !strings.HasPrefix(o.result.title, "Rejected") {
		t.Fatalf("result %+v", o.result)
	}
}

func TestActionKeysInTheHelp(t *testing.T) {
	m, _ := actionModel(t, 160, 40)
	if out := plain(m); !strings.Contains(out, "R restart") || !strings.Contains(out, "b roll back") || strings.Contains(out, "a approve") {
		t.Fatalf("apps help:\n%s", out)
	}
	m.Update(keypress("3"))
	m.shutdown()
	if out := plain(m); strings.Contains(out, "R restart") {
		t.Fatalf("restart on the server tab:\n%s", out)
	}
	m.Update(keypress("?"))
	if out := plain(m); !strings.Contains(out, "R restart") || !strings.Contains(out, "x reject") {
		t.Fatalf("full help lacks the actions:\n%s", out)
	}
	m.Update(keypress("?"))
	m.Update(keypress("4"))
	if out := plain(m); !strings.Contains(out, "a approve") || !strings.Contains(out, "x reject") {
		t.Fatalf("approvals help:\n%s", out)
	}
	// Only a pending request can be decided.
	step(t, m, "down")
	if out := plain(m); strings.Contains(out, "a approve") {
		t.Fatalf("approve offered on a decided request:\n%s", out)
	}
	m.Update(keypress("a"))
	if m.overlay != nil {
		t.Fatal("the overlay opened on a decided request")
	}
}

func TestOverlayTakesEveryKey(t *testing.T) {
	m, agent := actionModel(t, 120, 40)
	press(t, m, stageIs(m, stageConfirm), "R")
	m.Update(keypress("tab"))
	m.Update(keypress("4"))
	m.Update(keypress("q"))
	if m.active != tabApps || m.overlay == nil {
		t.Fatalf("a key went through the overlay: tab %v", m.active)
	}
	_ = agent
}

func TestTokenNeverInTheOverlay(t *testing.T) {
	m, agent := actionModel(t, 120, 40)
	agent.plan = `{"app":"api","note":"` + actionToken + `","roles":["web"]}`
	press(t, m, stageIs(m, stageConfirm), "R")
	if out := plain(m); strings.Contains(out, actionToken) || strings.Contains(m.View().Content, actionToken) {
		t.Fatalf("token in the confirmation:\n%s", out)
	}
	press(t, m, stageIs(m, stageDone), "y")
	if strings.Contains(m.View().Content, actionToken) {
		t.Fatal("token in the result")
	}
}

func TestPlanMarkdownKeepsAgentTextInCodeSpans(t *testing.T) {
	facts := planFacts([]byte("{\"app\":\"api *x* [link](http://evil) <b>\",\"roles\":[\"a_b\",\"`tick`\"],\"image\":\"x\\u001b[31m@sha256:00\"}"))
	doc := planDocument("Restart", facts, "", []byte("{\"a\":\"```\"}"), "", 80)
	if !strings.Contains(doc, "`api *x* [link](http://evil) <b>`") || strings.Contains(doc, "\x1b") {
		t.Errorf("agent text is not in a code span:\n%s", doc)
	}
	if !strings.Contains(doc, "````json") {
		t.Errorf("the fence can be closed by the plan:\n%s", doc)
	}
	out := strings.Join(renderMarkdown(newStyles(newPalette(true)), doc, 80), "\n")
	if text := stripANSIForTest(out); !strings.Contains(text, "api *x* [link](http://evil) <b>") {
		t.Errorf("the text changed on its way to the screen:\n%s", text)
	}
}

func TestNetworkErrorRetriesWithTheSameID(t *testing.T) {
	m, agent := actionModel(t, 120, 40)
	press(t, m, stageIs(m, stageConfirm), "R")
	o := overlayOf(t, m)
	// The agent goes away after the confirmation.
	origHost := m.c.target.Base.Host
	m.c.target.Base.Host = "127.0.0.1:1"
	press(t, m, stageIs(m, stageDone), "y")
	if !o.result.retry || !strings.Contains(plain(m), "cannot be reached") {
		t.Fatalf("result %+v", o.result)
	}
	id := o.reqID
	m.c.target.Base.Host = origHost
	press(t, m, stageIs(m, stageDone), "r")
	settle(t, m, func() bool { return o.stage == stageDone && o.result.kind == outOK })
	ch := agent.changes()
	if len(ch) != 1 || ch[0].body["request_id"] != id {
		t.Fatalf("the retry changed the id: %v want %s", ch, id)
	}
}

func actionGoldenModel(t *testing.T, w, h int) (*Model, *fakeAgent) {
	m, agent := actionModel(t, w, h)
	return m, agent
}

func TestActionGoldens(t *testing.T) {
	for _, s := range sizes {
		// restart
		m, _ := actionGoldenModel(t, s.w, s.h)
		press(t, m, stageIs(m, stageConfirm), "R")
		golden(t, "action-restart-"+s.name, plain(m))
		checkExact(t, m, s.w, s.h)

		// rollback on a protected environment, with the typed name
		m, _ = actionGoldenModel(t, s.w, s.h)
		step(t, m, "enter")
		press(t, m, pickReady(m), "b")
		golden(t, "action-rollback-pick-"+s.name, plain(m))
		press(t, m, stageIs(m, stageConfirm), "enter")
		typeText(t, m, "pro")
		golden(t, "action-rollback-"+s.name, plain(m))
		checkExact(t, m, s.w, s.h)

		// approve
		m, _ = actionGoldenModel(t, s.w, s.h)
		m.Update(keypress("4"))
		press(t, m, stageIs(m, stageConfirm), "a")
		golden(t, "action-approve-"+s.name, plain(m))
		checkExact(t, m, s.w, s.h)

		// follow, and done
		m, _ = actionGoldenModel(t, s.w, s.h)
		press(t, m, stageIs(m, stageConfirm), "R")
		o := overlayOf(t, m)
		o.stage, o.opID, o.form = stageFollow, "8f3a2b1c", nil
		o.events = []string{"step: pulling the image", "updating web", "web replica 1 healthy"}
		golden(t, "action-follow-"+s.name, plain(m))
		checkExact(t, m, s.w, s.h)
		o.operationDone(opDoneMsg{id: o.id, state: "succeeded"})
		golden(t, "action-done-"+s.name, plain(m))
		o.operationDone(opDoneMsg{id: o.id, state: "rolled_back", reason: "replica web became unhealthy: health check failed 3 times"})
		golden(t, "action-failed-"+s.name, plain(m))
		checkExact(t, m, s.w, s.h)
	}
}

// checkExact checks that every line of the view is exactly w cells wide and there are h of them.
func checkExact(t *testing.T, m *Model, w, h int) {
	t.Helper()
	lines := strings.Split(plain(m), "\n")
	if len(lines) != h {
		t.Errorf("%d lines, want %d", len(lines), h)
	}
	for i, l := range lines {
		if got := lipgloss.Width(l); got != w {
			t.Errorf("line %d is %d cells, want %d: %q", i, got, w, l)
		}
	}
}

func TestSendRefusesAnEventsURLTheAgentDoesNotOwn(t *testing.T) {
	for _, events := range []string{"/v1/environments/other/operations/op1/events", "//evil.example/operations/op1/events", "/v1/environments/prod/operations/op1/../op2/events"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprintf(w, `{"operation_id":"op1","events_url":%q}`, events)
		}))
		base, _ := url.Parse(srv.URL)
		c := newClient(t.Context(), &agentclient.Target{Env: "prod", Base: base, Prefix: "/v1/environments/prod/", Token: actionToken})
		msg := c.send(7, "restart", map[string]any{"app": "api"})()
		srv.Close()
		if e, ok := msg.(actionErrMsg); !ok || e.id != 7 || !strings.Contains(e.text, "invalid operation events URL") {
			t.Errorf("%s: %#v", events, msg)
		}
	}
}

func TestFollowReconnectsWithoutShowingEventsTwice(t *testing.T) {
	var mu sync.Mutex
	streams := 0
	prefix := "/v1/environments/prod/"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case prefix + "operations/op1/events":
			streams++
			fmt.Fprint(w, "event: step\ndata: {\"step\":\"one\"}\n\n")
			if streams > 1 {
				fmt.Fprint(w, "event: step\ndata: {\"step\":\"two\"}\n\n")
				fmt.Fprint(w, "event: terminal\ndata: {\"status\":\"succeeded\",\"operation\":{\"state\":\"succeeded\"}}\n\n")
			}
		case prefix + "operations/op1":
			fmt.Fprint(w, `{"state":"running"}`)
		}
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	c := newClient(t.Context(), &agentclient.Target{Env: "prod", Base: base, Prefix: prefix, Token: actionToken})
	var got []tea.Msg
	c.followOperation(t.Context(), 3, "op1", prefix+"operations/op1/events", func(m tea.Msg) { got = append(got, m) })
	if len(got) != 3 || got[0] != (opEventMsg{id: 3, text: "one"}) || got[1] != (opEventMsg{id: 3, text: "two"}) || got[2] != (opDoneMsg{id: 3, state: "succeeded"}) {
		t.Fatalf("messages %#v", got)
	}
}

func TestFollowEndsWhenTheStreamEndsAndTheOperationDid(t *testing.T) {
	prefix := "/v1/environments/prod/"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == prefix+"operations/op1" {
			fmt.Fprint(w, `{"state":"failed","reason":"boom"}`)
		}
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	c := newClient(t.Context(), &agentclient.Target{Env: "prod", Base: base, Prefix: prefix, Token: actionToken})
	var got []tea.Msg
	c.followOperation(t.Context(), 3, "op1", prefix+"operations/op1/events", func(m tea.Msg) { got = append(got, m) })
	if len(got) != 1 || got[0] != (opDoneMsg{id: 3, state: "failed", reason: "boom"}) {
		t.Fatalf("messages %#v", got)
	}
}

func TestImageFactsShowTheRepositoryAndTheWholeDigest(t *testing.T) {
	facts := planFacts([]byte(`{"app":"api","image":"127.0.0.1:15001/berthy-schema/app@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"}`))
	var image, digest *planFact
	for i := range facts {
		switch facts[i].label {
		case "Image":
			image = &facts[i]
		case "Digest":
			digest = &facts[i]
		}
	}
	if image == nil || image.md != "`127.0.0.1:15001/berthy-schema/app`" {
		t.Fatalf("image fact %+v", image)
	}
	if digest == nil || !strings.Contains(digest.md, "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789") || digest.short == "" {
		t.Fatalf("digest fact %+v", digest)
	}
	if got := planFacts([]byte(`{"image":"registry.example/app:v3@sha256:00aa"}`)); got[0].md != "`registry.example/app:v3`" {
		t.Fatalf("a tag is lost: %+v", got)
	}
	if got := planFacts([]byte(`{"image":"alpine"}`)); len(got) != 1 || got[0].md != "`alpine`" {
		t.Fatalf("an image without a digest: %+v", got)
	}
}

func TestRollbackPlanShowsFromAndTo(t *testing.T) {
	m, _ := actionModel(t, 120, 40)
	step(t, m, "enter")
	press(t, m, pickReady(m), "b")
	press(t, m, stageIs(m, stageConfirm), "enter")
	out := plain(m)
	if !strings.Contains(out, "From release 3 · sha256:abcdef01 · commit c3 · 10m ago") || !strings.Contains(out, "To release 2 · sha256:01234567 · commit c2 · 2h ago") {
		t.Fatalf("no from and to:\n%s", out)
	}
	if strings.Index(out, "From release") > strings.Index(out, "To release") {
		t.Fatalf("From must come before To:\n%s", out)
	}
	// Without a current release there is no From.
	o := overlayOf(t, m)
	o.current, o.stage = nil, stagePlan
	o.planned(o.planRaw)
	for _, f := range o.planFacts {
		if f.label == "From" {
			t.Fatalf("a From without a current release: %+v", f)
		}
	}
}

func TestFooterIsEmptyUnderTheOverlay(t *testing.T) {
	m, _ := actionModel(t, 120, 40)
	before := strings.Split(plain(m), "\n")
	press(t, m, stageIs(m, stageConfirm), "R")
	lines := strings.Split(plain(m), "\n")
	if len(lines) != len(before) {
		t.Fatalf("the layout moved: %d lines, was %d", len(lines), len(before))
	}
	if last := lines[len(lines)-1]; strings.TrimSpace(last) != "" || strings.Contains(plain(m), "R restart") {
		t.Fatalf("keys of the views under the overlay: %q", last)
	}
	press(t, m, closed(m), "esc")
	if out := plain(m); !strings.Contains(out, "R restart") {
		t.Fatalf("the footer did not come back:\n%s", out)
	}
}

func TestRestartTitleNamesNoScopeBeforeThePlan(t *testing.T) {
	m, agent := actionModel(t, 120, 40)
	agent.refuse, agent.refuseMsg = 403, "forbidden"
	press(t, m, stageIs(m, stageDone), "R")
	if title := overlayOf(t, m).title(); title != "Restart api on prod" {
		t.Fatalf("title %q", title)
	}
	step(t, m, "enter")
	// A Compose app restarts as a whole.
	step(t, m, "down")
	if app, _ := m.SelectedApp(); app.Ref.Kind != "compose" {
		t.Fatalf("selected %s", app.Ref.Name)
	}
	press(t, m, stageIs(m, stageDone), "R")
	if title := overlayOf(t, m).title(); title != "Restart blog (all roles) on prod" {
		t.Fatalf("title %q", title)
	}
}

func TestNoteAt80ColumnsKeepsLockAndPending(t *testing.T) {
	m, _ := actionModel(t, 80, 24)
	apps := fixtureApps()[:1]
	apps[0].Backups = nil
	send(m, appsMsg{apps: apps})
	press(t, m, stageIs(m, stageConfirm), "R")
	o := overlayOf(t, m)
	o.stage, o.opID = stageFollow, "8aa5c01aad7bc703851756d6d32b5184"
	// The full id is where there is room.
	if !strings.Contains(plain(m), "operation 8aa5c01aad7bc703851756d6d32b5184") {
		t.Fatalf("the overlay lacks the full id:\n%s", plain(m))
	}
	_, cmd := m.Update(keypress("esc"))
	settle(t, m, func() bool { return m.status.note != "" }, cmd)
	lines := strings.Split(plain(m), "\n")
	status := lines[len(lines)-2]
	for _, want := range []string{"operation 8aa5c01a continues on the agent", "unlocked", "1 pending"} {
		if !strings.Contains(status, want) {
			t.Errorf("status line lacks %q: %q", want, status)
		}
	}
	if strings.Contains(status, "8aa5c01aa") {
		t.Errorf("the note has more than the short id: %q", status)
	}
}
