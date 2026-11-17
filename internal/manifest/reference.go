package manifest

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// GuideBegin and GuideEnd mark the block of the user guide that holds the generated reference. UpdateGuide rewrites it.
const (
	GuideBegin = "<!-- BEGIN GENERATED: manifest reference. Edit internal/manifest/keys.go and run: go generate ./internal/manifest -->"
	GuideEnd   = "<!-- END GENERATED: manifest reference -->"
)

// textWidth is the width, in characters, that `berthy docs` wraps a description to.
const textWidth = 100

// referenceIntro starts the reference of the whole manifest on a terminal. The user guide has its own sentence above its tables.
const referenceIntro = `The manifest is berthy.yml plus berthy.<env>.yml. Unknown keys are errors. "Required" means the manifest is refused without the key, once the section that holds it is there.`

// SectionNames are the names `berthy docs` takes, in reference order.
func SectionNames() []string {
	names := make([]string, len(Sections))
	for i, s := range Sections {
		names[i] = s.Name
	}
	return names
}

func (s Section) keys() []Key {
	var keys []Key
	for _, k := range Keys {
		if k.Section == s.Name {
			keys = append(keys, k)
		}
	}
	return keys
}

// shown is the key's path as the table of its section shows it.
func (s Section) shown(k Key) string { return strings.TrimPrefix(k.Path, s.Prefix) }

// hasApplies reports whether the section needs a column that says which kinds of app a key is for.
func (s Section) hasApplies() bool {
	for _, k := range s.keys() {
		if k.Applies != "" {
			return true
		}
	}
	return false
}

// meaning is the key's description, with its allowed values when it has a fixed set.
func (k Key) meaning() string {
	text := k.Description
	if len(k.Enum) > 0 {
		quoted := make([]string, len(k.Enum))
		for i, v := range k.Enum {
			quoted[i] = "`" + v + "`"
		}
		text += " One of " + strings.Join(quoted, ", ") + "."
	}
	return text
}

// defaultText is what the Default column shows: "required", the default, or "none".
func (k Key) defaultText() string {
	switch {
	case k.Required:
		return "required"
	case k.Default != "":
		return k.Default
	}
	return "none"
}

func (k Key) appliesText() string {
	if k.Applies == "" {
		return "both"
	}
	return k.Applies
}

// cell makes text fit a table cell.
func cell(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "|", `\|`), "\n", " ")
}

// ReferenceMarkdown is the reference of every manifest key as Markdown: a heading and a table for each section. The user
// guide holds it between GuideBegin and GuideEnd.
func ReferenceMarkdown() string {
	var b strings.Builder
	for i, s := range Sections {
		if i > 0 {
			b.WriteString("\n")
		}
		title := s.Title
		if s.Code {
			title = "`" + title + "`"
		}
		fmt.Fprintf(&b, "#### %s\n\n", title)
		if s.Note != "" {
			fmt.Fprintf(&b, "%s\n\n", s.Note)
		}
		applies := s.hasApplies()
		if applies {
			b.WriteString("| Key | Applies to | Type | Default | Meaning |\n| --- | --- | --- | --- | --- |\n")
		} else {
			b.WriteString("| Key | Type | Default | Meaning |\n| --- | --- | --- | --- |\n")
		}
		for _, k := range s.keys() {
			def := k.defaultText()
			if def != "required" && def != "none" && !strings.Contains(def, " ") {
				def = "`" + def + "`"
			}
			row := []string{"`" + s.shown(k) + "`"}
			if applies {
				row = append(row, k.appliesText())
			}
			row = append(row, k.Type, def, k.meaning())
			for i := range row {
				row[i] = cell(row[i])
			}
			fmt.Fprintf(&b, "| %s |\n", strings.Join(row, " | "))
		}
	}
	return b.String()
}

var markdownLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)

// plain removes the Markdown of a description: the backticks around code, and the targets of links.
func plain(text string) string {
	return strings.ReplaceAll(markdownLink.ReplaceAllString(text, "$1"), "`", "")
}

// wrap breaks text into lines of at most width characters, each starting with indent.
func wrap(text, indent string, width int) []string {
	var lines []string
	line := indent
	for _, word := range strings.Fields(text) {
		if line != indent && len(line)+1+len(word) > width {
			lines = append(lines, line)
			line = indent
		}
		if line != indent {
			line += " "
		}
		line += word
	}
	if line != indent {
		lines = append(lines, line)
	}
	return lines
}

func pad(s string, width int) string { return s + strings.Repeat(" ", max(0, width-len(s))) }

// sectionText renders one section for a terminal: the keys with their type and default in aligned columns, and under each
// the description, wrapped.
func sectionText(s Section) string {
	keys := s.keys()
	var b strings.Builder
	b.WriteString(plain(s.Title) + "\n")
	if s.Note != "" {
		b.WriteString(strings.Join(wrap(plain(s.Note), "", textWidth), "\n") + "\n")
	}
	b.WriteString("\n")
	applies := s.hasApplies()
	var keyW, typeW, defW int
	for _, k := range keys {
		keyW = max(keyW, len(s.shown(k)))
		typeW = max(typeW, len(k.Type))
		defW = max(defW, len(plain(k.defaultText())))
	}
	for _, k := range keys {
		row := "  " + pad(s.shown(k), keyW) + "  " + pad(k.Type, typeW) + "  " + pad(plain(k.defaultText()), defW)
		if applies {
			row += "  " + k.appliesText()
		}
		b.WriteString(strings.TrimRight(row, " ") + "\n")
		b.WriteString(strings.Join(wrap(plain(k.meaning()), "      ", textWidth), "\n") + "\n")
	}
	return b.String()
}

// ReferenceText is the reference for a terminal, with no Markdown: the section named, or the whole manifest when section is
// empty. An unknown section is an error that lists the valid names.
func ReferenceText(section string) (string, error) {
	if section != "" {
		for _, s := range Sections {
			if s.Name == section {
				return sectionText(s), nil
			}
		}
		return "", fmt.Errorf("unknown section %q; the sections are %s", section, strings.Join(SectionNames(), ", "))
	}
	intro := referenceIntro + " Sections: " + strings.Join(SectionNames(), ", ") + ". Run berthy docs <section> for one."
	parts := []string{strings.Join(wrap(intro, "", textWidth), "\n") + "\n"}
	for _, s := range Sections {
		parts = append(parts, sectionText(s))
	}
	return strings.Join(parts, "\n"), nil
}

// UpdateGuide returns the user guide with the text between GuideBegin and GuideEnd replaced by the current reference. It
// keeps the guide's line endings: a guide with CRLF stays CRLF.
func UpdateGuide(doc string) (string, error) {
	crlf := strings.Contains(doc, "\r\n")
	doc = strings.ReplaceAll(doc, "\r\n", "\n")
	begin := strings.Index(doc, GuideBegin)
	end := strings.Index(doc, GuideEnd)
	switch {
	case begin < 0:
		return "", errors.New("the guide has no line that starts the generated manifest reference: " + GuideBegin)
	case end < begin:
		return "", errors.New("the guide has no line that ends the generated manifest reference after its start: " + GuideEnd)
	}
	out := doc[:begin] + GuideBegin + "\n\n" + ReferenceMarkdown() + "\n" + doc[end:]
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	return out, nil
}
