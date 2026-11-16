package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"berthy/internal/api"
	"berthy/internal/metrics"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/stopwatch"
	"charm.land/bubbles/v2/timer"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type tab int

const (
	tabApps tab = iota
	tabLogs
	tabServer
	tabApprovals
)

var tabNames = [...]string{"Apps", "Logs", "Server", "Approvals"}

// The sizes below which the interface asks for a bigger terminal.
const (
	minWidth  = 60
	minHeight = 14
)

// How often each part of the data is read.
const (
	appsEvery      = 3 * time.Second
	lockEvery      = 5 * time.Second
	pendingEvery   = 5 * time.Second
	approvalsEvery = 5 * time.Second
	historyEvery   = 10 * time.Second
	dfEvery        = 30 * time.Second
	infoEvery      = 30 * time.Second
)

// overlay is the slot of what is drawn over the views: the confirmations of the actions in the next slice. While there is one,
// it gets every key.
type overlay interface {
	Update(tea.Msg) (overlay, tea.Cmd)
	View(st styles, w, h int) string
}

// Model is the whole interface: the tabs, the status line and the help.
type Model struct {
	c      *client
	env    string
	host   string
	secure bool // the environment is protected

	width, height int
	bodyH         int

	st   styles
	keys keyMap
	help help.Model
	now  func() time.Time
	// noAnim draws the meters at their value at once; the tests use it.
	noAnim bool

	active    tab
	apps      appsModel
	detail    *detailModel
	logs      logsModel
	server    serverModel
	approvals approvalsModel
	status    statusModel
	overlay   overlay

	lastRefresh time.Time
}

// NewModel makes the interface for an agent. now is the clock, which the tests fix.
func NewModel(c *client, env string, now func() time.Time) *Model {
	st := newStyles(newPalette(true))
	m := &Model{c: c, env: env, st: st, keys: newKeyMap(), help: help.New(), now: now, width: 120, height: 40}
	m.help.Styles = st.helpStyles()
	m.apps = newAppsModel(st)
	m.logs = newLogsModel(st)
	m.server = newServerModel(st)
	m.approvals = newApprovalsModel(st, now)
	m.status = newStatusModel(st)
	m.layout()
	return m
}

// SelectedApp is the app under the cursor of the apps tab, or the one whose detail is open. The actions of the next slice act
// on it.
func (m *Model) SelectedApp() (api.App, bool) {
	if m.detail != nil {
		return m.detail.app, true
	}
	return m.apps.selected()
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		tea.RequestBackgroundColor,
		m.start("info", m.c.fetchInfo()),
		m.start("apps", m.c.fetchApps()),
		m.start("lock", m.c.fetchLock()),
		m.start("pending", m.c.fetchPending()),
		tick(),
	)
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// start marks a request as under way and makes sure the spinner runs while it is.
func (m *Model) start(kind string, cmd tea.Cmd) tea.Cmd {
	m.status.inflight[kind] = true
	m.status.fetched[kind] = m.now()
	return tea.Batch(cmd, m.wantSpinner())
}

// wantSpinner starts the spinner's ticks when something needs it and they are not running.
func (m *Model) wantSpinner() tea.Cmd {
	if m.status.spinning || !m.spinnerNeeded() {
		return nil
	}
	m.status.spinning = true
	return m.status.spin.Tick
}

// spinnerNeeded is true while a request is under way or a stream is connecting on the tab that shows it.
func (m *Model) spinnerNeeded() bool {
	switch {
	case m.status.busy(), m.status.unreachable:
		return m.status.busy()
	case m.active == tabLogs && m.logs.app != "" && !m.logs.live:
		return true
	case m.active == tabServer && !m.server.live:
		return true
	}
	return false
}

