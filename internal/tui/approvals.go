package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/timer"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// approvalItem is a request of an AI agent in the list of the approvals tab.
type approvalItem struct{ row approvalRow }

func (i approvalItem) FilterValue() string {
	return i.row.Summary + " " + i.row.Actor + " " + i.row.Status
}

// approvalTitle is the title of a request: "deploy api → v3".
func approvalTitle(r approvalRow) string {
	words := strings.Fields(sanitize(r.Summary))
	if len(words) == 3 && words[0] == "deploy" {
		return words[0] + " " + words[1] + " " + glyphArrow + " " + words[2]
	}
	if len(words) == 0 {
		return sanitize(r.Kind)
	}
	return strings.Join(words, " ")
}

// approvalNote is the second line of an item: who asked and what the time is.
func approvalNote(r approvalRow, now time.Time) string {
	note := "requested by " + sanitize(r.Actor)
	switch r.Status {
	case "pending":
		if now.Before(r.Expires) {
			return note + " " + glyphDot + " expires in " + until(now, r.Expires)
		}
		return note + " " + glyphDot + " expired " + ago(now, r.Expires)
	case "expired":
		return note + " " + glyphDot + " expired " + ago(now, r.Expires)
	}
	note += " " + glyphDot + " " + ago(now, r.Created)
	if r.DecidedBy != "" {
		note += " " + glyphDot + " " + r.Status + " by " + sanitize(r.DecidedBy)
	}
	return note
}

// approvalDelegate draws an approval as two lines: the title with its status pill and who asked with the time left.
type approvalDelegate struct {
	st  styles
	now func() time.Time
}

func (d approvalDelegate) Height() int                         { return 2 }
func (d approvalDelegate) Spacing() int                        { return 1 }
func (d approvalDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }

func (d approvalDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	it, ok := item.(approvalItem)
	if !ok {
		return
	}
	width := m.Width()
	selected := index == m.Index()
	st := d.st
	pill := st.statusPill(it.row.Status)
	title := approvalTitle(it.row)
	note := approvalNote(it.row, d.now())
	marker := "  "
	titleStyle := st.text.Bold(true)
	if selected {
		marker = st.accent.Render(glyphCursor) + " "
		titleStyle = st.accent.Bold(true)
	}
	left := marker + titleStyle.Render(clip(title, max(width-lipgloss.Width(pill)-4, 8)))
	first := between(left, pill, width-1)
	second := "  " + st.muted.Render(clip(note, max(width-4, 8)))
	if selected {
		second = st.accent.Render(glyphCursor) + " " + st.muted.Render(clip(note, max(width-4, 8)))
	}
	fmt.Fprint(w, first+"\n"+second)
}

// approvalDetail is one request opened: its facts, a countdown and its plan.
type approvalDetail struct {
	row   approvalRow
	vp    viewport.Model
	timer timer.Model
	// raw is the plan as indented JSON, one entry per line; note says why there is none. plan is raw wrapped and coloured for
	// the width of the pane.
	raw, note []string
	plan      []string
}

func newApprovalDetail(st styles, r approvalRow, now time.Time) (*approvalDetail, tea.Cmd) {
	vp := viewport.New()
	vp.MouseWheelEnabled = false
	d := &approvalDetail{row: r, vp: vp}
	d.load(st)
	d.timer = timer.New(max(r.Expires.Sub(now), 0), timer.WithInterval(time.Second))
	if r.Status != "pending" || !now.Before(r.Expires) {
		return d, nil
	}
	return d, d.timer.Init()
}

// load reads the plan of the row as indented JSON, or notes why there is none.
func (d *approvalDetail) load(st styles) {
	d.raw, d.note = nil, nil
	raw := bytes.TrimSpace(d.row.Plan)
	if len(raw) == 0 || string(raw) == "null" {
		d.note = []string{" " + st.muted.Render("The plan is not shown to this token: it needs the deploy scope."), "",
			" " + st.faint.Render("Review it with a deploy-scope token, or with `berthy approvals`.")}
		return
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", "  ") != nil {
		d.note = []string{" " + st.err.Render("The plan could not be read.")}
		return
	}
	for _, l := range strings.Split(pretty.String(), "\n") {
		d.raw = append(d.raw, sanitize(l))
	}
}

