package tui

import (
	"image/color"
	"net/http"
	"strings"
	"testing"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/logs"
	"berthy/internal/metrics"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

var sizes = []struct {
	name string
	w, h int
}{{"80x24", 80, 24}, {"120x40", 120, 40}}

func TestAppsViewGolden(t *testing.T) {
	for _, s := range sizes {
		m := loadedModel(t, s.w, s.h)
		golden(t, "apps-"+s.name, plain(m))
	}
}

func TestTheStartupViewSaysToUpgradeAnOlderAgent(t *testing.T) {
	m := newTestModel(t, 80, 24)
	send(m, errMsg{src: "apps", text: agentclient.Message(http.StatusNotFound, []byte(`{"message":"`+agentclient.OlderAgentMessage+`"}`))})
	out := plain(m)
	if !strings.Contains(out, "berthy setup") {
		t.Fatalf("startup view at 80x24 lacks berthy setup:\n%s", out)
	}
}

func TestTheHeaderNamesProjectAndEnvironment(t *testing.T) {
	m := loadedModel(t, 120, 40)
	if got := m.View().WindowTitle; got != "berthy · demo/prod" {
		t.Fatalf("title %q", got)
	}
	out := plain(m)
	if !strings.Contains(out, "demo/prod") {
		t.Fatalf("header lacks demo/prod:\n%s", out)
	}
}

func TestTheHeaderFitsALongTargetAndKeepsProtected(t *testing.T) {
	project, env := strings.Repeat("a", 40), strings.Repeat("b", 18)
	m := newTestModel(t, 80, 24)
	m.project, m.env = project, env
	send(m, infoMsg{Hostname: "lab-1", Protected: true})
	header := strings.Split(plain(m), "\n")[0]
	if w := lipgloss.Width(header); w != 80 {
		t.Fatalf("header width %d:\n%s", w, header)
	}
	if !strings.Contains(header, "protected") {
		t.Fatalf("protected cut:\n%s", header)
	}
	if strings.Contains(header, "lab-1") {
		t.Fatalf("hostname kept while the target and protection needed the room:\n%s", header)
	}
	if got := m.View().WindowTitle; got != "berthy · "+project+"/"+env {
		t.Fatalf("title %q", got)
	}
}

func TestAppsFilter(t *testing.T) {
	m := loadedModel(t, 120, 40)
	keys(m, "/", "w", "o")
	if !m.apps.filtering || len(m.apps.shown) != 1 {
		t.Fatalf("filtering=%v shown=%v", m.apps.filtering, m.apps.shown)
	}
	golden(t, "apps-filter-120x40", plain(m))
	keys(m, "enter")
	if m.apps.filtering || m.apps.query != "wo" || len(m.apps.shown) != 1 {
		t.Fatalf("filter not applied: %+v", m.apps.query)
	}
	keys(m, "esc")
	if m.apps.query != "" || len(m.apps.shown) != 3 {
		t.Fatalf("filter not cleared")
	}
	// A filter that matches nothing says so.
	keys(m, "/", "z", "z", "z")
	if !strings.Contains(plain(m), "No app matches") {
		t.Fatalf("missing empty-filter message:\n%s", plain(m))
	}
}

func TestAppsCursorStaysOnApp(t *testing.T) {
	m := loadedModel(t, 120, 40)
	keys(m, "down")
	sel, _ := m.apps.selected()
	if sel.Ref.Name != "blog" && sel.Ref.Name != "worker" {
		t.Fatalf("unexpected selection %s", sel.Ref.Name)
	}
	name := sel.Ref.Name
	apps := fixtureApps()
	apps[0], apps[2] = apps[2], apps[0]
	send(m, appsMsg{apps: apps})
	sel, _ = m.apps.selected()
	if sel.Ref.Name != name {
		t.Fatalf("selection moved from %s to %s", name, sel.Ref.Name)
	}
}

func TestDetailGolden(t *testing.T) {
	for _, s := range sizes {
		m := loadedModel(t, s.w, s.h)
		keys(m, "enter")
		if m.detail == nil || m.detail.app.Ref.Name != "api" {
			t.Fatalf("detail not opened")
		}
		send(m, historyMsg{app: "api", rows: fixtureHistory()})
		golden(t, "detail-"+s.name, plain(m))
	}
	m := loadedModel(t, 120, 40)
	keys(m, "down", "enter")
	send(m, historyMsg{app: m.detail.path, rows: fixtureHistory()})
	golden(t, "detail-worker-120x40", plain(m))
	keys(m, "esc")
	if m.detail != nil {
		t.Fatal("esc did not close the detail")
	}
}

func TestDetailFailedDeployShowsReason(t *testing.T) {
	m := loadedModel(t, 120, 40)
	keys(m, "enter")
	send(m, historyMsg{app: "api", rows: fixtureHistory()})
	out := plain(m)
	for _, want := range []string{"Recent deploys", "rolled back", "health check failed 3 times", "commit cbad", "restarts 2", "cleanup", "main-db"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail lacks %q", want)
		}
	}
}