// setTheme builds every style again for a light or a dark terminal.
func (m *Model) setTheme(dark bool) {
	m.st = newStyles(newPalette(dark))
	st := m.st
	m.help.Styles = st.helpStyles()
	m.apps.table.SetStyles(st.appTableStyles())
	m.apps.filter.SetStyles(st.inputStyles())
	m.logs.grep.SetStyles(st.inputStyles())
	m.logs.picker.Styles = st.listStyles()
	del := m.logs.pickerDelegate(st)
	m.logs.picker.SetDelegate(del)
	m.server.table.SetStyles(st.tableStyles())
	m.server.colors = ramp(st)
	m.server.meters = map[string]*meter{}
	m.approvals.list.Styles = st.listStyles()
	m.approvals.list.SetDelegate(approvalDelegate{st: st, now: m.now})
	m.status.spin.Style = st.accent
	m.logs.restyle(st)
	if m.detail != nil {
		m.detail.render(st, m.now())
	}
	if len(m.server.samples) > 0 {
		m.server.moveMeters(st)
	}
}

// helpContext says which help to show.
func (m *Model) helpContext() helpView {
	switch m.active {
	case tabApps:
		switch {
		case m.apps.filtering:
			return helpFilter
		case m.detail != nil:
			return helpDetail
		}
		return helpApps
	case tabLogs:
		switch {
		case m.logs.app == "" && m.logs.picker.SettingFilter():
			return helpListFilter
		case m.logs.app == "":
			return helpLogsPick
		case m.logs.grepping:
			return helpFilter
		}
		return helpLogs
	case tabServer:
		return helpServer
	}
	switch {
	case m.approvals.detail != nil:
		return helpApprovalDetail
	case m.approvals.list.SettingFilter():
		return helpListFilter
	}
	return helpApprovals
}

func (m *Model) helpView() string {
	return m.help.View(contextKeys{k: m.keys, view: m.helpContext()})
}

// layout sizes every part for the terminal.
func (m *Model) layout() {
	m.help.SetWidth(m.width)
	footer := lipgloss.Height(m.helpView())
	m.bodyH = max(m.height-2-1-footer, 3)
	iw, ih := max(m.width-2, 4), max(m.bodyH-2, 1)
	m.apps.resize(iw, ih)
	if m.detail != nil {
		m.detail.resize(m.st, m.now(), iw, ih)
	}
	m.logs.picker.SetSize(iw, ih)
	m.logs.vp.SetWidth(iw)
	m.logs.vp.SetHeight(max(ih-1, 1))
	m.logs.grep.SetWidth(max(iw-8, 10))
	m.approvals.list.SetSize(iw, ih)
	if d := m.approvals.detail; d != nil {
		d.resize(m.st, iw, ih, len(d.header(m.st, m.now(), iw)))
	}
}

