package tui

import (
	"context"
	"fmt"
	"hash/fnv"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"berthy/internal/api"
	"berthy/internal/logs"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/stopwatch"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// maxLogLines is how many lines the logs view keeps.
const maxLogLines = 5000

// appItem is an app in the picker of the logs tab.
type appItem struct {
	name, path, desc string
}

func (i appItem) Title() string       { return i.name }
func (i appItem) Description() string { return i.desc }
func (i appItem) FilterValue() string { return i.name }

// logLine is one line of the logs view, drawn once when it arrives.
type logLine struct {
	rec      logs.Record
	source   string // the replica the line came from, as sourceName names it
	plain    string // what the grep filter reads: source and message
	rendered string
}

// logsModel is the logs tab: a picker of apps, then the followed logs of the chosen one.
type logsModel struct {
	picker   list.Model
	vp       viewport.Model
	grep     textinput.Model
	grepping bool
	pattern  string
	re       *regexp.Regexp
	reErr    bool
	replica  string // the source shown alone, or "" for all of them

	app, label string
	lines      []logLine
	pending    []logs.Record
	follow     bool

	gen    int
	cancel context.CancelFunc
	state  streamState
	text   string
	retry  time.Duration
	live   bool

	watch stopwatch.Model
}

func newLogsModel(st styles) logsModel {
	var lm logsModel
	pk := list.New(nil, lm.pickerDelegate(st), 0, 0)
	pk.SetShowTitle(false)
	pk.SetShowHelp(false)
	pk.SetShowStatusBar(true)
	pk.SetStatusBarItemName("app", "apps")
	pk.SetFilteringEnabled(true)
	pk.DisableQuitKeybindings()
	pk.Styles = st.listStyles()

	gi := textinput.New()
	gi.Prompt = "grep "
	gi.Placeholder = "regular expression"
	gi.SetVirtualCursor(true)
	gi.SetStyles(st.inputStyles())

	vp := viewport.New()
	vp.SoftWrap = true
	vp.MouseWheelEnabled = false
	vp.LeftGutterFunc = func(viewport.GutterContext) string { return " " }

	return logsModel{
		picker: pk, vp: vp, grep: gi, follow: true,
		watch: stopwatch.New(stopwatch.WithInterval(time.Second)),
	}
}

// pickerDelegate is the delegate of the picker for a set of styles.
func (l *logsModel) pickerDelegate(st styles) list.DefaultDelegate {
	del := list.NewDefaultDelegate()
	del.Styles = pickerItemStyles(st)
	return del
}

// restyle draws the buffered lines again with new styles.
func (l *logsModel) restyle(st styles) {
	for i := range l.lines {
		l.lines[i] = renderLine(st, l.lines[i].rec)
	}
	l.rebuild()
}

func pickerItemStyles(st styles) list.DefaultItemStyles {
	d := list.NewDefaultItemStyles(st.isDark())
	d.NormalTitle = lipgloss.NewStyle().Foreground(st.p.text).Padding(0, 0, 0, 2)
	d.NormalDesc = lipgloss.NewStyle().Foreground(st.p.muted).Padding(0, 0, 0, 2)
	d.SelectedTitle = lipgloss.NewStyle().Foreground(st.p.accent).Bold(true).Border(lipgloss.NormalBorder(), false, false, false, true).BorderForeground(st.p.accent).Padding(0, 0, 0, 1)
	d.SelectedDesc = lipgloss.NewStyle().Foreground(st.p.muted).Border(lipgloss.NormalBorder(), false, false, false, true).BorderForeground(st.p.accent).Padding(0, 0, 0, 1)
	d.DimmedTitle = lipgloss.NewStyle().Foreground(st.p.faint).Padding(0, 0, 0, 2)
	d.DimmedDesc = lipgloss.NewStyle().Foreground(st.p.faint).Padding(0, 0, 0, 2)
	d.FilterMatch = lipgloss.NewStyle().Foreground(st.p.accent).Underline(true)
	return d
}

// setApps fills the picker with the apps. apiName maps an app to the name the API takes.
func (l *logsModel) setApps(apps []api.App, apiName func(api.App) string, now time.Time) tea.Cmd {
	items := make([]list.Item, 0, len(apps))
	apps = append([]api.App(nil), apps...)
	sort.SliceStable(apps, func(i, j int) bool { return apps[i].Ref.Name < apps[j].Ref.Name })
	for _, a := range apps {
		sum := summarize(a, now)
		items = append(items, appItem{
			name: sanitize(a.Ref.Name),
			path: apiName(a),
			desc: fmt.Sprintf("%s %s %d/%d %s %s", a.Ref.Kind, glyphDot, sum.running, sum.desired, glyphDot, sum.health.glyph()+" "+sum.health.label()),
		})
	}
	return l.picker.SetItems(items)
}

// srcPalette is the set of colours the log sources take turns in.
func srcPalette(st styles) []lipgloss.Style {
	cols := []string{"#7DD3FC", "#F0ABFC", "#86EFAC", "#FDBA74", "#A5B4FC", "#FCA5A5"}
	if !st.isDark() {
		cols = []string{"#0369A1", "#A21CAF", "#15803D", "#C2410C", "#4338CA", "#B91C1C"}
	}
	out := make([]lipgloss.Style, len(cols))
	for i, c := range cols {
		out[i] = lipgloss.NewStyle().Foreground(lipgloss.Color(c))
	}
	return out
}

// sourceName is the name of the container a record came from: its role and the first characters of its container id, the
// ones docker ps shows. A record with no container id takes the first characters of its task id.
func sourceName(r logs.Record) string {
	name := r.Role
	if name == "" {
		name = "app"
	}
	id := []rune(r.ContainerID)
	if len(id) == 0 {
		id = []rune(r.Replica)
	}
	if len(id) > 0 {
		name += "." + string(id[:min(len(id), 6)])
	}
	return sanitize(name)
}

// renderLine draws one record: a dim time, the source in its own colour and the message, tinted when it came from stderr.
func renderLine(st styles, r logs.Record) logLine {
	src := sourceName(r)
	h := fnv.New32a()
	_, _ = h.Write([]byte(r.Role))
	pal := srcPalette(st)
	srcStyle := pal[int(h.Sum32())%len(pal)]
	msg := sanitize(r.Message)
	msgStyle := st.text
	if r.Stream == "stderr" {
		msgStyle = st.err
	}
	ts := r.Timestamp.Local().Format("15:04:05")
	return logLine{
		rec:      r,
		source:   src,
		plain:    src + " " + msg,
		rendered: st.faint.Render(ts) + " " + srcStyle.Render(src) + " " + msgStyle.Render(msg),
	}
}

// ingest adds records to the buffer, keeps the newest maxLogLines and redraws.
func (l *logsModel) ingest(st styles, recs []logs.Record) {
	for _, r := range recs {
		l.lines = append(l.lines, renderLine(st, r))
	}
	if len(l.lines) > maxLogLines+500 {
		l.lines = append([]logLine(nil), l.lines[len(l.lines)-maxLogLines:]...)
	}
	l.rebuild()
}

// rebuild fills the viewport with the lines that pass the replica filter and the grep.
func (l *logsModel) rebuild() {
	lines := l.lines
	if len(lines) > maxLogLines {
		lines = lines[len(lines)-maxLogLines:]
	}
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if l.replica != "" && ln.source != l.replica {
			continue
		}
		if l.re == nil || l.re.MatchString(ln.plain) {
			out = append(out, ln.rendered)
		}
	}
	l.vp.SetContentLines(out)
	if l.follow {
		l.vp.GotoBottom()
	}
}

