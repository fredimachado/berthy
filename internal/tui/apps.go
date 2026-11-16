package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"berthy/internal/api"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// health is how well an app is: the worst health among its replicas.
type health int

const (
	healthNone health = iota
	healthOK
	healthStarting
	healthBad
)

func (h health) label() string {
	switch h {
	case healthOK:
		return "healthy"
	case healthStarting:
		return "starting"
	case healthBad:
		return "unhealthy"
	}
	return "none"
}

func (h health) glyph() string {
	switch h {
	case healthOK:
		return glyphHealthy
	case healthStarting:
		return glyphStarting
	case healthBad:
		return glyphUnhealthy
	}
	return glyphNone
}

func (s styles) healthStyle(h health) lipgloss.Style {
	switch h {
	case healthOK:
		return s.ok
	case healthStarting:
		return s.warn
	case healthBad:
		return s.err
	}
	return s.faint
}

// replicaHealth is the health of one replica, from its health check and its state.
func replicaHealth(r api.Replica) health {
	switch r.Health {
	case "healthy":
		return healthOK
	case "starting":
		return healthStarting
	case "unhealthy":
		return healthBad
	}
	switch r.State {
	case "running", "":
		return healthNone
	case "starting", "preparing", "assigned", "accepted", "pending", "new", "allocated", "ready", "created", "restarting":
		return healthStarting
	}
	return healthBad
}

// appSummary is what the apps table shows of an app.
type appSummary struct {
	running, desired int
	desiredKnown     bool
	health           health
	version          string
	roles            string
	// backupAge is the age of the oldest of the app's newest successful backups; zero when there is none.
	backupAge    time.Duration
	hasBackup    bool
	backupFailed bool
}

func summarize(app api.App, now time.Time) appSummary {
	var s appSummary
	versions := map[string]bool{}
	var order []string
	var names []string
	known := true
	for _, role := range app.Roles {
		names = append(names, role.Name)
		s.running += role.Running
		if role.Desired != nil {
			s.desired += int(*role.Desired)
			if role.Running < int(*role.Desired) {
				if role.Running == 0 {
					s.health = max(s.health, healthBad)
				} else {
					s.health = max(s.health, healthStarting)
				}
			}
		} else {
			known = false
			s.desired += role.Running
		}
		for _, r := range role.Replicas {
			s.health = max(s.health, replicaHealth(r))
			v := shortImage(r.Image)
			if !versions[v] {
				versions[v] = true
				order = append(order, v)
			}
		}
	}
	s.desiredKnown = known
	switch len(order) {
	case 0:
		s.version = "-"
	case 1:
		s.version = order[0]
	default:
		s.version = fmt.Sprintf("mixed (%d)", len(order))
	}
	s.roles = strings.Join(names, ",")
	for _, b := range app.Backups {
		if b.LastStatus == "failed" {
			s.backupFailed = true
		}
		if b.LastSuccess != nil {
			age := max(now.Sub(*b.LastSuccess), 0)
			if !s.hasBackup || age > s.backupAge {
				s.backupAge = age
			}
			s.hasBackup = true
		}
	}
	return s
}

// appKey identifies an app across refreshes.
func appKey(a api.App) string { return a.Ref.Kind + ":" + a.Ref.Name }

// appsModel is the apps tab: the table of apps and its filter.
type appsModel struct {
	table     table.Model
	filter    textinput.Model
	filtering bool
	query     string
	apps      []api.App
	shown     []int
	loaded    bool
}

func newAppsModel(st styles) appsModel {
	ti := textinput.New()
	ti.Prompt = "/ "
	ti.Placeholder = "filter apps by name"
	ti.SetVirtualCursor(true)
	ti.SetStyles(st.inputStyles())
	tb := table.New(table.WithFocused(true), table.WithStyles(st.appTableStyles()))
	return appsModel{table: tb, filter: ti}
}