// Update handles every message of the program.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	add := func(c tea.Cmd) {
		if c != nil {
			cmds = append(cmds, c)
		}
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
	case tea.BackgroundColorMsg:
		m.setTheme(msg.IsDark())
		m.layout()
	case tea.KeyPressMsg:
		add(m.key(msg))
	case tickMsg:
		add(tick())
		add(m.poll(m.now()))
	case spinner.TickMsg:
		var c tea.Cmd
		m.status.spin, c = m.status.spin.Update(msg)
		if m.spinnerNeeded() {
			add(c)
		} else if c != nil {
			m.status.spinning = false
		}
	case progress.FrameMsg:
		for _, c := range m.server.updateFrame(msg) {
			add(c)
		}
	case stopwatch.TickMsg, stopwatch.StartStopMsg, stopwatch.ResetMsg:
		var c tea.Cmd
		m.logs.watch, c = m.logs.watch.Update(msg)
		add(c)
	case timer.TickMsg, timer.TimeoutMsg, timer.StartStopMsg:
		if d := m.approvals.detail; d != nil {
			var c tea.Cmd
			d.timer, c = d.timer.Update(msg)
			add(c)
		}
	case batchMsg:
		add(m.stream(msg))
	case infoMsg:
		m.ok("info")
		m.host, m.secure, m.status.infoKnown = msg.Hostname, msg.Protected, true
	case appsMsg:
		m.ok("apps")
		m.lastRefresh = m.now()
		m.apps.setApps(msg.apps)
		m.server.setNames(msg.apps)
		add(m.logs.setApps(msg.apps, m.appPath, m.now()))
		if m.detail != nil {
			for _, a := range msg.apps {
				if appKey(a) == m.detail.key {
					m.detail.app = a
				}
			}
		}
	case lockMsg:
		m.ok("lock")
		m.status.lock, m.status.lockKnown = lockInfo(msg), true
	case pendingMsg:
		m.ok("pending")
		m.status.pending, m.status.pendingKnown = msg.n, true
	case approvalsMsg:
		m.ok("approvals")
		add(m.approvals.setRows(msg.rows))
		n := 0
		for _, r := range msg.rows {
			if r.Status == "pending" && m.now().Before(r.Expires) {
				n++
			}
		}
		m.status.pending, m.status.pendingKnown = n, true
		if d := m.approvals.detail; d != nil {
			for _, r := range msg.rows {
				if r.ID == d.row.ID {
					d.row = r
					d.load(m.st)
					d.resize(m.st, max(m.width-2, 4), max(m.bodyH-2, 1), len(d.header(m.st, m.now(), max(m.width-2, 4))))
				}
			}
		}
	case historyMsg:
		m.ok("history")
		if m.detail != nil && m.detail.path == msg.app {
			m.detail.hist, m.detail.histLoaded, m.detail.histErr = msg.rows, true, ""
		}
	case dfMsg:
		m.ok("df")
		for _, c := range m.server.setDF(m.st, dfInfo(msg)) {
			add(c)
		}
	case statsSeedMsg:
		m.ok("stats")
		for _, c := range m.server.seed(m.st, msg.samples) {
			add(c)
		}
	case errMsg:
		m.failed(msg)
	}
	if o := m.overlay; o != nil {
		if _, isKey := msg.(tea.KeyPressMsg); !isKey {
			var c tea.Cmd
			m.overlay, c = o.Update(msg)
			add(c)
		}
	}
	return m, tea.Batch(cmds...)
}

// ok records that a request was answered: the agent is reachable, and any earlier refusal of it is over.
func (m *Model) ok(src string) {
	s := &m.status
	delete(s.inflight, src)
	delete(s.errs, src)
	s.unreachable = false
	s.failures = 0
	s.lastOK = m.now()
}

// failed records a request that did not work. A refusal goes to the status line; an agent that cannot be reached is retried
// with a growing wait while the last data stays on the screen.
func (m *Model) failed(e errMsg) {
	s := &m.status
	delete(s.inflight, e.src)
	if e.network {
		s.failures++
		s.unreachable = true
		s.retryAt = m.now().Add(backoff(s.failures))
		return
	}
	if e.text == "stopped" {
		return
	}
	s.unreachable = false
	if e.src == "history" && m.detail != nil {
		m.detail.histErr = e.text
		return
	}
	if e.src == "stats" {
		// The seed of the sparkline is optional: the stream fills it.
		return
	}
	s.errs[e.src] = e.text
}

// poll starts the requests that are due. While the agent cannot be reached only the apps are asked for, when the wait is over.
func (m *Model) poll(now time.Time) tea.Cmd {
	var cmds []tea.Cmd
	s := &m.status
	due := func(kind string, every time.Duration) bool {
		return !s.inflight[kind] && now.Sub(s.fetched[kind]) >= every
	}
	if s.unreachable {
		if !s.inflight["apps"] && !now.Before(s.retryAt) {
			cmds = append(cmds, m.start("apps", m.c.fetchApps()))
		}
		return tea.Batch(cmds...)
	}
	if due("apps", appsEvery) {
		cmds = append(cmds, m.start("apps", m.c.fetchApps()))
	}
	if due("lock", lockEvery) {
		cmds = append(cmds, m.start("lock", m.c.fetchLock()))
	}
	if due("pending", pendingEvery) && m.active != tabApprovals {
		cmds = append(cmds, m.start("pending", m.c.fetchPending()))
	}
	if due("info", infoEvery) {
		cmds = append(cmds, m.start("info", m.c.fetchInfo()))
	}
	if m.active == tabApprovals && due("approvals", approvalsEvery) {
		cmds = append(cmds, m.start("approvals", m.c.fetchApprovals()))
	}
	if m.active == tabServer && due("df", dfEvery) {
		cmds = append(cmds, m.start("df", m.c.fetchDF()))
	}
	if m.detail != nil && m.active == tabApps && due("history", historyEvery) {
		cmds = append(cmds, m.start("history", m.c.fetchHistory(m.detail.path)))
	}
	return tea.Batch(cmds...)
}