// render wraps and colours the plan for a pane w cells wide. A line that is too long continues on the next line, under its
// own value, so a whole digest can be read and no line goes past the border.
func (d *approvalDetail) render(st styles, w int) {
	if d.note != nil {
		d.plan = d.note
		return
	}
	d.plan = d.plan[:0]
	for _, l := range d.raw {
		d.plan = append(d.plan, wrapJSONLine(st, l, w)...)
	}
}

// wrapJSONLine breaks one line of indented JSON into lines of at most w-1 cells, with a margin of one cell on the left. The continuation lines
// are indented two cells deeper than the line itself. The key is drawn in the accent colour and the value by its kind.
func wrapJSONLine(st styles, line string, w int) []string {
	runes := []rune(line)
	indent := len(runes) - len([]rune(strings.TrimLeft(line, " ")))
	rest := string(runes[indent:])
	keyEnd, valueStart := -1, indent
	if strings.HasPrefix(rest, `"`) {
		if k, _, ok := strings.Cut(rest, `": `); ok {
			keyEnd = indent + len([]rune(k)) + 1
			valueStart = keyEnd + 2
		}
	}
	value := string(runes[min(valueStart, len(runes)):])
	vs := valueStyle(st, value)
	styleAt := func(i int) lipgloss.Style {
		switch {
		case i < indent:
			return lipgloss.NewStyle()
		case keyEnd >= 0 && i < keyEnd:
			return st.accent
		case keyEnd >= 0 && i < valueStart:
			return st.faint
		}
		return vs
	}
	first := max(w-2, 4) // a margin of one cell on each side
	cont := indent + 2
	second := first - cont
	if second < 12 {
		cont, second = 0, first
	}
	var out []string
	pos := 0
	for pos < len(runes) || len(out) == 0 {
		limit := first
		lead := " "
		if len(out) > 0 {
			limit = second
			lead = " " + strings.Repeat(" ", cont)
		}
		end, width := pos, 0
		for end < len(runes) {
			rw := lipgloss.Width(string(runes[end]))
			if width+rw > limit {
				break
			}
			width += rw
			end++
		}
		if end == pos && pos < len(runes) {
			end = pos + 1
		}
		var b strings.Builder
		for i := pos; i < end; {
			j := i
			st := styleAt(i)
			for j < end && sameStyle(styleAt(j), st) {
				j++
			}
			b.WriteString(st.Render(string(runes[i:j])))
			i = j
		}
		out = append(out, lead+b.String())
		pos = end
		if pos >= len(runes) {
			break
		}
	}
	return out
}

// sameStyle reports whether two styles of the plan are the same one.
func sameStyle(a, b lipgloss.Style) bool { return a.Render("x") == b.Render("x") }

func valueStyle(st styles, v string) lipgloss.Style {
	v = strings.TrimRight(v, ",")
	switch {
	case v == "true" || v == "false" || v == "null":
		return st.info
	case strings.HasPrefix(v, `"`):
		return st.text
	case v != "" && (v[0] == '-' || (v[0] >= '0' && v[0] <= '9')):
		return st.info
	}
	return st.faint
}

// resize sets the plan viewport below the header lines and wraps the plan for the width.
func (d *approvalDetail) resize(st styles, w, h, header int) {
	d.vp.SetWidth(w)
	d.vp.SetHeight(max(h-header, 1))
	d.render(st, w)
	d.vp.SetContentLines(d.plan)
}

