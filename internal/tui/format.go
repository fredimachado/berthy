package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"berthy/internal/api"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// sanitize makes text from an app, an agent or a user safe to put on the screen: escape sequences are removed whole, then every
// remaining control, format and invalid character is dropped, and a tab becomes spaces. Nothing an app prints can move the
// cursor, change a colour or set the window title.
func sanitize(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	s = ansi.Strip(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteString("    ")
		case r == '�':
			b.WriteRune(r)
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), r == ' ', r == ' ':
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// fit cuts s to w cells with an ellipsis, or pads it with spaces to w cells.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) > w {
		s = ansi.Truncate(s, w, "…")
	}
	if pad := w - lipgloss.Width(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// clip cuts s to w cells with an ellipsis and does not pad it.
func clip(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) > w {
		return ansi.Truncate(s, w, "…")
	}
	return s
}

// fitLines makes s exactly w cells wide and h lines tall.
func fitLines(s string, w, h int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for i := range lines {
		lines[i] = fit(lines[i], w)
	}
	blank := strings.Repeat(" ", max(w, 0))
	for len(lines) < h {
		lines = append(lines, blank)
	}
	return strings.Join(lines, "\n")
}

// between lays left and right out on one line of w cells, right-aligned, and cuts the left part when they do not both fit.
func between(left, right string, w int) string {
	rw := lipgloss.Width(right)
	if rw >= w {
		return clip(right, w)
	}
	if lipgloss.Width(left) > w-rw-1 {
		left = clip(left, max(w-rw-1, 0))
	}
	gap := w - lipgloss.Width(left) - rw
	return left + strings.Repeat(" ", max(gap, 0)) + right
}

// ago is how long ago t was, "5m ago", or "-" for an unknown time.
func ago(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return api.HumanAge(now.Sub(t)) + " ago"
}

// agoShort is how long ago t was, "5m", or "-".
func agoShort(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return api.HumanAge(now.Sub(t))
}

// until is how long until t, "12m", or "0s" when it has passed.
func until(now, t time.Time) string {
	return api.HumanAge(max(t.Sub(now), 0))
}

// countdown is a duration as m:ss, or h:mm:ss.
func countdown(d time.Duration) string {
	d = max(d, 0).Round(time.Second)
	h := int(d / time.Hour)
	m := int(d % time.Hour / time.Minute)
	s := int(d % time.Minute / time.Second)
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// bytesText is a byte count for a table cell.
func bytesText(n uint64) string {
	if n > 1<<62 {
		return "-"
	}
	return api.HumanSize(int64(n))
}

// rateText is a byte rate, "1.2 MiB/s".
func rateText(perSecond float64) string {
	if perSecond < 0 {
		perSecond = 0
	}
	return api.HumanSize(int64(perSecond)) + "/s"
}

// shortImage is the version of an image reference for a table: its tag, or else the first characters of its digest.
func shortImage(image string) string {
	if image == "" {
		return "-"
	}
	name, digest, hasDigest := strings.Cut(image, "@")
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		name = name[slash+1:]
	}
	if _, tag, ok := strings.Cut(name, ":"); ok && tag != "" {
		return tag
	}
	if hasDigest {
		algo, hex, _ := strings.Cut(digest, ":")
		if hex == "" {
			hex, algo = algo, "sha256"
		}
		return algo + ":" + hex[:min(len(hex), 8)]
	}
	return "latest"
}

var sparkRunes = []rune("▁▂▃▄▅▆▇█")

// sparkline draws values, from 0 to ceiling, as one line of block characters w cells wide. The newest values are at the right;
// when there are fewer values than cells the line is padded at the left with the lowest block.
func sparkline(values []float64, w int, ceiling float64) string {
	if w <= 0 {
		return ""
	}
	if len(values) > w {
		values = values[len(values)-w:]
	}
	if ceiling <= 0 {
		ceiling = 100
	}
	var b strings.Builder
	for i := 0; i < w-len(values); i++ {
		b.WriteRune(' ')
	}
	for _, v := range values {
		f := min(max(v/ceiling, 0), 1)
		b.WriteRune(sparkRunes[int(f*float64(len(sparkRunes)-1)+0.5)])
	}
	return b.String()
}

// plural is "1 app" or "3 apps".
func plural(n int, one string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %ss", n, one)
}

// matches reports whether the characters of query appear in name in order, ignoring case: a substring always matches, and so
// does a loose abbreviation of it.
func matches(name, query string) bool {
	if query == "" {
		return true
	}
	name, query = strings.ToLower(name), strings.ToLower(query)
	if strings.Contains(name, query) {
		return true
	}
	qi := 0
	qr := []rune(query)
	for _, r := range name {
		if r == qr[qi] {
			qi++
			if qi == len(qr) {
				return true
			}
		}
	}
	return false
}