// selected is the app under the cursor.
func (a *appsModel) selected() (api.App, bool) {
	i := a.table.Cursor()
	if i < 0 || i >= len(a.shown) {
		return api.App{}, false
	}
	return a.apps[a.shown[i]], true
}

// setApps replaces the apps and keeps the cursor on the same app.
func (a *appsModel) setApps(apps []api.App) {
	prev, had := a.selected()
	a.apps = apps
	a.loaded = true
	a.refilter()
	if had {
		for i, idx := range a.shown {
			if appKey(a.apps[idx]) == appKey(prev) {
				a.table.SetCursor(i)
				break
			}
		}
	}
}

// refilter works out which apps the filter shows. The filter text being typed counts as well as the one applied.
func (a *appsModel) refilter() {
	query := a.query
	if a.filtering {
		query = a.filter.Value()
	}
	a.shown = a.shown[:0]
	for i, app := range a.apps {
		if matches(app.Ref.Name, query) {
			a.shown = append(a.shown, i)
		}
	}
	sort.SliceStable(a.shown, func(i, j int) bool { return a.apps[a.shown[i]].Ref.Name < a.apps[a.shown[j]].Ref.Name })
	// The table needs to know how many rows there are for its cursor; refresh draws them.
	a.table.SetRows(make([]table.Row, len(a.shown)))
	if a.table.Cursor() >= len(a.shown) {
		a.table.SetCursor(max(len(a.shown)-1, 0))
	}
}

// appColumns lays the columns out for a table w cells wide. A column is drawn with a space on each side of its text, and its
// width counts both: the cells are padded here, not by the table, so that a highlighted row has no gaps.
func appColumns(w int) []table.Column {
	wide := w >= 100
	cols := []table.Column{{Title: "", Width: 3}, {Title: "App"}}
	if wide {
		cols = append(cols, table.Column{Title: "Kind", Width: 10}, table.Column{Title: "Roles"})
	}
	cols = append(cols,
		table.Column{Title: "Replicas", Width: 10},
		table.Column{Title: "Health", Width: 13},
		table.Column{Title: "Version", Width: 17},
		table.Column{Title: "Backup", Width: 12},
	)
	// Two columns share what the fixed ones leave.
	fixed := 0
	for _, c := range cols {
		fixed += c.Width
	}
	free := max(w-fixed, 0)
	for i := range cols {
		switch cols[i].Title {
		case "App":
			if wide {
				cols[i].Width = max(free*55/100, 12)
			} else {
				cols[i].Width = max(free, 12)
			}
		case "Roles":
			cols[i].Width = max(free-free*55/100, 8)
		}
		if cols[i].Title != "" {
			cols[i].Title = " " + cols[i].Title
		}
	}
	return cols
}

// rows renders the apps as table rows. The selected row is drawn with its own background in every cell, padding included, so
// the colour of a cell does not cut the highlight.
func (a *appsModel) rows(st styles, now time.Time, cols []table.Column) []table.Row {
	wide := len(cols) > 6
	rows := make([]table.Row, 0, len(a.shown))
	for n, idx := range a.shown {
		app := a.apps[idx]
		sum := summarize(app, now)
		sel := n == a.table.Cursor()
		col := 0
		cell := func(s lipgloss.Style, text string) string {
			width := cols[col].Width
			col++
			if sel {
				s = s.Background(st.p.selected).Bold(true)
			}
			return s.Render(" " + fit(text, width-2) + " ")
		}
		markerText := " "
		if sel {
			markerText = glyphCursor
		}
		marker := cell(st.accent, markerText)
		replicas := fmt.Sprintf("%d/%d", sum.running, sum.desired)
		rs := st.text
		if sum.running < sum.desired {
			rs = st.err
		}
		backupText, backupStyle := "-", st.faint
		switch {
		case sum.backupFailed && !sum.hasBackup:
			backupText, backupStyle = glyphFail+" failed", st.err
		case sum.backupFailed:
			backupText, backupStyle = glyphFail+" "+api.HumanAge(sum.backupAge), st.err
		case sum.hasBackup:
			backupText, backupStyle = api.HumanAge(sum.backupAge), st.text
		}
		row := table.Row{marker, cell(st.text, sanitize(app.Ref.Name))}
		if wide {
			row = append(row, cell(st.muted, app.Ref.Kind), cell(st.muted, sanitize(sum.roles)))
		}
		row = append(row,
			cell(rs, replicas),
			cell(st.healthStyle(sum.health), sum.health.glyph()+" "+sum.health.label()),
			cell(st.muted, sanitize(sum.version)),
			cell(backupStyle, backupText),
		)
		rows = append(rows, row)
	}
	return rows
}

