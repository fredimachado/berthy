package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/color"
	"sort"
	"strings"

	"charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"

	"berthy/internal/deploy"
)

// This file turns what an action will do into what the person reads before confirming: a small Markdown document that Glamour
// renders in the colours of the interface, and the Huh theme that makes the confirmation look like the rest of it.

// mdText makes agent text safe to put in a Markdown line. Glamour does not undo a backslash escape, so the text goes in a code span, where
// nothing is Markdown. A backtick cannot be in one, so it is swapped for a quote.
func mdText(s string) string {
	s = strings.ReplaceAll(sanitize(s), "`", "'")
	if strings.TrimSpace(s) == "" {
		s = "-"
	}
	return "`" + s + "`"
}

// mdLabel is a label of the facts: the name of a field of the plan, with only the characters that cannot be Markdown.
func mdLabel(s string) string {
	var b strings.Builder
	for _, r := range sanitize(s) {
		if r >= 0x80 || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == ' ' || r == '.' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// planFact is one line of the facts list: a label and a Markdown value.
type planFact struct {
	label, md string
	// short is what to show when md does not fit the width.
	short string
}

// splitDiff takes the diff out of a plan: what the deploy changes, as the text lines the CLI prints (made safe for the screen),
// and the plan without it. A plan that has no diff, or is not an object, comes back as it is with no lines.
func splitDiff(raw []byte) (lines []string, rest []byte) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil, raw
	}
	diffRaw, ok := obj["diff"]
	var d deploy.Diff
	if !ok || json.Unmarshal(diffRaw, &d) != nil {
		return nil, raw
	}
	delete(obj, "diff")
	rest, err := json.Marshal(obj)
	if err != nil {
		return nil, raw
	}
	for _, l := range d.Lines() {
		lines = append(lines, sanitize(l))
	}
	return lines, rest
}

// planFacts reads the meaningful top-level fields of a plan: the app, the image, the roles and whatever else the plan names.
// Anything that is not a plain value is pointed at the full plan below.
func planFacts(raw []byte) []planFact {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	order := []string{"action", "app", "version", "image", "roles", "command", "target", "name", "registry", "exists", "blockers"}
	rank := map[string]int{}
	for i, k := range order {
		rank[k] = i + 1
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ri, rj := rank[keys[i]], rank[keys[j]]
		if ri != rj {
			if ri == 0 || rj == 0 {
				return ri != 0
			}
			return ri < rj
		}
		return keys[i] < keys[j]
	})
	var out []planFact
	for _, k := range keys {
		v := bytes.TrimSpace(obj[k])
		if k == "action" || k == "diff" {
			continue
		}
		label := mdLabel(strings.ToUpper(k[:1]) + strings.ReplaceAll(k[1:], "_", " "))
		if label == "" {
			continue
		}
		var s string
		var list []string
		var num json.Number
		var flag bool
		switch {
		case string(v) == "null" || len(v) == 0:
			continue
		case json.Unmarshal(v, &s) == nil:
			switch {
			case s == "":
			case k == "image":
				out = append(out, imageFacts(label, s)...)
			default:
				out = append(out, planFact{label: label, md: mdText(s)})
			}
		case json.Unmarshal(v, &list) == nil:
			if len(list) == 0 {
				if k == "roles" {
					out = append(out, planFact{label: label, md: "*the whole app*"})
				}
				continue
			}
			parts := make([]string, len(list))
			for i, x := range list {
				parts[i] = mdText(x)
			}
			sep := ", "
			if k == "command" {
				sep = " "
			}
			out = append(out, planFact{label: label, md: strings.Join(parts, sep)})
		case json.Unmarshal(v, &flag) == nil:
			out = append(out, planFact{label: label, md: fmt.Sprintf("%t", flag)})
		case json.Unmarshal(v, &num) == nil:
			out = append(out, planFact{label: label, md: mdText(num.String())})
		default:
			out = append(out, planFact{label: label, md: "*see the full plan below*"})
		}
	}
	return out
}

// imageFacts is an image as the plan names it: the repository (with its tag when it has one), and the whole digest on its own line
// so that it is never cut; a digest that does not fit is shortened.
func imageFacts(label, image string) []planFact {
	repo, digest, hasDigest := strings.Cut(image, "@")
	if !hasDigest {
		return []planFact{{label: label, md: mdText(image)}}
	}
	return []planFact{{label: label, md: mdText(repo)}, {label: "Digest", md: mdText(digest), short: mdText(shortDigest(digest))}}
}

