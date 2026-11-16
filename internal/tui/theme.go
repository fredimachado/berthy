package tui

import (
	"image/color"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"
)

// Glyphs. Each state has a glyph as well as a colour, so nothing depends on colour alone (NO_COLOR, colour blindness).
const (
	glyphHealthy   = "●"
	glyphStarting  = "◐"
	glyphUnhealthy = "✖"
	glyphNone      = "○"
	glyphOK        = "✔"
	glyphFail      = "✖"
	glyphLock      = "🔒"
	glyphArrow     = "→"
	glyphDot       = "·"
	glyphCursor    = "▌"
)

// palette is the one set of colours of the interface: an accent, the four semantic colours and the neutrals. Every colour is
// chosen for a dark or a light terminal.
type palette struct {
	accent, onAccent                    color.Color
	ok, warn, err, muted, text, faint   color.Color
	surface, selected, border, pillText color.Color
	info                                color.Color
	dark                                bool
}

func newPalette(isDark bool) palette {
	ld := lipgloss.LightDark(isDark)
	return palette{
		dark:     isDark,
		accent:   ld(lipgloss.Color("#5B3FD9"), lipgloss.Color("#9D8CFF")),
		onAccent: ld(lipgloss.Color("#FFFFFF"), lipgloss.Color("#16131F")),
		ok:       ld(lipgloss.Color("#15803D"), lipgloss.Color("#4ADE80")),
		warn:     ld(lipgloss.Color("#B45309"), lipgloss.Color("#FACC15")),
		err:      ld(lipgloss.Color("#B91C1C"), lipgloss.Color("#F87171")),
		info:     ld(lipgloss.Color("#0E7490"), lipgloss.Color("#67E8F9")),
		muted:    ld(lipgloss.Color("#6B7280"), lipgloss.Color("#8B93A7")),
		faint:    ld(lipgloss.Color("#9CA3AF"), lipgloss.Color("#555C70")),
		text:     ld(lipgloss.Color("#1F2937"), lipgloss.Color("#E5E7EB")),
		surface:  ld(lipgloss.Color("#E8E6F5"), lipgloss.Color("#221F33")),
		selected: ld(lipgloss.Color("#DDD6FE"), lipgloss.Color("#34305A")),
		border:   ld(lipgloss.Color("#C4C0DA"), lipgloss.Color("#3E3A5C")),
		pillText: ld(lipgloss.Color("#FFFFFF"), lipgloss.Color("#0B0A12")),
	}
}

// styles are the Lip Gloss styles built from a palette.
type styles struct {
	p palette

	header, brand, hostInfo        lipgloss.Style
	tabActive, tabIdle, tabBadge   lipgloss.Style
	paneBorder, paneTitle, paneSub lipgloss.Style
	ok, warn, err, muted, text     lipgloss.Style
	faint, accent, bold, info      lipgloss.Style
	heading, key                   lipgloss.Style
	status                         lipgloss.Style
	selectedRow                    lipgloss.Style
}

func newStyles(p palette) styles {
	s := styles{p: p}
	s.header = lipgloss.NewStyle().Background(p.surface).Foreground(p.text)
	s.brand = lipgloss.NewStyle().Background(p.surface).Foreground(p.accent).Bold(true)
	s.hostInfo = lipgloss.NewStyle().Background(p.surface).Foreground(p.muted)
	s.tabActive = lipgloss.NewStyle().Background(p.accent).Foreground(p.onAccent).Bold(true)
	s.tabIdle = lipgloss.NewStyle().Foreground(p.muted)
	s.tabBadge = lipgloss.NewStyle().Background(p.warn).Foreground(p.pillText).Bold(true)
	s.paneBorder = lipgloss.NewStyle().Foreground(p.border)
	s.paneTitle = lipgloss.NewStyle().Foreground(p.accent).Bold(true)
	s.paneSub = lipgloss.NewStyle().Foreground(p.muted)
	s.ok = lipgloss.NewStyle().Foreground(p.ok)
	s.warn = lipgloss.NewStyle().Foreground(p.warn)
	s.err = lipgloss.NewStyle().Foreground(p.err)
	s.info = lipgloss.NewStyle().Foreground(p.info)
	s.muted = lipgloss.NewStyle().Foreground(p.muted)
	s.faint = lipgloss.NewStyle().Foreground(p.faint)
	s.text = lipgloss.NewStyle().Foreground(p.text)
	s.accent = lipgloss.NewStyle().Foreground(p.accent)
	s.bold = lipgloss.NewStyle().Bold(true)
	s.heading = lipgloss.NewStyle().Foreground(p.accent).Bold(true)
	s.key = lipgloss.NewStyle().Foreground(p.accent)
	s.status = lipgloss.NewStyle().Foreground(p.muted)
	s.selectedRow = lipgloss.NewStyle().Background(p.selected).Bold(true)
	return s
}