// refreshNow reads everything the visible tab shows, now.
func (m *Model) refreshNow() tea.Cmd {
	cmds := []tea.Cmd{m.start("apps", m.c.fetchApps()), m.start("lock", m.c.fetchLock()), m.start("pending", m.c.fetchPending())}
	switch m.active {
	case tabApps:
		if m.detail != nil {
			cmds = append(cmds, m.start("history", m.c.fetchHistory(m.detail.path)))
		}
	case tabApprovals:
		cmds = append(cmds, m.start("approvals", m.c.fetchApprovals()))
	case tabServer:
		cmds = append(cmds, m.start("df", m.c.fetchDF()))
	}
	return tea.Batch(cmds...)
}

// appPath is the name the API takes for an app: its name, or kind:name when two kinds share it.
func (m *Model) appPath(a api.App) string {
	for _, o := range m.apps.apps {
		if o.Ref.Name == a.Ref.Name && o.Ref.Kind != a.Ref.Kind {
			return a.Ref.Kind + ":" + a.Ref.Name
		}
	}
	return a.Ref.Name
}

// stream handles what a stream delivered: the lines or samples, and its state.
func (m *Model) stream(b batchMsg) tea.Cmd {
	var cmds []tea.Cmd
	var current int
	switch b.kind {
	case "logs":
		current = m.logs.gen
	case "stats":
		current = m.server.gen
	}
	if b.gen != current {
		return nil
	}
	for _, msg := range b.msgs {
		switch x := msg.(type) {
		case logMsg:
			m.logs.pending = append(m.logs.pending, x.rec)
		case statsMsg:
			for _, c := range m.server.addSamples(m.st, []metrics.Sample{x.sample}) {
				cmds = append(cmds, c)
			}
		case streamStateMsg:
			cmds = append(cmds, m.streamState(x))
		}
	}
	if len(m.logs.pending) > 0 {
		m.logs.ingest(m.st, m.logs.pending)
		m.logs.pending = m.logs.pending[:0]
	}
	if !b.closed {
		cmds = append(cmds, listen(b.ch, b.kind, b.gen))
	}
	return tea.Batch(cmds...)
}

// streamState records a change of a stream's state.
func (m *Model) streamState(x streamStateMsg) tea.Cmd {
	var cmds []tea.Cmd
	switch x.kind {
	case "logs":
		m.logs.state, m.logs.text, m.logs.retry = x.state, x.text, x.retry
		switch x.state {
		case streamLive:
			if !m.logs.live {
				m.logs.live = true
				m.logs.watch = stopwatch.New(stopwatch.WithInterval(time.Second))
				cmds = append(cmds, m.logs.watch.Start())
			}
		case streamRetrying:
			m.logs.live = false
			cmds = append(cmds, m.logs.watch.Stop())
		}
	case "stats":
		m.server.state, m.server.text, m.server.retry = x.state, x.text, x.retry
		switch x.state {
		case streamLive:
			m.server.live = true
		case streamRetrying:
			m.server.live = false
		}
	}
	cmds = append(cmds, m.wantSpinner())
	return tea.Batch(cmds...)
}