// fenceFor is a code fence that the text cannot close.
func fenceFor(text string) string {
	run, longest := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(longest+1, 3))
}

// prettyPlan is the plan as indented JSON with every line sanitized, or "" when it is not JSON.
func prettyPlan(raw []byte) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", "  ") != nil {
		return ""
	}
	lines := strings.Split(pretty.String(), "\n")
	for i, l := range lines {
		lines[i] = sanitize(l)
	}
	return strings.Join(lines, "\n")
}

// planDocument is the Markdown the person reads: a heading, the facts, what will happen, and the full plan as JSON.
func planDocument(heading string, facts []planFact, effect string, raw []byte, note string, w int) string {
	var b strings.Builder
	b.WriteString("### " + heading + "\n\n")
	for _, f := range facts {
		md := f.md
		if f.short != "" && !f.fits(w) {
			md = f.short
		}
		b.WriteString("- **" + f.label + "** " + md + "\n")
	}
	if effect != "" {
		b.WriteString("\n" + effect + "\n")
	}
	changes, raw := splitDiff(raw)
	if len(changes) > 0 {
		text := strings.Join(changes, "\n")
		fence := fenceFor(text)
		b.WriteString("\n#### What changes\n\n" + fence + "\n" + text + "\n" + fence + "\n")
	}
	if pretty := prettyPlan(raw); pretty != "" {
		fence := fenceFor(pretty)
		b.WriteString("\n#### Full plan\n\n" + fence + "json\n" + pretty + "\n" + fence + "\n")
	} else if note != "" {
		b.WriteString("\n" + note + "\n")
	}
	return b.String()
}

// hexOf is a colour as #rrggbb.
func hexOf(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}

// glamourStyle is the Markdown style of the plan, drawn from the palette so it looks like the rest of the interface.
func glamourStyle(st styles) ansi.StyleConfig {
	p := st.p
	col := func(c color.Color) *string { s := hexOf(c); return &s }
	yes, no := true, false
	zero := uint(0)
	two := uint(2)
	prim := func(c color.Color) ansi.StylePrimitive { return ansi.StylePrimitive{Color: col(c)} }
	return ansi.StyleConfig{
		Document:  ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: col(p.text)}, Margin: &zero},
		Paragraph: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: col(p.muted)}},
		List:      ansi.StyleList{StyleBlock: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: col(p.text)}}, LevelIndent: 2},
		Item:      ansi.StylePrimitive{BlockPrefix: glyphDot + " "},
		Heading:   ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: col(p.accent), Bold: &yes, BlockSuffix: "\n"}},
		H3:        ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: col(p.accent), Bold: &yes}},
		H4:        ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: col(p.muted), Bold: &yes}},
		Strong:    ansi.StylePrimitive{Color: col(p.muted), Bold: &no},
		Emph:      ansi.StylePrimitive{Color: col(p.faint), Italic: &yes},
		Code:      ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: col(p.text)}},
		CodeBlock: ansi.StyleCodeBlock{
			StyleBlock: ansi.StyleBlock{StylePrimitive: prim(p.text), Margin: &two},
			Chroma: &ansi.Chroma{
				Text:          prim(p.text),
				Error:         prim(p.err),
				Comment:       prim(p.faint),
				Keyword:       prim(p.info),
				Punctuation:   prim(p.faint),
				Name:          prim(p.text),
				NameTag:       prim(p.accent),
				NameAttribute: prim(p.accent),
				Literal:       prim(p.text),
				LiteralString: prim(p.text),
				LiteralNumber: prim(p.info),
			},
		},
	}
}

// renderMarkdown draws a document with Glamour for a pane w cells wide: the lines, each at most w cells, without the blank
// lines Glamour puts around a document. The text is wrapped by Glamour at words. A fenced block is rendered apart, without any
// wrapping, and its long lines continue under their own value, so a digest is never cut where it cannot be read.
func renderMarkdown(st styles, md string, w int) []string {
	w = max(w, 20)
	text, block, hasBlock := strings.Cut(md, "\n#### Full plan\n")
	lines := renderPart(st, text, w, false)
	if hasBlock {
		lines = append(lines, "")
		lines = append(lines, renderPart(st, "#### Full plan\n"+block, w, true)...)
	}
	return lines
}

