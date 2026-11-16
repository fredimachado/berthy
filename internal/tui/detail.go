package tui

import (
	"fmt"
	"strings"
	"time"

	"berthy/internal/api"

	"charm.land/bubbles/v2/viewport"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/tree"
)

// detailModel is the detail of one app: a scrollable view of its roles, replicas, deploys, jobs and backups.
type detailModel struct {
	vp viewport.Model
	// key identifies the app across refreshes; path is how the agent API names it.
	key, path  string
	app        api.App
	hist       []historyRow
	histLoaded bool
	histErr    string
}

func newDetail(app api.App, path string) *detailModel {
	vp := viewport.New()
	vp.MouseWheelEnabled = false
	return &detailModel{vp: vp, key: appKey(app), path: path, app: app}
}

// resize sets the viewport to the pane's inner size and redraws it.
func (d *detailModel) resize(st styles, now time.Time, w, h int) {
	d.vp.SetWidth(w)
	d.vp.SetHeight(h)
	d.render(st, now)
}

// render redraws the content for the current data, keeping the scroll position.
func (d *detailModel) render(st styles, now time.Time) {
	y := d.vp.YOffset()
	d.vp.SetContent(d.content(st, now, max(d.vp.Width()-2, 20)))
	d.vp.SetYOffset(y)
}

// section is a heading with the count of what it lists.
func (s styles) section(title string, note string) string {
	h := s.heading.Render(title)
	if note != "" {
		h += s.faint.Render("  " + note)
	}
	return h
}