// startLogs follows the logs of the chosen app.
func (m *Model) startLogs() tea.Cmd {
	m.stopLogs()
	ctx, cancel := context.WithCancel(m.c.ctx)
	m.logs.cancel = cancel
	m.logs.gen++
	gen, app := m.logs.gen, m.logs.app
	m.logs.reset()
	ch := make(chan tea.Msg, 256)
	go func() {
		defer close(ch)
		m.c.streamLogs(ctx, app, gen, channelSender(ctx, ch))
	}()
	return tea.Batch(listen(ch, "logs", gen), m.wantSpinner())
}

func (m *Model) stopLogs() {
	if m.logs.cancel != nil {
		m.logs.cancel()
		m.logs.cancel = nil
	}
	m.logs.live = false
	m.logs.gen++
}

// startStats follows the host's samples and seeds the history.
func (m *Model) startStats() tea.Cmd {
	m.stopStats()
	ctx, cancel := context.WithCancel(m.c.ctx)
	m.server.cancel = cancel
	m.server.gen++
	gen := m.server.gen
	m.server.state, m.server.live = streamConnecting, false
	ch := make(chan tea.Msg, 64)
	go func() {
		defer close(ch)
		m.c.streamStats(ctx, gen, channelSender(ctx, ch))
	}()
	return tea.Batch(listen(ch, "stats", gen), m.start("stats", m.c.fetchStatsSeed()), m.start("df", m.c.fetchDF()))
}

func (m *Model) stopStats() {
	if m.server.cancel != nil {
		m.server.cancel()
		m.server.cancel = nil
	}
	m.server.live = false
	m.server.gen++
}

// shutdown ends the streams.
func (m *Model) shutdown() {
	m.stopLogs()
	m.stopStats()
}

// setTab moves to a tab and starts or ends the streams that belong to it.
func (m *Model) setTab(t tab) tea.Cmd {
	if t == m.active {
		return nil
	}
	switch m.active {
	case tabLogs:
		m.stopLogs()
	case tabServer:
		m.stopStats()
	}
	m.active = t
	m.layout()
	switch t {
	case tabLogs:
		if m.logs.app != "" {
			return m.startLogs()
		}
	case tabServer:
		return m.startStats()
	case tabApprovals:
		return m.start("approvals", m.c.fetchApprovals())
	}
	return nil
}

// typing is true while a text field has the keyboard, so letters are not commands.
func (m *Model) typing() bool {
	switch m.active {
	case tabApps:
		return m.apps.filtering
	case tabLogs:
		return m.logs.settingFilter()
	case tabApprovals:
		return m.approvals.detail == nil && m.approvals.list.SettingFilter()
	}
	return false
}

// key handles a key press.
func (m *Model) key(msg tea.KeyPressMsg) tea.Cmd {
	k := m.keys
	if msg.String() == "ctrl+c" {
		m.shutdown()
		return tea.Quit
	}
	if m.overlay != nil {
		var c tea.Cmd
		m.overlay, c = m.overlay.Update(msg)
		return c
	}
	if !m.typing() {
		switch {
		case key.Matches(msg, k.Quit):
			m.shutdown()
			return tea.Quit
		case key.Matches(msg, k.Help):
			m.help.ShowAll = !m.help.ShowAll
			m.layout()
			return nil
		case key.Matches(msg, k.NextTab):
			return m.setTab((m.active + 1) % 4)
		case key.Matches(msg, k.PrevTab):
			return m.setTab((m.active + 3) % 4)
		case key.Matches(msg, k.GoTab):
			return m.setTab(tab(msg.String()[0] - '1'))
		case key.Matches(msg, k.Refresh):
			return m.refreshNow()
		}
	}
	switch m.active {
	case tabApps:
		return m.keyApps(msg)
	case tabLogs:
		return m.keyLogs(msg)
	case tabServer:
		var c tea.Cmd
		m.server.table, c = m.server.table.Update(msg)
		return c
	}
	return m.keyApprovals(msg)
}