// renderPart renders a part of the document. A part that must not be wrapped is given room for its longest line.
func renderPart(st styles, md string, w int, wrapLong bool) []string {
	wrap := w
	if wrapLong {
		longest := 0
		for _, l := range strings.Split(md, "\n") {
			longest = max(longest, lipgloss.Width(l))
		}
		wrap = min(max(longest+8, w), 4000)
	}
	r, err := glamour.NewTermRenderer(glamour.WithStyles(glamourStyle(st)), glamour.WithWordWrap(wrap), glamour.WithChromaFormatter("terminal16m"))
	var out string
	if err == nil {
		out, err = r.Render(md)
	}
	if err != nil {
		out = md
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		lines = append(lines, wrapLine(trimPadding(l), w)...)
	}
	for len(lines) > 0 && strings.TrimSpace(xansi.Strip(lines[0])) == "" {
		lines = lines[1:]
	}
	return lines
}

// trimPadding removes the spaces Glamour pads a line with, each of which is a styled cell of its own.
func trimPadding(l string) string {
	plain := xansi.Strip(l)
	kept := strings.TrimRight(plain, " ")
	if len(kept) == len(plain) {
		return l
	}
	return xansi.Truncate(l, lipgloss.Width(kept), "")
}

// huhTheme is the Huh theme drawn from the palette. warn makes the confirm button the colour of a warning.
func huhTheme(st styles, warn bool) huh.Theme {
	return huh.ThemeFunc(func(bool) *huh.Styles {
		p := st.p
		t := huh.ThemeBase(p.dark)
		fg := func(c color.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }
		button := lipgloss.NewStyle().Padding(0, 2).MarginRight(1)
		on := p.accent
		if warn {
			on = p.warn
		}
		f := &t.Focused
		f.Base = f.Base.BorderForeground(p.accent)
		f.Card = f.Base
		f.Title = fg(p.accent).Bold(true)
		f.NoteTitle = f.Title
		f.Description = fg(p.muted)
		f.ErrorIndicator = fg(p.err).SetString(" " + glyphFail)
		f.ErrorMessage = fg(p.err).PaddingLeft(1)
		f.SelectSelector = fg(p.accent).SetString("❯ ")
		f.NextIndicator = fg(p.accent).MarginLeft(1).SetString(glyphArrow)
		f.PrevIndicator = fg(p.accent).MarginRight(1).SetString("←")
		f.Option = fg(p.text)
		f.FocusedButton = button.Foreground(p.pillText).Background(on).Bold(true)
		f.BlurredButton = button.Foreground(p.muted).Background(p.surface)
		f.TextInput.Cursor = fg(p.accent)
		f.TextInput.CursorText = fg(p.text)
		f.TextInput.Placeholder = fg(p.faint)
		f.TextInput.Prompt = fg(p.accent)
		f.TextInput.Text = fg(p.text)
		t.Blurred = t.Focused
		t.Blurred.Base = t.Focused.Base.BorderStyle(lipgloss.HiddenBorder())
		t.Blurred.Card = t.Blurred.Base
		t.Blurred.Title = fg(p.muted)
		t.Blurred.NextIndicator = lipgloss.NewStyle()
		t.Blurred.PrevIndicator = lipgloss.NewStyle()
		t.Blurred.TextInput.Text = fg(p.muted)
		t.Blurred.FocusedButton = button.Foreground(p.muted).Background(p.surface)
		t.Group.Title = f.Title
		t.Group.Description = f.Description
		t.FieldSeparator = lipgloss.NewStyle().SetString("\n")
		t.Help = st.helpStyles()
		return t
	})
}

// shortDigest is a digest cut to its first characters.
func shortDigest(d string) string {
	algo, hex, ok := strings.Cut(d, ":")
	if !ok || len(hex) <= 20 {
		return d
	}
	return algo + ":" + hex[:20] + "…"
}

// fits is true when a fact is at most w cells wide as drawn: a bullet, the label, a space and the value without its backticks.
func (f planFact) fits(w int) bool {
	return 2+lipgloss.Width(f.label)+1+lipgloss.Width(strings.ReplaceAll(f.md, "`", "")) <= w
}

// wrapLine breaks a rendered line that is wider than w cells. The pieces after the first are indented two cells deeper than the
// line itself, so a long value stays under its key.
func wrapLine(l string, w int) []string {
	if xansi.StringWidth(l) <= w {
		return []string{l}
	}
	plain := xansi.Strip(l)
	lead := len(plain) - len(strings.TrimLeft(plain, " "))
	indent := min(lead+2, w/2)
	var out []string
	prefix, width, rest := "", w, l
	for xansi.StringWidth(rest) > width {
		out = append(out, prefix+xansi.Truncate(rest, width, ""))
		rest = xansi.TruncateLeft(rest, width, "")
		prefix, width = strings.Repeat(" ", indent), w-indent
	}
	return append(out, prefix+rest)
}
