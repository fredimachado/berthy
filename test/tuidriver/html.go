//go:build unix

package main

import (
	"fmt"
	"html"
	"image/color"
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
)

// The colours of the page and of cells that name none.
const (
	defaultFg = "#c9d1d9"
	defaultBg = "#0d1117"
)

// screen is what the emulated terminal shows: a grid of cells.
type screen interface {
	Width() int
	Height() int
	CellAt(x, y int) *uv.Cell
}

// grid is a copy of a screen, taken at one moment.
type grid struct {
	w, h  int
	cells []*uv.Cell
}

func (g *grid) Width() int  { return g.w }
func (g *grid) Height() int { return g.h }
func (g *grid) CellAt(x, y int) *uv.Cell {
	if x < 0 || y < 0 || x >= g.w || y >= g.h {
		return nil
	}
	return g.cells[y*g.w+x]
}

// copyScreen copies every cell of s. The caller must keep s from changing meanwhile.
func copyScreen(s screen) *grid {
	g := &grid{w: s.Width(), h: s.Height()}
	g.cells = make([]*uv.Cell, g.w*g.h)
	for y := 0; y < g.h; y++ {
		for x := 0; x < g.w; x++ {
			if c := s.CellAt(x, y); c != nil {
				cp := *c
				g.cells[y*g.w+x] = &cp
			}
		}
	}
	return g
}

// hex is the CSS colour of a terminal colour, or def when it names none.
func hex(c color.Color, def string) string {
	if c == nil {
		return def
	}
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}

// cellText is what a cell shows, and false for the second half of a wide character, which shows nothing of its own.
func cellText(c *uv.Cell) (string, bool) {
	if c == nil {
		return " ", true
	}
	if c.Width == 0 && c.Content == "" {
		return "", false
	}
	if c.Content == "" {
		return " ", true
	}
	return c.Content, true
}

// plainText is the screen as lines of text, without trailing spaces or trailing empty lines.
func plainText(s screen) []string {
	lines := make([]string, 0, s.Height())
	for y := 0; y < s.Height(); y++ {
		var b strings.Builder
		for x := 0; x < s.Width(); x++ {
			if t, ok := cellText(s.CellAt(x, y)); ok {
				b.WriteString(t)
			}
		}
		lines = append(lines, strings.TrimRight(b.String(), " "))
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// cssFor is the inline style of a cell.
func cssFor(st uv.Style) string {
	fg, bg := hex(st.Fg, defaultFg), hex(st.Bg, defaultBg)
	if st.Attrs&uv.AttrReverse != 0 {
		fg, bg = bg, fg
	}
	var css []string
	if fg != defaultFg {
		css = append(css, "color:"+fg)
	}
	if bg != defaultBg {
		css = append(css, "background:"+bg)
	}
	if st.Attrs&uv.AttrBold != 0 {
		css = append(css, "font-weight:bold")
	}
	if st.Attrs&uv.AttrFaint != 0 {
		css = append(css, "opacity:.6")
	}
	if st.Attrs&uv.AttrItalic != 0 {
		css = append(css, "font-style:italic")
	}
	var deco []string
	if st.Underline != uv.UnderlineNone {
		deco = append(deco, "underline")
	}
	if st.Attrs&uv.AttrStrikethrough != 0 {
		deco = append(deco, "line-through")
	}
	if len(deco) > 0 {
		css = append(css, "text-decoration:"+strings.Join(deco, " "))
	}
	return strings.Join(css, ";")
}

// renderHTML is the screen as a page: every cell with its colours and attributes as inline styles, on a dark background, in a
// monospace font.
func renderHTML(s screen, title string) string {
	var b strings.Builder
	b.WriteString("<!doctype html>\n<html><head><meta charset=\"utf-8\"><title>" + html.EscapeString(title) + "</title>\n")
	b.WriteString("<style>body{margin:0;padding:16px;background:" + defaultBg + ";color:" + defaultFg + "}" +
		"h1{font:12px sans-serif;color:#8b949e;margin:0 0 8px}" +
		"pre{margin:0;font:14px/1.25 'JetBrains Mono','Cascadia Mono','DejaVu Sans Mono',Menlo,Consolas,monospace;" +
		"background:" + defaultBg + ";display:inline-block;padding:8px;border:1px solid #30363d;border-radius:6px}</style></head><body>\n")
	fmt.Fprintf(&b, "<h1>%s &middot; %d&times;%d</h1>\n<pre>", html.EscapeString(title), s.Width(), s.Height())
	height := s.Height()
	for height > 0 && rowBlank(s, height-1) {
		height--
	}
	for y := 0; y < max(height, 1); y++ {
		var run strings.Builder
		var runCSS string
		flush := func() {
			if run.Len() == 0 {
				return
			}
			if runCSS == "" {
				b.WriteString(html.EscapeString(run.String()))
			} else {
				fmt.Fprintf(&b, "<span style=\"%s\">%s</span>", runCSS, html.EscapeString(run.String()))
			}
			run.Reset()
		}
		for x := 0; x < s.Width(); x++ {
			c := s.CellAt(x, y)
			t, ok := cellText(c)
			if !ok {
				continue
			}
			css := ""
			if c != nil {
				css = cssFor(c.Style)
			}
			if css != runCSS {
				flush()
				runCSS = css
			}
			run.WriteString(t)
		}
		flush()
		b.WriteString("\n")
	}
	b.WriteString("</pre>\n</body></html>\n")
	return b.String()
}

// rowBlank is true for a row that shows nothing but default blanks.
func rowBlank(s screen, y int) bool {
	for x := 0; x < s.Width(); x++ {
		c := s.CellAt(x, y)
		if c == nil {
			continue
		}
		if t, ok := cellText(c); ok && strings.TrimSpace(t) != "" {
			return false
		}
		if cssFor(c.Style) != "" {
			return false
		}
	}
	return true
}