func (m *Model) keyApps(msg tea.KeyPressMsg) tea.Cmd {
	k := m.keys
	if d := m.detail; d != nil {
		switch {
		case key.Matches(msg, k.Back):
			m.detail = nil
			m.layout()
			return nil
		case key.Matches(msg, k.Logs):
			return m.openLogs(d.app)
		}
		var c tea.Cmd
		d.vp, c = d.vp.Update(msg)
		return c
	}
	if !m.apps.filtering {
		switch {
		case key.Matches(msg, k.Open):
			if app, ok := m.apps.selected(); ok {
				m.detail = newDetail(app, m.appPath(app))
				m.layout()
				return m.start("history", m.c.fetchHistory(m.detail.path))
			}
			return nil
		case key.Matches(msg, k.Logs):
			if app, ok := m.apps.selected(); ok {
				return m.openLogs(app)
			}
			return nil
		}
	}
	c, _ := m.apps.update(msg, k)
	return c
}

// openLogs goes to the logs tab with an app chosen.
func (m *Model) openLogs(app api.App) tea.Cmd {
	m.logs.app, m.logs.label = m.appPath(app), sanitize(app.Ref.Name)
	m.logs.pattern, m.logs.re, m.logs.reErr = "", nil, false
	m.logs.grep.Reset()
	if m.active == tabLogs {
		return m.startLogs()
	}
	m.stopLogs()
	cmd := m.setTab(tabLogs)
	return cmd
}

func (m *Model) keyLogs(msg tea.KeyPressMsg) tea.Cmd {
	k := m.keys
	if m.logs.app == "" {
		if !m.logs.picker.SettingFilter() && key.Matches(msg, k.Open) {
			it, ok := m.logs.selectedItem()
			if !ok {
				return nil
			}
			m.logs.app, m.logs.label = it.path, it.name
			m.logs.pattern, m.logs.re, m.logs.reErr = "", nil, false
			m.logs.grep.Reset()
			return m.startLogs()
		}
		var c tea.Cmd
		m.logs.picker, c = m.logs.picker.Update(msg)
		return c
	}
	if !m.logs.grepping && m.logs.pattern == "" && key.Matches(msg, k.Back) {
		m.stopLogs()
		m.logs.app = ""
		m.logs.lines = nil
		return nil
	}
	c, _ := m.logs.updateView(msg, k)
	return c
}

func (m *Model) keyApprovals(msg tea.KeyPressMsg) tea.Cmd {
	k := m.keys
	a := &m.approvals
	if d := a.detail; d != nil {
		if key.Matches(msg, k.Back) {
			a.detail = nil
			return nil
		}
		var c tea.Cmd
		d.vp, c = d.vp.Update(msg)
		return c
	}
	if !a.list.SettingFilter() && key.Matches(msg, k.Open) {
		if row, ok := a.selected(); ok {
			d, c := newApprovalDetail(m.st, row, m.now())
			a.detail = d
			iw := max(m.width-2, 4)
			d.resize(m.st, iw, max(m.bodyH-2, 1), len(d.header(m.st, m.now(), iw)))
			return c
		}
		return nil
	}
	var c tea.Cmd
	a.list, c = a.list.Update(msg)
	return c
}

// View draws the interface.
func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "berthy · " + sanitize(m.env)
	return v
}

func (m *Model) render() string {
	if m.width < minWidth || m.height < minHeight {
		msg := fmt.Sprintf("berthy needs a terminal of at least %d×%d\n(this one is %d×%d)", minWidth, minHeight, m.width, m.height)
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, m.st.muted.Render(msg))
	}
	body := m.body(m.width, m.bodyH)
	if m.overlay != nil {
		body = lipgloss.Place(m.width, m.bodyH, lipgloss.Center, lipgloss.Center, m.overlay.View(m.st, m.width, m.bodyH))
	}
	backups := summarizeBackups(m.apps.apps, m.now())
	parts := []string{
		m.viewHeader(),
		m.viewTabs(),
		body,
		m.status.view(m.st, m.now(), m.width, backups, m.status.spin.View(), m.lastRefresh),
		m.helpView(),
	}
	return fitLines(strings.Join(parts, "\n"), m.width, m.height)
}