// resize sets the table up for a pane with inner width w and height h.
func (a *appsModel) resize(w, h int) {
	a.filter.SetWidth(max(w-4, 10))
	a.table.SetWidth(w)
	a.table.SetHeight(max(h-1, 1))
}

// refresh redraws the table's rows for the current data, size and cursor.
func (a *appsModel) refresh(st styles, now time.Time, w int) {
	cols := appColumns(w)
	cursor := max(a.table.Cursor(), 0)
	rows := a.rows(st, now, cols)
	a.table.SetRows(nil)
	a.table.SetColumns(cols)
	a.table.SetRows(rows)
	a.table.SetCursor(cursor)
	a.table.UpdateViewport()
}

// update handles a key of the apps tab. It reports whether the key was used.
func (a *appsModel) update(msg tea.Msg, k keyMap) (cmd tea.Cmd, used bool) {
	if a.filtering {
		if km, ok := msg.(tea.KeyPressMsg); ok {
			switch {
			case key.Matches(km, k.Apply):
				a.filtering = false
				a.query = a.filter.Value()
				a.filter.Blur()
				a.refilter()
				return nil, true
			case key.Matches(km, k.Clear):
				a.filtering = false
				a.query = ""
				a.filter.Reset()
				a.filter.Blur()
				a.refilter()
				return nil, true
			}
		}
		var c tea.Cmd
		a.filter, c = a.filter.Update(msg)
		a.refilter()
		return c, true
	}
	if km, ok := msg.(tea.KeyPressMsg); ok {
		switch {
		case key.Matches(km, k.Filter):
			a.filtering = true
			return a.filter.Focus(), true
		case key.Matches(km, k.Clear) && a.query != "":
			a.query = ""
			a.filter.Reset()
			a.refilter()
			return nil, true
		}
	}
	var c tea.Cmd
	a.table, c = a.table.Update(msg)
	return c, false
}

// view draws the pane's content: the table, a summary and the filter.
func (a *appsModel) view(st styles, w, h int) string {
	if !a.loaded {
		return ""
	}
	var foot string
	switch {
	case a.filtering:
		foot = a.filter.View()
	case a.query != "":
		foot = st.muted.Render("filter: ") + st.accent.Render(sanitize(a.query)) + st.faint.Render("  (esc clears)")
	default:
		healthy, bad := 0, 0
		now := time.Time{}
		for _, app := range a.apps {
			switch summarize(app, now).health {
			case healthBad:
				bad++
			case healthOK:
				healthy++
			}
		}
		foot = st.muted.Render(plural(len(a.apps), "app")) + st.faint.Render(" "+glyphDot+" ") + st.ok.Render(fmt.Sprintf("%d healthy", healthy))
		if bad > 0 {
			foot += st.faint.Render(" "+glyphDot+" ") + st.err.Render(fmt.Sprintf("%d unhealthy", bad))
		}
	}
	if len(a.shown) == 0 {
		msg := st.muted.Render("No apps deployed yet.")
		if len(a.apps) > 0 {
			msg = st.muted.Render("No app matches the filter.")
		}
		body := lipgloss.Place(w, max(h-1, 1), lipgloss.Center, lipgloss.Center, msg)
		return body + "\n " + foot
	}
	return a.table.View() + "\n " + foot
}