// pill is a short label on a coloured background, with half-block ends that round it off.
func (s styles) pill(text string, bg color.Color) string { return s.pillOn(text, bg, nil) }

// pillOn is a pill drawn on a background of another colour, such as the header bar.
func (s styles) pillOn(text string, bg, behind color.Color) string {
	body := lipgloss.NewStyle().Background(bg).Foreground(s.p.pillText).Bold(true).Render(" " + text + " ")
	edge := lipgloss.NewStyle().Foreground(bg)
	if behind != nil {
		edge = edge.Background(behind)
	}
	return edge.Render("▐") + body + edge.Render("▌")
}

// statusPill is the pill of an approval's status.
func (s styles) statusPill(status string) string {
	switch status {
	case "pending":
		return s.pill(status, s.p.warn)
	case "approved":
		return s.pill(status, s.p.ok)
	case "rejected", "failed":
		return s.pill(status, s.p.err)
	}
	return s.pill(status, s.p.muted)
}

// tableStyles are the styles of the bubbles tables.
func (s styles) tableStyles() table.Styles {
	t := table.DefaultStyles()
	t.Header = lipgloss.NewStyle().Foreground(s.p.muted).Bold(true).Padding(0, 1)
	t.Cell = lipgloss.NewStyle().Padding(0, 1)
	t.Selected = lipgloss.NewStyle().Background(s.p.selected).Bold(true)
	return t
}

// appTableStyles are the styles of the apps table: no padding, because its cells pad themselves.
func (s styles) appTableStyles() table.Styles {
	t := table.DefaultStyles()
	t.Header = lipgloss.NewStyle().Foreground(s.p.muted).Bold(true)
	t.Cell = lipgloss.NewStyle()
	t.Selected = lipgloss.NewStyle()
	return t
}

func (s styles) helpStyles() help.Styles {
	h := help.DefaultStyles(s.isDark())
	h.ShortKey = lipgloss.NewStyle().Foreground(s.p.accent)
	h.ShortDesc = lipgloss.NewStyle().Foreground(s.p.muted)
	h.ShortSeparator = lipgloss.NewStyle().Foreground(s.p.faint)
	h.FullKey = h.ShortKey
	h.FullDesc = h.ShortDesc
	h.FullSeparator = h.ShortSeparator
	h.Ellipsis = h.ShortSeparator
	return h
}

func (s styles) listStyles() list.Styles {
	l := list.DefaultStyles(s.isDark())
	l.Title = lipgloss.NewStyle().Foreground(s.p.accent).Bold(true)
	l.TitleBar = lipgloss.NewStyle().Padding(0, 0, 0, 1)
	l.StatusBar = lipgloss.NewStyle().Foreground(s.p.muted).Padding(0, 0, 0, 1)
	l.StatusEmpty = lipgloss.NewStyle().Foreground(s.p.faint)
	l.StatusBarActiveFilter = lipgloss.NewStyle().Foreground(s.p.accent)
	l.StatusBarFilterCount = lipgloss.NewStyle().Foreground(s.p.faint)
	l.NoItems = lipgloss.NewStyle().Foreground(s.p.muted).Padding(0, 0, 0, 2)
	l.PaginationStyle = lipgloss.NewStyle().Padding(0, 0, 0, 1)
	l.ActivePaginationDot = lipgloss.NewStyle().Foreground(s.p.accent).SetString("•")
	l.InactivePaginationDot = lipgloss.NewStyle().Foreground(s.p.faint).SetString("•")
	l.DividerDot = lipgloss.NewStyle().Foreground(s.p.faint).SetString(" • ")
	l.DefaultFilterCharacterMatch = lipgloss.NewStyle().Foreground(s.p.accent).Underline(true)
	l.Filter.Focused.Prompt = lipgloss.NewStyle().Foreground(s.p.accent)
	l.Filter.Focused.Text = lipgloss.NewStyle().Foreground(s.p.text)
	l.Filter.Blurred.Prompt = lipgloss.NewStyle().Foreground(s.p.muted)
	l.Filter.Blurred.Text = lipgloss.NewStyle().Foreground(s.p.muted)
	return l
}

func (s styles) inputStyles() textinput.Styles {
	t := textinput.DefaultStyles(s.isDark())
	t.Focused.Prompt = lipgloss.NewStyle().Foreground(s.p.accent)
	t.Focused.Text = lipgloss.NewStyle().Foreground(s.p.text)
	t.Focused.Placeholder = lipgloss.NewStyle().Foreground(s.p.faint)
	t.Blurred.Prompt = lipgloss.NewStyle().Foreground(s.p.muted)
	t.Blurred.Text = lipgloss.NewStyle().Foreground(s.p.muted)
	t.Blurred.Placeholder = lipgloss.NewStyle().Foreground(s.p.faint)
	return t
}

// isDark tells which kind of palette these styles were built from.
func (s styles) isDark() bool { return s.p.dark }