func (m *Model) viewHeader() string {
	st := m.st
	sp := st.header.Render(" ")
	left := sp + st.brand.Render("berthy") + sp + sp + st.pillOn(sanitize(m.env), st.p.accent, st.p.surface)
	if m.secure {
		left += sp + st.pillOn("protected", st.p.warn, st.p.surface)
	}
	right := ""
	if m.host != "" {
		right = st.hostInfo.Render(clip(sanitize(m.host), 30)) + sp
	}
	gap := max(m.width-lipgloss.Width(left)-lipgloss.Width(right), 0)
	return left + st.header.Render(strings.Repeat(" ", gap)) + right
}

func (m *Model) viewTabs() string {
	st := m.st
	var b strings.Builder
	b.WriteString(" ")
	for i, name := range tabNames {
		label := fmt.Sprintf("%d %s", i+1, name)
		if tab(i) == m.active {
			edge := lipgloss.NewStyle().Foreground(st.p.accent)
			b.WriteString(edge.Render("▐") + st.tabActive.Render(" "+label+" ") + edge.Render("▌"))
		} else {
			b.WriteString("  " + st.tabIdle.Render(label) + " ")
		}
		if tab(i) == tabApprovals && m.status.pendingKnown && m.status.pending > 0 {
			b.WriteString(st.tabBadge.Render(fmt.Sprintf(" %d ", m.status.pending)))
		}
	}
	hint := st.faint.Render("? help ")
	return between(b.String(), hint, m.width)
}

// pane draws a bordered box w by h cells with a title on its top edge, and right-aligned text next to it.
func (st styles) pane(title, right, content string, w, h int) string {
	b := lipgloss.RoundedBorder()
	iw := max(w-2, 0)
	line := func(s string) string { return st.paneBorder.Render(s) }
	t := " " + st.paneTitle.Render(title) + " "
	r := ""
	if right != "" {
		r = " " + right + " "
	}
	if lipgloss.Width(t)+lipgloss.Width(r)+2 > iw {
		r = ""
	}
	if lipgloss.Width(t)+2 > iw {
		t = clip(t, max(iw-2, 0))
	}
	fill := max(iw-lipgloss.Width(t)-lipgloss.Width(r)-1, 0)
	top := line(b.TopLeft+b.Top) + t + line(strings.Repeat(b.Top, fill)) + r + line(b.TopRight)
	rows := strings.Split(fitLines(content, iw, max(h-2, 0)), "\n")
	out := make([]string, 0, h)
	out = append(out, top)
	for _, row := range rows[:min(len(rows), max(h-2, 0))] {
		out = append(out, line(b.Left)+row+line(b.Right))
	}
	out = append(out, line(b.BottomLeft+strings.Repeat(b.Bottom, iw)+b.BottomRight))
	return strings.Join(out, "\n")
}