func TestTabs(t *testing.T) {
	m := loadedModel(t, 120, 40)
	keys(m, "tab")
	if m.active != tabLogs {
		t.Fatalf("tab -> %v", m.active)
	}
	keys(m, "shift+tab")
	if m.active != tabApps {
		t.Fatalf("shift+tab -> %v", m.active)
	}
	keys(m, "3")
	if m.active != tabServer {
		t.Fatalf("3 -> %v", m.active)
	}
	m.shutdown()
	keys(m, "4")
	if m.active != tabApprovals {
		t.Fatalf("4 -> %v", m.active)
	}
	keys(m, "1")
	if m.active != tabApps {
		t.Fatalf("1 -> %v", m.active)
	}
}

func TestQuit(t *testing.T) {
	for _, k := range []string{"q", "ctrl+c"} {
		m := loadedModel(t, 120, 40)
		_, cmd := m.Update(keypress(k))
		if cmd == nil {
			t.Fatalf("%s gave no command", k)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("%s did not quit", k)
		}
	}
}

func TestHelpToggle(t *testing.T) {
	m := loadedModel(t, 120, 40)
	short := plain(m)
	keys(m, "?")
	full := plain(m)
	if !m.help.ShowAll || strings.Count(full, "\n") != strings.Count(short, "\n") {
		t.Fatalf("full help changed the height or did not turn on")
	}
	if !strings.Contains(full, "go to tab") {
		t.Fatalf("full help lacks the tab keys:\n%s", full)
	}
	golden(t, "help-120x40", full)
	keys(m, "?")
	if m.help.ShowAll {
		t.Fatal("? did not turn help off")
	}
}

func TestTooSmall(t *testing.T) {
	m := loadedModel(t, 40, 10)
	if !strings.Contains(plain(m), "needs a terminal of at least") {
		t.Fatalf("no size notice:\n%s", plain(m))
	}
}

func TestViewHasExactSize(t *testing.T) {
	for _, s := range []struct{ w, h int }{{60, 14}, {80, 24}, {100, 30}, {120, 40}, {200, 60}} {
		m := loadedModel(t, s.w, s.h)
		m.shutdown()
		for _, tb := range []string{"1", "3", "4", "2"} {
			keys(m, tb)
			lines := strings.Split(plain(m), "\n")
			if len(lines) != s.h {
				t.Errorf("%dx%d tab %s: %d lines", s.w, s.h, tb, len(lines))
			}
			for i, l := range lines {
				if w := lipgloss.Width(l); w != s.w {
					t.Errorf("%dx%d tab %s line %d is %d cells wide: %q", s.w, s.h, tb, i, w, l)
				}
			}
			m.shutdown()
		}
	}
}

func TestStatusLine(t *testing.T) {
	m := loadedModel(t, 120, 40)
	out := plain(m)
	for _, want := range []string{"locked by ci: migrating", "1 pending", "backup failed", "connected"} {
		if !strings.Contains(out, want) {
			t.Errorf("status line lacks %q:\n%s", want, out)
		}
	}
	send(m, lockMsg{Locked: false})
	if !strings.Contains(plain(m), "unlocked") {
		t.Error("unlocked not shown")
	}
}