func (d *detailModel) content(st styles, now time.Time, w int) string {
	var b strings.Builder
	line := func(s string) { b.WriteString(" " + s + "\n") }
	app := d.app
	sum := summarize(app, now)
	line(st.bold.Render(sanitize(app.Ref.Name)) + st.faint.Render("  ") + st.muted.Render(app.Ref.Kind) + st.faint.Render("  "+glyphDot+"  ") +
		st.healthStyle(sum.health).Render(sum.health.glyph()+" "+sum.health.label()) + st.faint.Render("  "+glyphDot+"  ") +
		st.muted.Render(fmt.Sprintf("%d/%d running", sum.running, sum.desired)) + st.faint.Render("  "+glyphDot+"  ") + st.muted.Render(sanitize(sum.version)))
	b.WriteString("\n")

	// Roles and their replicas.
	line(st.section("Roles", plural(len(app.Roles), "role")))
	if len(app.Roles) == 0 {
		line(st.faint.Render("  no roles"))
	}
	for _, role := range app.Roles {
		desired := role.Running
		if role.Desired != nil {
			desired = int(*role.Desired)
		}
		rs := st.ok
		if role.Running < desired {
			rs = st.err
		}
		label := st.bold.Render(sanitize(role.Name)) + "  " + rs.Render(fmt.Sprintf("%d/%d running", role.Running, desired))
		t := tree.Root(label).Enumerator(tree.RoundedEnumerator).EnumeratorStyle(st.faint).RootStyle(lipgloss.NewStyle())
		for _, r := range role.Replicas {
			t.Child(" " + replicaLine(st, r))
		}
		for _, l := range strings.Split(t.String(), "\n") {
			line("  " + l)
		}
	}
	b.WriteString("\n")

	// Recent deploys.
	hist := d.hist
	if len(hist) > 10 {
		hist = hist[:10]
	}
	switch {
	case d.histErr != "":
		line(st.section("Recent deploys", ""))
		line("  " + st.err.Render(sanitize(d.histErr)))
	case !d.histLoaded:
		line(st.section("Recent deploys", "loading"))
	default:
		line(st.section("Recent deploys", fmt.Sprintf("last %d", len(hist))))
		if len(hist) == 0 {
			line(st.faint.Render("  no deploys recorded"))
		}
		for _, h := range hist {
			for _, l := range deployLines(st, now, h, w-2) {
				line("  " + l)
			}
		}
	}
	b.WriteString("\n")

	// Jobs.
	line(st.section("Jobs", plural(len(app.Jobs), "job")))
	if len(app.Jobs) == 0 {
		line(st.faint.Render("  no jobs"))
	}
	for _, j := range app.Jobs {
		for _, l := range jobLines(st, now, j, w-2) {
			line("  " + l)
		}
	}
	b.WriteString("\n")

	// Backups.
	line(st.section("Backups", plural(len(app.Backups), "backup")))
	if len(app.Backups) == 0 {
		line(st.faint.Render("  no backups"))
	}
	for _, bk := range app.Backups {
		for _, l := range backupLines(st, now, bk, w-2) {
			line("  " + l)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// replicaLine is one replica of a role: its state, health, restarts and image.
func replicaLine(st styles, r api.Replica) string {
	h := replicaHealth(r)
	parts := []string{st.healthStyle(h).Render(h.glyph() + " " + sanitize(r.State))}
	hl := r.Health
	if hl == "" || hl == "none" || hl == "unknown" {
		hl = "no health check"
	}
	parts = append(parts, st.healthStyle(h).Render(sanitize(hl)))
	if r.RestartCount != nil {
		rc := fmt.Sprintf("restarts %d", *r.RestartCount)
		if *r.RestartCount > 0 {
			parts = append(parts, st.warn.Render(rc))
		} else {
			parts = append(parts, st.muted.Render(rc))
		}
	}
	if r.OOMKilled != nil && *r.OOMKilled {
		parts = append(parts, st.err.Bold(true).Render("OOM killed"))
	}
	parts = append(parts, st.muted.Render(sanitize(shortImage(r.Image))))
	id := r.ContainerID
	if id == "" {
		id = r.TaskID
	}
	if id != "" {
		parts = append(parts, st.faint.Render(sanitize(id[:min(len(id), 12)])))
	}
	return strings.Join(parts, st.faint.Render(" "+glyphDot+" "))
}

// wrapIndent wraps text to w cells and indents the lines after the first by indent cells.
func wrapIndent(text string, w, indent int) []string {
	text = sanitize(text)
	if w < 10 {
		return []string{text}
	}
	wrapped := lipgloss.NewStyle().Width(w).Render(text)
	lines := strings.Split(wrapped, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
		if i > 0 {
			lines[i] = strings.Repeat(" ", indent) + lines[i]
		}
	}
	return lines
}

// reasonLines is a reason in red, wrapped to w cells: an arrow in front of the first line, an indent in front of the others.
func reasonLines(st styles, text string, indent, w int) []string {
	var out []string
	for i, l := range wrapIndent(text, w, 0) {
		lead := "  "
		if i == 0 {
			lead = glyphArrow + " "
		}
		out = append(out, strings.Repeat(" ", indent)+st.err.Render(lead+l))
	}
	return out
}

// deployLines is one deploy of the history: when, what, and for a failed one the reason.
func deployLines(st styles, now time.Time, h historyRow, w int) []string {
	when := fit(agoShort(now, h.Created)+" ago", 9)
	what := h.Digest
	switch {
	case what == "":
	case strings.Contains(what, "@"):
		what = shortImage(what)
	default:
		what = shortImage("x@" + what)
	}
	commit := h.Commit
	if commit == "" {
		commit = "-"
	}
	if h.failed() {
		state := h.State
		if state == "" {
			state = "failed"
		}
		head := st.err.Render(glyphFail+" ") + st.muted.Render(when) + " " + st.err.Render(fit(sanitize(strings.ReplaceAll(state, "_", " ")), 12)) + " " + st.muted.Render(fit("commit "+sanitize(clip(commit, 12)), 19))
		lines := []string{head}
		if h.Reason != "" {
			indent := 4
			lines = append(lines, reasonLines(st, h.Reason, indent, max(w-indent-2, 20))...)
		}
		return lines
	}
	return []string{st.ok.Render(glyphOK+" ") + st.muted.Render(when) + " " + st.text.Render(fit(sanitize(what), 16)) + " " + st.muted.Render("commit "+sanitize(clip(commit, 12)))}
}

func jobLines(st styles, now time.Time, j api.JobStatus, w int) []string {
	head := st.bold.Render(sanitize(j.Name))
	if j.Schedule != "" {
		head += st.faint.Render("  ") + st.muted.Render(sanitize(j.Schedule))
	}
	if j.Next != nil {
		head += st.faint.Render("  ") + st.muted.Render("next in "+until(now, *j.Next))
	}
	lines := []string{head}
	if j.Last == nil {
		return append(lines, "    "+st.faint.Render("never ran"))
	}
	last := j.Last
	glyph, style := glyphOK, st.ok
	switch last.Status {
	case "succeeded":
	case "running":
		glyph, style = glyphStarting, st.warn
	default:
		glyph, style = glyphFail, st.err
	}
	text := style.Render(glyph+" "+sanitize(last.Status)) + st.muted.Render(" "+ago(now, last.Started))
	if last.ExitCode != nil {
		text += st.muted.Render(fmt.Sprintf(" · exit %d", *last.ExitCode))
	}
	lines = append(lines, "    "+text)
	if last.Detail != "" && last.Status != "succeeded" {
		lines = append(lines, reasonLines(st, last.Detail, 6, max(w-8, 20))...)
	}
	return lines
}

func backupLines(st styles, now time.Time, b api.BackupStatus, w int) []string {
	head := st.bold.Render(sanitize(b.Name)) + st.faint.Render("  ") + st.muted.Render("volume "+sanitize(b.Volume))
	if b.Schedule != "" {
		head += st.faint.Render("  ") + st.muted.Render(sanitize(b.Schedule))
	}
	lines := []string{head}
	var parts []string
	if b.LastSuccess != nil {
		parts = append(parts, st.ok.Render(glyphOK+" last success "+ago(now, *b.LastSuccess)))
		if b.Size > 0 {
			parts = append(parts, st.muted.Render(api.HumanSize(b.Size)))
		}
	} else {
		parts = append(parts, st.faint.Render("no successful backup yet"))
	}
	if b.LastStatus == "failed" {
		parts = append(parts, st.err.Render(glyphFail+" last run failed"))
	}
	lines = append(lines, "    "+strings.Join(parts, st.faint.Render(" "+glyphDot+" ")))
	if b.LastStatus == "failed" && b.Detail != "" {
		lines = append(lines, reasonLines(st, b.Detail, 6, max(w-8, 20))...)
	}
	return lines
}