// shown is how many lines pass the filter.
func (l *logsModel) shown() int { return l.vp.TotalLineCount() }

// compile reads the pattern being typed. A pattern that is not a regular expression keeps the previous filter and is marked.
func (l *logsModel) compile(p string) {
	if p == "" {
		l.re, l.reErr = nil, false
		return
	}
	expr := p
	if !strings.ContainsFunc(p, unicode.IsUpper) {
		expr = "(?i)" + p
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		l.reErr = true
		return
	}
	l.re, l.reErr = re, false
}

// reset forgets the lines of the previous app, and the replica chosen for it.
func (l *logsModel) reset() {
	l.lines = nil
	l.replica = ""
	l.live = false
	l.follow = true
	l.vp.SetContentLines(nil)
	l.state, l.text, l.retry = streamConnecting, "", 0
}

// sources are the distinct sources of the buffered lines, in the order each was first seen.
func (l *logsModel) sources() []string {
	seen := map[string]bool{}
	var out []string
	for _, ln := range l.lines {
		if !seen[ln.source] {
			seen[ln.source] = true
			out = append(out, ln.source)
		}
	}
	return out
}

// nextReplica moves the replica filter on: all, then each source in turn, then all again. A source that has left the
// buffer ends the cycle.
func (l *logsModel) nextReplica() {
	srcs := l.sources()
	next := ""
	if l.replica == "" {
		if len(srcs) > 0 {
			next = srcs[0]
		}
	} else if i := slices.Index(srcs, l.replica); i >= 0 && i+1 < len(srcs) {
		next = srcs[i+1]
	}
	l.replica = next
	l.rebuild()
}