func TestUnreachableKeepsData(t *testing.T) {
	m := loadedModel(t, 120, 40)
	send(m, errMsg{src: "apps", text: "agent unreachable", network: true})
	out := plain(m)
	if !strings.Contains(out, "agent unreachable — retrying in 1s") {
		t.Fatalf("no unreachable notice:\n%s", out)
	}
	if !strings.Contains(out, "worker") {
		t.Fatal("the last data left the screen")
	}
	golden(t, "unreachable-120x40", out)
	// Backoff grows.
	send(m, errMsg{src: "apps", text: "agent unreachable", network: true})
	if !m.status.retryAt.Equal(testNow.Add(2 * time.Second)) {
		t.Fatalf("retryAt %v", m.status.retryAt)
	}
	// The next success clears the state.
	send(m, appsMsg{apps: fixtureApps()})
	if m.status.unreachable || strings.Contains(plain(m), "unreachable") {
		t.Fatal("did not recover")
	}
}

func TestRefusalGoesToStatusLine(t *testing.T) {
	m := loadedModel(t, 120, 40)
	send(m, errMsg{src: "approvals", text: "agent returned HTTP 403: forbidden"})
	if !strings.Contains(plain(m), "HTTP 403") || m.status.unreachable {
		t.Fatal("refusal not shown as a refusal")
	}
	send(m, approvalsMsg{rows: fixtureApprovals()})
	if strings.Contains(plain(m), "HTTP 403") {
		t.Fatal("refusal not cleared")
	}
}

func TestPollSchedule(t *testing.T) {
	m := loadedModel(t, 120, 40)
	m.status.inflight = map[string]bool{}
	for _, k := range []string{"apps", "lock", "pending", "info"} {
		m.status.fetched[k] = testNow
	}
	m.poll(testNow.Add(time.Second))
	if len(m.status.inflight) != 0 {
		t.Fatalf("something polled too early: %v", m.status.inflight)
	}
	m.status.fetched["apps"] = testNow
	m.status.fetched["lock"] = testNow
	due := testNow.Add(4 * time.Second)
	m.poll(due)
	if !m.status.inflight["apps"] || m.status.inflight["lock"] {
		t.Fatalf("after 4s only apps are due: %v", m.status.inflight)
	}
}

func TestLogsPickerAndStream(t *testing.T) {
	m := loadedModel(t, 120, 40)
	keys(m, "2")
	golden(t, "logs-picker-120x40", plain(m))
	keys(m, "enter")
	if m.logs.app == "" {
		t.Fatal("enter did not choose an app")
	}
	gen := m.logs.gen
	send(m, batchMsg{kind: "logs", gen: gen, msgs: []tea.Msg{
		streamStateMsg{gen: gen, kind: "logs", state: streamLive},
		logMsg{gen: gen, rec: logRec(0, "web", "1", "stdout", "listening on :8080")},
		logMsg{gen: gen, rec: logRec(1, "web", "2", "stdout", "GET /health 200")},
		logMsg{gen: gen, rec: logRec(2, "web", "1", "stderr", "warn: slow query \x1b]0;pwned\x07 took 2s\x1b[2J")},
		logMsg{gen: gen, rec: logRec(3, "web", "1", "stdout", "GET /api/items 200")},
	}, closed: true})
	out := plain(m)
	if strings.Contains(out, "pwned") && strings.Contains(out, "\x1b") {
		t.Fatal("escape sequences reached the screen")
	}
	if !strings.Contains(out, "warn: slow query  took 2s") {
		t.Fatalf("log line missing or not sanitized:\n%s", out)
	}
	if strings.Contains(m.View().Content, "\x1b]0;") || strings.Contains(m.View().Content, "\x1b[2J") {
		t.Fatal("app output reached the terminal as a control sequence")
	}
	golden(t, "logs-120x40", out)
	golden(t, "logs-80x24", plain(resized(m, 80, 24)))
	// grep
	keys(m, "/", "a", "p", "i")
	if got := m.logs.shown(); got != 1 {
		t.Fatalf("grep shows %d lines", got)
	}
	golden(t, "logs-grep-120x40", plain(m))
	keys(m, "esc")
	if m.logs.shown() != 4 {
		t.Fatal("esc did not clear the grep")
	}
	// follow toggles
	keys(m, "f")
	if m.logs.follow {
		t.Fatal("f did not pause following")
	}
	keys(m, "f")
	if !m.logs.follow {
		t.Fatal("f did not resume following")
	}
	// A line from an old stream is ignored.
	before := len(m.logs.lines)
	send(m, batchMsg{kind: "logs", gen: gen - 1, msgs: []tea.Msg{logMsg{gen: gen - 1, rec: logRec(9, "web", "1", "stdout", "stale")}}, closed: true})
	if len(m.logs.lines) != before {
		t.Fatal("a stale stream wrote lines")
	}
	// Esc goes back to the picker and ends the stream.
	keys(m, "esc")
	if m.logs.app != "" {
		t.Fatal("esc did not go back to the picker")
	}
	m.shutdown()
}