// body draws the active tab in w by h cells.
func (m *Model) body(w, h int) string {
	st := m.st
	iw, ih := max(w-2, 1), max(h-2, 1)
	switch m.active {
	case tabApps:
		if d := m.detail; d != nil {
			d.render(st, m.now())
			pct := fmt.Sprintf("%3.0f%%", d.vp.ScrollPercent()*100)
			return st.pane("Apps "+glyphArrow+" "+sanitize(d.app.Ref.Name), st.faint.Render(pct), d.vp.View(), w, h)
		}
		if !m.apps.loaded {
			return st.pane("Apps", "", m.loadingView(iw, ih, "Reading the apps…"), w, h)
		}
		m.apps.refresh(st, m.now(), iw)
		right := ""
		if len(m.apps.shown) > 0 {
			right = st.faint.Render(fmt.Sprintf("%d/%d", m.apps.table.Cursor()+1, len(m.apps.shown)))
		}
		return st.pane("Apps", right, m.apps.view(st, iw, ih), w, h)
	case tabLogs:
		spin := m.status.spin.View()
		if m.logs.app == "" {
			return st.pane("Logs", st.faint.Render("choose an app"), m.logs.view(st, spin, iw, ih), w, h)
		}
		return st.pane("Logs "+glyphArrow+" "+m.logs.label, m.logs.status(st, spin), m.logs.view(st, spin, iw, ih), w, h)
	case tabServer:
		return m.viewServer(w, h)
	}
	a := &m.approvals
	if d := a.detail; d != nil {
		head := strings.Join(d.header(st, m.now(), iw), "\n")
		short := d.row.ID
		return st.pane("Approvals "+glyphArrow+" "+sanitize(short[:min(len(short), 8)]), st.faint.Render(fmt.Sprintf("%3.0f%%", d.vp.ScrollPercent()*100)), head+"\n"+d.vp.View(), w, h)
	}
	if !a.loaded {
		return st.pane("Approvals", "", m.loadingView(iw, ih, "Reading the requests…"), w, h)
	}
	right := ""
	if m.status.pendingKnown {
		right = st.warn.Render(fmt.Sprintf("%d pending", m.status.pending))
		if m.status.pending == 0 {
			right = st.muted.Render("0 pending")
		}
	}
	return st.pane("Approvals", right, a.view(st, iw, ih), w, h)
}

// loadingView is a spinner and a line, in the middle of a pane.
func (m *Model) loadingView(w, h int, text string) string {
	if m.status.unreachable {
		return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, m.st.err.Render("The agent cannot be reached yet."))
	}
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, m.status.spin.View()+m.st.muted.Render(" "+text))
}

// viewServer draws the server tab: the host's meters, the disks and the containers.
func (m *Model) viewServer(w, h int) string {
	st := m.st
	s := &m.server
	spin := m.status.spin.View()
	iw := max(w-2, 1)
	if _, ok := s.latest(); !ok {
		text := "Waiting for the first sample…"
		if s.state == streamRetrying {
			text = "No sample yet."
		}
		return st.pane("Server", s.status(st, spin), m.loadingView(iw, max(h-2, 1), text), w, h)
	}
	animate := !m.noAnim
	const minTable = 6
	var top string
	var topH int
	if w >= 100 {
		lw := w * 55 / 100
		rw := w - lw
		left, right := s.hostLines(st, lw-4, animate), s.diskLines(st, rw-4, animate)
		topH = min(max(len(left), len(right))+2, max(h-minTable, 3))
		top = lipgloss.JoinHorizontal(lipgloss.Top,
			st.pane("Host", s.status(st, spin), padLines(left), lw, topH),
			st.pane("Disks", "", padLines(right), rw, topH))
	} else {
		lines := append(s.hostLines(st, iw-2, animate), s.diskLines(st, iw-2, animate)...)
		topH = min(len(lines)+2, max(h-minTable, 3))
		top = st.pane("Host", s.status(st, spin), padLines(lines), w, topH)
	}
	tableH := max(h-topH, 3)
	cols := containerColumns(iw)
	cursor := max(s.table.Cursor(), 0)
	rows := s.containerRows(st)
	s.table.SetRows(nil)
	s.table.SetColumns(cols)
	s.table.SetRows(rows)
	s.table.SetCursor(cursor)
	s.table.SetWidth(iw)
	s.table.SetHeight(max(tableH-2, 2))
	s.table.UpdateViewport()
	n := len(s.table.Rows())
	right := st.faint.Render(fmt.Sprintf("%s, busiest first", plural(n, "container")))
	return top + "\n" + st.pane("Containers", right, s.table.View(), w, tableH)
}

// padLines joins lines with a margin of one cell on the left.
func padLines(lines []string) string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = " " + l
	}
	return strings.Join(out, "\n")
}