// header is the fixed part of the detail: the request, who asked, and the time left.
func (d *approvalDetail) header(st styles, now time.Time, w int) []string {
	r := d.row
	lines := []string{
		between(" "+st.bold.Render(clip(approvalTitle(r), max(w-16, 10))), st.statusPill(r.Status)+" ", w),
		" " + st.muted.Render("requested by ") + st.text.Render(sanitize(r.Actor)) + st.faint.Render(" "+glyphDot+" ") + st.muted.Render(ago(now, r.Created)) +
			st.faint.Render(" "+glyphDot+" ") + st.muted.Render(sanitize(r.Kind)),
	}
	switch {
	case r.Status == "pending" && now.Before(r.Expires):
		left := countdown(d.timer.Timeout)
		lines = append(lines, " "+st.warn.Render("⏳ "+left)+st.muted.Render(" left to decide")+st.faint.Render(" "+glyphDot+" expires "+r.Expires.Local().Format("15:04:05")))
	case r.Status == "pending":
		lines = append(lines, " "+st.err.Render("expired "+ago(now, r.Expires)))
	default:
		decided := r.Status
		if r.DecidedBy != "" {
			decided += " by " + sanitize(r.DecidedBy)
		}
		lines = append(lines, " "+st.muted.Render(decided))
		if r.Result != "" {
			lines[len(lines)-1] += st.faint.Render(" "+glyphDot+" ") + st.muted.Render(clip(sanitize(r.Result), max(w-30, 10)))
		}
	}
	lines = append(lines, " "+st.faint.Render(strings.Repeat("─", max(w-2, 0))), " "+st.heading.Render("Plan"))
	return lines
}

// orderApprovals puts the requests that wait first, the soonest to expire at the top, then the decided ones, newest first.
func orderApprovals(rows []approvalRow) []approvalRow {
	out := append([]approvalRow(nil), rows...)
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := out[i].Status == "pending", out[j].Status == "pending"
		if pi != pj {
			return pi
		}
		if pi {
			return out[i].Expires.Before(out[j].Expires)
		}
		return out[i].Created.After(out[j].Created)
	})
	return out
}

// approvalsModel is the approvals tab.
type approvalsModel struct {
	list   list.Model
	rows   []approvalRow
	loaded bool
	detail *approvalDetail
}

func newApprovalsModel(st styles, now func() time.Time) approvalsModel {
	l := list.New(nil, approvalDelegate{st: st, now: now}, 0, 0)
	l.SetShowTitle(false)
	l.SetShowHelp(false)
	l.SetShowStatusBar(true)
	l.SetStatusBarItemName("request", "requests")
	l.SetFilteringEnabled(true)
	l.DisableQuitKeybindings()
	l.Styles = st.listStyles()
	return approvalsModel{list: l}
}

// setRows replaces the requests and keeps the cursor on the same one.
func (a *approvalsModel) setRows(rows []approvalRow) tea.Cmd {
	var id string
	if it, ok := a.list.SelectedItem().(approvalItem); ok {
		id = it.row.ID
	}
	a.rows = orderApprovals(rows)
	a.loaded = true
	items := make([]list.Item, 0, len(a.rows))
	sel := -1
	for i, r := range a.rows {
		items = append(items, approvalItem{row: r})
		if r.ID == id {
			sel = i
		}
	}
	cmd := a.list.SetItems(items)
	if sel >= 0 {
		a.list.Select(sel)
	}
	return cmd
}

// selected is the request under the cursor.
func (a *approvalsModel) selected() (approvalRow, bool) {
	it, ok := a.list.SelectedItem().(approvalItem)
	return it.row, ok
}

// view draws the pane's content.
func (a *approvalsModel) view(st styles, w, h int) string {
	if !a.loaded {
		return ""
	}
	if len(a.rows) == 0 {
		return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, st.muted.Render("No change has been requested by an AI agent.")+"\n"+
			lipgloss.PlaceHorizontal(w, lipgloss.Center, st.faint.Render("Held requests of agent tokens appear here.")))
	}
	return a.list.View()
}