func resized(m *Model, w, h int) *Model {
	send(m, tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

func TestLogsBufferIsCapped(t *testing.T) {
	m := loadedModel(t, 120, 40)
	keys(m, "2", "enter")
	gen := m.logs.gen
	recs := make([]tea.Msg, 0, 6200)
	for i := 0; i < 6200; i++ {
		recs = append(recs, logMsg{gen: gen, rec: logs.Record{Identity: logs.Identity{Role: "web"}, Timestamp: testNow, Message: "line"}})
	}
	for i := 0; i < len(recs); i += 256 {
		send(m, batchMsg{kind: "logs", gen: gen, msgs: recs[i:min(i+256, len(recs))], closed: true})
	}
	if n := len(m.logs.lines); n > maxLogLines+500 {
		t.Fatalf("buffer has %d lines", n)
	}
	if m.logs.shown() > maxLogLines {
		t.Fatalf("viewport shows %d lines", m.logs.shown())
	}
	m.shutdown()
}

func TestInvalidGrepKeepsPreviousFilter(t *testing.T) {
	m := loadedModel(t, 120, 40)
	keys(m, "2", "enter")
	gen := m.logs.gen
	send(m, batchMsg{kind: "logs", gen: gen, closed: true, msgs: []tea.Msg{logMsg{gen: gen, rec: logRec(0, "web", "1", "stdout", "hello (world)")}}})
	keys(m, "/", "(")
	if !m.logs.reErr {
		t.Fatal("invalid pattern not marked")
	}
	if !strings.Contains(plain(m), "not a regular expression") {
		t.Fatal("no message for an invalid pattern")
	}
	m.shutdown()
}

func TestServerGolden(t *testing.T) {
	for _, s := range sizes {
		m := loadedModel(t, s.w, s.h)
		keys(m, "3")
		gen := m.server.gen
		var samples []metrics.Sample
		for i := 0; i < 30; i++ {
			samples = append(samples, fixtureSample(testNow.Add(time.Duration(i-30)*10*time.Second), float64(10+(i*7)%80)))
		}
		send(m, statsSeedMsg{samples: samples}, dfMsg(fixtureDF()),
			batchMsg{kind: "stats", gen: gen, closed: true, msgs: []tea.Msg{streamStateMsg{gen: gen, kind: "stats", state: streamLive}, statsMsg{gen: gen, sample: fixtureSample(testNow, 64)}}})
		golden(t, "server-"+s.name, plain(m))
		m.shutdown()
	}
}

func TestServerWaitingForSample(t *testing.T) {
	m := loadedModel(t, 120, 40)
	keys(m, "3")
	if !strings.Contains(plain(m), "Waiting for the first sample") {
		t.Fatal("no waiting notice")
	}
	m.shutdown()
}

func TestSparkline(t *testing.T) {
	if got := sparkline([]float64{0, 50, 100}, 5, 100); got != "  ▁▅█" {
		t.Fatalf("sparkline %q", got)
	}
	if got := sparkline([]float64{0, 100, 0, 100}, 2, 100); got != "▁█" {
		t.Fatalf("sparkline %q", got)
	}
}

func TestApprovalsGolden(t *testing.T) {
	for _, s := range sizes {
		m := loadedModel(t, s.w, s.h)
		keys(m, "4")
		send(m, approvalsMsg{rows: fixtureApprovals()})
		golden(t, "approvals-"+s.name, plain(m))
	}
	m := loadedModel(t, 120, 40)
	keys(m, "4")
	send(m, approvalsMsg{rows: fixtureApprovals()})
	keys(m, "enter")
	if m.approvals.detail == nil {
		t.Fatal("detail not opened")
	}
	golden(t, "approval-detail-120x40", plain(m))
	keys(m, "esc")
	if m.approvals.detail != nil {
		t.Fatal("esc did not close the detail")
	}
}

func TestApprovalsPendingFirst(t *testing.T) {
	rows := fixtureApprovals()
	rows[0], rows[2] = rows[2], rows[0]
	ordered := orderApprovals(rows)
	if ordered[0].Status != "pending" {
		t.Fatalf("pending not first: %v", ordered[0].Status)
	}
}

func TestApprovalPlanHiddenForReadToken(t *testing.T) {
	m := loadedModel(t, 120, 40)
	keys(m, "4")
	rows := fixtureApprovals()
	rows[0].Plan = nil
	send(m, approvalsMsg{rows: rows})
	keys(m, "enter")
	if !strings.Contains(plain(m), "needs the deploy scope") {
		t.Fatalf("no explanation for a hidden plan:\n%s", plain(m))
	}
}

func TestApprovalTitle(t *testing.T) {
	for in, want := range map[string]string{
		"deploy api v3":       "deploy api → v3",
		"restart api/web":     "restart api/web",
		"scale worker to 3":   "scale worker to 3",
		"deploy api":          "deploy api",
		"deploy\x1b[2J api x": "deploy api → x",
	} {
		if got := approvalTitle(approvalRow{Summary: in}); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestTokenNeverShown(t *testing.T) {
	m := loadedModel(t, 120, 40)
	send(m, errMsg{src: "apps", text: "agent returned HTTP 500: secret-token-value"})
	// The client redacts before the message exists; here the model must still not add the token itself.
	for _, tb := range []string{"1", "2", "3", "4"} {
		keys(m, tb)
		if strings.Contains(strings.ReplaceAll(plain(m), "secret-token-value", ""), m.c.target.Token) {
			t.Fatal("token shown")
		}
	}
	m.shutdown()
}

func TestLightTheme(t *testing.T) {
	m := loadedModel(t, 120, 40)
	dark := plain(m)
	send(m, tea.BackgroundColorMsg{Color: color.White})
	if m.st.isDark() {
		t.Fatal("a white background did not give the light palette")
	}
	// The text is the same; only the colours change.
	if plain(m) != dark {
		t.Fatal("the light palette changed the layout")
	}
	if strings.Contains(m.View().Content, "\x1b") == false {
		t.Fatal("the view carries no colours")
	}
	send(m, tea.BackgroundColorMsg{Color: color.Black})
	if !m.st.isDark() {
		t.Fatal("a black background did not give the dark palette")
	}
}

func TestSanitize(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                  "plain",
		"a\x1b[31mred\x1b[0m":    "ared",
		"x\x1b]0;title\x07y":     "xy",
		"tab\there":              "tab    here",
		"bell\x07 and \x00 null": "bell and  null",
		"rtl\u202eevil":          "rtlevil",
		"bad\xffbyte":            "bad\uFFFDbyte",
		"emoji 🚀 ok":             "emoji 🚀 ok",
	} {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShortImage(t *testing.T) {
	for in, want := range map[string]string{
		digestA:                       "sha256:abcdef01",
		"registry.example/worker:v3":  "v3",
		"ghost:5":                     "ghost-skip",
		"nginx":                       "latest",
		"host:5000/app:v1@sha256:abc": "v1",
		"":                            "-",
	} {
		if in == "ghost:5" {
			want = "5"
		}
		if got := shortImage(in); got != want {
			t.Errorf("shortImage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatches(t *testing.T) {
	cases := []struct {
		name, q string
		want    bool
	}{{"worker", "wo", true}, {"worker", "WRK", true}, {"worker", "x", false}, {"api", "", true}, {"api", "pa", false}}
	for _, c := range cases {
		if matches(c.name, c.q) != c.want {
			t.Errorf("matches(%q,%q)", c.name, c.q)
		}
	}
}

func TestSelectedAppAccessor(t *testing.T) {
	m := loadedModel(t, 120, 40)
	a, ok := m.SelectedApp()
	if !ok || a.Ref.Name != "api" {
		t.Fatalf("selected %v %v", a.Ref.Name, ok)
	}
}

// replicaLog is a line of one replica of the web role, read from the container cid.
func replicaLog(sec int, task, cid, stream, msg string) logs.Record {
	return logs.Record{Identity: logs.Identity{Role: "web", Replica: task, ContainerID: cid}, Timestamp: testNow.Add(time.Duration(sec) * time.Second), Stream: stream, Message: msg}
}

// replicaLines is the stream of two replicas: the first answers twice, the second once.
func replicaLines(gen int) batchMsg {
	return batchMsg{kind: "logs", gen: gen, closed: true, msgs: []tea.Msg{
		streamStateMsg{gen: gen, kind: "logs", state: streamLive},
		logMsg{gen: gen, rec: replicaLog(0, "k2x8pq7vbn3rwm5ts6yhl4jd1", "f77449eeec32abcdef0123456789", "stdout", "GET /version 200")},
		logMsg{gen: gen, rec: replicaLog(1, "k2x8pq7vbn3rwm5ts6yhl4jd2", "a1b2c3d4e5f6abcdef0123456789", "stdout", "GET /health 200")},
		logMsg{gen: gen, rec: replicaLog(2, "k2x8pq7vbn3rwm5ts6yhl4jd1", "f77449eeec32abcdef0123456789", "stderr", "panic: something")},
	}}
}

// TestSourceNameUsesTheContainerID: a line's source is the role and the first characters of the container id, which a
// response and docker ps show; a record without one falls back to its task id.
func TestSourceNameUsesTheContainerID(t *testing.T) {
	r := logs.Record{Identity: logs.Identity{Role: "web", Replica: "k2x8pq7vbn3rwm5ts6yhl4jdc", ContainerID: "f77449eeec32abcdef"}}
	if got := sourceName(r); got != "web.f77449" {
		t.Fatalf("container id: %q", got)
	}
	r.ContainerID = ""
	if got := sourceName(r); got != "web.k2x8pq" {
		t.Fatalf("task id fallback: %q", got)
	}
	if got := sourceName(logs.Record{}); got != "app" {
		t.Fatalf("no role: %q", got)
	}
}

// TestLogsReplicaFilterCyclesThroughReplicas: p shows one replica's lines, then the next, then all of them again.
func TestLogsReplicaFilterCyclesThroughReplicas(t *testing.T) {
	m := loadedModel(t, 120, 40)
	keys(m, "2", "enter")
	send(m, replicaLines(m.logs.gen))
	if m.logs.shown() != 3 {
		t.Fatalf("shows %d lines before p", m.logs.shown())
	}
	keys(m, "p")
	if m.logs.replica != "web.f77449" || m.logs.shown() != 2 {
		t.Fatalf("first replica: %q, %d lines", m.logs.replica, m.logs.shown())
	}
	if out := plain(m); strings.Contains(out, "GET /health 200") || !strings.Contains(out, "panic: something") {
		t.Fatalf("the other replica's line is shown:\n%s", out)
	}
	keys(m, "p")
	if m.logs.replica != "web.a1b2c3" || m.logs.shown() != 1 {
		t.Fatalf("second replica: %q, %d lines", m.logs.replica, m.logs.shown())
	}
	keys(m, "p")
	if m.logs.replica != "" || m.logs.shown() != 3 {
		t.Fatalf("all again: %q, %d lines", m.logs.replica, m.logs.shown())
	}
	m.shutdown()
}

// TestLogsReplicaAndGrepBothApply: a line has to pass the replica and the grep; esc clears both.
func TestLogsReplicaAndGrepBothApply(t *testing.T) {
	m := loadedModel(t, 120, 40)
	keys(m, "2", "enter")
	send(m, replicaLines(m.logs.gen))
	keys(m, "p", "/", "h", "e", "a", "l", "t", "h", "enter")
	if m.logs.shown() != 0 {
		t.Fatalf("grep for the other replica's line shows %d lines", m.logs.shown())
	}
	keys(m, "p")
	if m.logs.replica != "web.a1b2c3" || m.logs.shown() != 1 {
		t.Fatalf("replica and grep: %q, %d lines", m.logs.replica, m.logs.shown())
	}
	if out := plain(m); !strings.Contains(out, "web.a1b2c3") || !strings.Contains(out, "grep health") || !strings.Contains(out, "replica web.a1b2c3") {
		t.Fatalf("footer does not show both filters:\n%s", out)
	}
	keys(m, "esc")
	if m.logs.pattern != "" || m.logs.replica != "" || m.logs.shown() != 3 || m.logs.app == "" {
		t.Fatalf("esc did not clear both: pattern %q replica %q, %d lines, app %q", m.logs.pattern, m.logs.replica, m.logs.shown(), m.logs.app)
	}
	m.shutdown()
}

// TestLogsEscClearsReplicaBeforeLeaving: with a replica chosen, esc clears it and stays in the logs; the next esc goes back
// to the picker. A new stream starts with every replica.
func TestLogsEscClearsReplicaBeforeLeaving(t *testing.T) {
	m := loadedModel(t, 120, 40)
	keys(m, "2", "enter")
	send(m, replicaLines(m.logs.gen))
	keys(m, "p", "esc")
	if m.logs.replica != "" || m.logs.app == "" {
		t.Fatalf("esc with a replica chosen: replica %q, app %q", m.logs.replica, m.logs.app)
	}
	keys(m, "p", "1", "2")
	if m.logs.replica != "" {
		t.Fatalf("a new stream kept the replica %q", m.logs.replica)
	}
	keys(m, "esc")
	if m.logs.app != "" {
		t.Fatal("esc with no filter did not go back to the picker")
	}
	m.shutdown()
}

// TestLogsReplicaGolden: the logs of one replica, at both sizes.
func TestLogsReplicaGolden(t *testing.T) {
	for _, s := range sizes {
		m := loadedModel(t, s.w, s.h)
		keys(m, "2", "enter")
		send(m, replicaLines(m.logs.gen))
		keys(m, "p")
		golden(t, "logs-replica-"+s.name, plain(m))
		m.shutdown()
	}
}

// TestLogsHelpKeepsHelpAndQuitAt80Columns: the logs footer fits 80 columns with ? help and q quit still showing.
func TestLogsHelpKeepsHelpAndQuitAt80Columns(t *testing.T) {
	m := loadedModel(t, 80, 24)
	keys(m, "2", "enter")
	if out := plain(m); !strings.Contains(out, "? help • q quit") {
		t.Fatalf("footer cut off at 80 columns:\n%s", out)
	}
	m.shutdown()
}

// TestCountsGroupThousands: the footer counts read as 1,203 lines.
func TestCountsGroupThousands(t *testing.T) {
	for n, want := range map[int]string{0: "0", 412: "412", 1203: "1,203", 1000000: "1,000,000"} {
		if got := grouped(n); got != want {
			t.Errorf("grouped(%d) = %q, want %q", n, got, want)
		}
	}
}