// grouped writes a count with a comma every three digits, as the footer shows it.
func grouped(n int) string {
	s := strconv.Itoa(n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// selectedItem is the app under the picker's cursor.
func (l *logsModel) selectedItem() (appItem, bool) {
	it, ok := l.picker.SelectedItem().(appItem)
	return it, ok
}

// settingFilter is true while a text field of the tab has the keyboard.
func (l *logsModel) settingFilter() bool {
	if l.app == "" {
		return l.picker.SettingFilter()
	}
	return l.grepping
}

// updateView handles a key of the followed logs.
func (l *logsModel) updateView(msg tea.Msg, k keyMap) (cmd tea.Cmd, used bool) {
	if l.grepping {
		if km, ok := msg.(tea.KeyPressMsg); ok {
			switch {
			case key.Matches(km, k.Apply):
				l.grepping = false
				l.pattern = l.grep.Value()
				l.grep.Blur()
				l.compile(l.pattern)
				l.rebuild()
				return nil, true
			case key.Matches(km, k.Clear):
				l.grepping = false
				l.pattern = ""
				l.grep.Reset()
				l.grep.Blur()
				l.compile("")
				l.rebuild()
				return nil, true
			}
		}
		var c tea.Cmd
		l.grep, c = l.grep.Update(msg)
		l.compile(l.grep.Value())
		l.rebuild()
		return c, true
	}
	km, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil, false
	}
	switch {
	case key.Matches(km, k.Filter):
		l.grepping = true
		return l.grep.Focus(), true
	case key.Matches(km, k.Follow):
		l.follow = !l.follow
		if l.follow {
			l.vp.GotoBottom()
		}
		return nil, true
	case key.Matches(km, k.Bottom):
		l.follow = true
		l.vp.GotoBottom()
		return nil, true
	case key.Matches(km, k.Replica):
		l.nextReplica()
		return nil, true
	case key.Matches(km, k.Clear) && (l.pattern != "" || l.replica != ""):
		l.pattern = ""
		l.grep.Reset()
		l.compile("")
		l.replica = ""
		l.rebuild()
		return nil, true
	}
	var c tea.Cmd
	l.vp, c = l.vp.Update(msg)
	if l.follow && !l.vp.AtBottom() {
		// Scrolling up leaves the end: following stops until f or G.
		l.follow = false
	}
	return c, true
}

// status is the right-hand part of the pane title: the state of the stream.
func (l *logsModel) status(st styles, spin string) string {
	switch {
	case l.app == "":
		return ""
	case l.state == streamRetrying:
		why := l.text
		if why == "" {
			why = "stream ended"
		}
		return st.err.Render(fmt.Sprintf("%s %s %s retrying in %s", glyphFail, clip(sanitize(why), 40), glyphDot, api.HumanAge(l.retry)))
	case !l.live:
		return spin + st.muted.Render(" connecting")
	case l.follow:
		return st.ok.Render(glyphHealthy+" following") + st.faint.Render(" "+glyphDot+" ") + st.muted.Render(countdown(l.watch.Elapsed()))
	}
	return st.warn.Render("❚❚ paused") + st.faint.Render(" "+glyphDot+" ") + st.muted.Render(countdown(l.watch.Elapsed()))
}

// filterFoot is the line under the logs: the filters on, how many lines pass them and how to clear them.
func (l *logsModel) filterFoot(st styles) string {
	var parts []string
	if l.pattern != "" {
		parts = append(parts, st.muted.Render("grep ")+st.accent.Render(sanitize(l.pattern)))
	}
	hint := "(esc clears)"
	if l.replica != "" {
		parts = append(parts, st.muted.Render("replica ")+st.accent.Render(sanitize(l.replica)))
		hint = "(p next " + glyphDot + " esc clears)"
	}
	count := fmt.Sprintf("%s of %s lines  %s", grouped(l.shown()), grouped(len(l.lines)), hint)
	return strings.Join(parts, "  ") + "  " + st.faint.Render(count)
}

// view draws the pane's content.
func (l *logsModel) view(st styles, spin string, w, h int) string {
	if l.app == "" {
		return l.picker.View()
	}
	foot := ""
	switch {
	case l.grepping:
		foot = l.grep.View()
	case l.pattern != "" || l.replica != "":
		foot = l.filterFoot(st)
	}
	if l.reErr {
		foot = fit(foot, max(w-27, 10)) + "  " + st.err.Render("not a regular expression")
	}
	body := l.vp.View()
	if len(l.lines) == 0 {
		msg := st.muted.Render("Waiting for log lines…")
		if !l.live {
			msg = spin + st.muted.Render(" Connecting to the log stream…")
		}
		if l.state == streamRetrying {
			msg = st.muted.Render("No log lines yet.")
		}
		body = lipgloss.Place(w, max(h-1, 1), lipgloss.Center, lipgloss.Center, msg)
	} else if l.re != nil && l.shown() == 0 {
		body = lipgloss.Place(w, max(h-1, 1), lipgloss.Center, lipgloss.Center, st.muted.Render("No line matches the pattern."))
	} else if l.replica != "" && l.shown() == 0 {
		body = lipgloss.Place(w, max(h-1, 1), lipgloss.Center, lipgloss.Center, st.muted.Render("No line from this replica."))
	}
	if foot == "" {
		return body
	}
	return fitLines(body, w, max(h-1, 1)) + "\n " + foot
}
