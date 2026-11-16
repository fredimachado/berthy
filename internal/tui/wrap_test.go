package tui

import (
	"strings"
	"testing"

	"berthy/internal/logs"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// borderIntact checks that every line of the view is w cells wide and that the pane's frame is whole: a top edge, a left and
// a right border on every line of the body, and a bottom edge.
func borderIntact(t *testing.T, what, view string, w, h int) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	if len(lines) != h {
		t.Errorf("%s: %d lines, want %d", what, len(lines), h)
		return
	}
	for i, l := range lines {
		if got := lipgloss.Width(l); got != w {
			t.Errorf("%s: line %d is %d cells wide, want %d: %q", what, i, got, w, l)
		}
	}
	bottom := h - 3 // the status line and one line of help follow the pane
	for i := 2; i <= bottom; i++ {
		r := []rune(lines[i])
		first, last := string(r[0]), string(r[len(r)-1])
		switch {
		case i == 2 && (first != "╭" || last != "╮"):
			t.Errorf("%s: top edge broken: %q", what, lines[i])
		case i == bottom && (first != "╰" || last != "╯"):
			t.Errorf("%s: bottom edge broken: %q", what, lines[i])
		case i != 2 && i != bottom && (first != "│" || last != "│"):
			t.Errorf("%s: line %d leaves the frame: %q", what, i, lines[i])
		}
	}
}

const longDigest = "127.0.0.1:15001/berthy-schema/app@sha256:dd3efc15a599e4fe866920fd6eae2d07567498f2f1794914c38eb868177378b0"

func longPlanRows() []approvalRow {
	rows := fixtureApprovals()
	rows[0].Plan = []byte(`{"app":"api","image":"` + longDigest + `","env":{"DATABASE_URL":"[redacted]","NOTE":"` + strings.Repeat("0123456789", 40) + `"},"roles":["web"]}`)
	return rows
}

func TestApprovalPlanWrapsLongLinesInsideThePane(t *testing.T) {
	for _, s := range sizes {
		m := loadedModel(t, s.w, s.h)
		keys(m, "4")
		send(m, approvalsMsg{rows: longPlanRows()})
		keys(m, "enter")
		out := plain(m)
		borderIntact(t, "approval detail "+s.name, out, s.w, s.h)
		// The whole digest can be read: the pieces, joined, are the value.
		squashed := strings.NewReplacer("│", "", " ", "", "\n", "").Replace(out)
		if !strings.Contains(squashed, longDigest) {
			t.Errorf("%s: the digest is not whole on screen:\n%s", s.name, out)
		}
		if !strings.Contains(squashed, strings.Repeat("0123456789", 40)) {
			t.Errorf("%s: the long value is cut", s.name)
		}
		// A continuation is indented under its value: two cells deeper than the key, after the margin.
		lines := strings.Split(out, "\n")
		for i, l := range lines {
			if strings.Contains(l, `"image": "127.0.0.1`) {
				next := []rune(lines[i+1])
				if !strings.HasPrefix(string(next), "│     ") || strings.TrimSpace(string(next[1:len(next)-1])) == "" {
					t.Errorf("%s: continuation not indented: %q", s.name, lines[i+1])
				}
			}
		}
		golden(t, "approval-long-plan-"+s.name, out)
	}
}

func TestLongAgentStringsStayInsideTheFrame(t *testing.T) {
	word := strings.Repeat("x", 300)
	for _, s := range sizes {
		// A failed deploy with a reason that is one very long word, and a job and a backup that failed the same way.
		m := loadedModel(t, s.w, s.h)
		keys(m, "enter")
		hist := fixtureHistory()
		hist[1].Reason = "boom " + word + " end"
		send(m, historyMsg{app: "api", rows: hist})
		m.detail.app.Jobs[0].Last.Status = "failed"
		m.detail.app.Jobs[0].Last.Detail = word
		m.detail.app.Backups[0].LastStatus = "failed"
		m.detail.app.Backups[0].Detail = word
		borderIntact(t, "app detail "+s.name, plain(m), s.w, s.h)

		// An approval that was decided with a long result and a long actor name.
		m = loadedModel(t, s.w, s.h)
		keys(m, "4")
		rows := fixtureApprovals()
		rows[1].Result = word
		rows[1].DecidedBy = word
		rows[0].Actor = word
		send(m, approvalsMsg{rows: rows})
		borderIntact(t, "approvals list "+s.name, plain(m), s.w, s.h)
		keys(m, "down", "enter")
		borderIntact(t, "approval detail (decided) "+s.name, plain(m), s.w, s.h)
		keys(m, "esc", "up", "enter")
		borderIntact(t, "approval detail (pending) "+s.name, plain(m), s.w, s.h)

		// A log line that is one very long word, and one long line with spaces.
		m = loadedModel(t, s.w, s.h)
		keys(m, "2", "enter")
		gen := m.logs.gen
		send(m, batchMsg{kind: "logs", gen: gen, closed: true, msgs: []tea.Msg{
			streamStateMsg{gen: gen, kind: "logs", state: streamLive},
			logMsg{gen: gen, rec: logs.Record{Identity: logs.Identity{Role: "web", Replica: "1"}, Timestamp: testNow, Message: word}},
			logMsg{gen: gen, rec: logs.Record{Identity: logs.Identity{Role: "web", Replica: "1"}, Timestamp: testNow, Stream: "stderr", Message: strings.Repeat("word ", 120)}},
			logMsg{gen: gen, rec: logs.Record{Identity: logs.Identity{Role: "web", Replica: "1"}, Timestamp: testNow, Message: "日本語のとても長い行です。" + strings.Repeat("日本語", 80)}},
		}})
		borderIntact(t, "logs "+s.name, plain(m), s.w, s.h)
		keys(m, "/", "w")
		borderIntact(t, "logs grep "+s.name, plain(m), s.w, s.h)
		m.shutdown()

		// The apps table with a very long name and role names.
		m = loadedModel(t, s.w, s.h)
		apps := fixtureApps()
		apps[0].Ref.Name = word
		apps[0].Roles[0].Name = word
		send(m, appsMsg{apps: apps})
		borderIntact(t, "apps "+s.name, plain(m), s.w, s.h)
	}
}

func TestWrapJSONLine(t *testing.T) {
	st := newStyles(newPalette(true))
	line := `    "image": "` + strings.Repeat("a", 50) + `",`
	got := wrapJSONLine(st, line, 40)
	if len(got) < 2 {
		t.Fatalf("not wrapped: %q", got)
	}
	var joined strings.Builder
	for i, l := range got {
		if w := lipgloss.Width(l); w > 40 {
			t.Errorf("line %d is %d cells", i, w)
		}
		text := stripANSIForTest(l)
		if i > 0 && !strings.HasPrefix(text, " "+strings.Repeat(" ", 6)) {
			t.Errorf("continuation %d not indented under the line: %q", i, text)
		}
		joined.WriteString(strings.TrimLeft(text, " "))
	}
	if joined.String() != strings.TrimLeft(line, " ") {
		t.Errorf("the pieces do not make the line: %q", joined.String())
	}
	// A short line is one line; an empty line is kept.
	if got := wrapJSONLine(st, "  ", 40); len(got) != 1 {
		t.Errorf("empty line: %q", got)
	}
	if got := wrapJSONLine(st, `{`, 40); len(got) != 1 {
		t.Errorf("brace: %q", got)
	}
	// Very narrow: no endless loop, no zero-width chunks.
	if got := wrapJSONLine(st, `"k": "`+strings.Repeat("b", 30)+`"`, 6); len(got) == 0 {
		t.Error("narrow wrap lost the line")
	}
}
