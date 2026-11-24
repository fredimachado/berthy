package cli

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

var progressWidth = func(w io.Writer) (int, error) {
	f, ok := w.(*os.File)
	if !ok {
		return 0, fmt.Errorf("not a terminal file")
	}
	width, _, err := term.GetSize(f.Fd())
	return width, err
}

var progressVT = progressPlatformVT

// progressLine owns the output while a step is pending.
type progressLine struct {
	out     io.Writer
	enabled bool
	animate bool
	color   bool
	mu      sync.Mutex
	label   string
	started time.Time
	visible bool
	stop    chan struct{}
	done    chan struct{}
}

func newProgressLine(out io.Writer, enabled bool) *progressLine {
	p := &progressLine{out: out, enabled: enabled}
	if enabled && progressVT(out, os.Getenv("TERM")) {
		width, err := progressWidth(out)
		p.animate = err == nil && width >= 30
		p.color = p.animate && os.Getenv("NO_COLOR") == ""
	}
	return p
}

func (p *progressLine) Start(label string) {
	p.Close()
	p.label = terminalSafe(label)
	p.started = time.Now()
	if !p.animate {
		return
	}
	p.stop, p.done = make(chan struct{}), make(chan struct{})
	go func(stop <-chan struct{}, done chan<- struct{}) {
		defer close(done)
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-stop:
			return
		case <-timer.C:
		}
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		frames := "|/-\\"
		for i := 0; ; i++ {
			p.mu.Lock()
			width, err := progressWidth(p.out)
			if err == nil && width >= 30 {
				prefix := fmt.Sprintf("%c ", frames[i%len(frames)])
				suffix := fmt.Sprintf(" (%.1fs)", time.Since(p.started).Seconds())
				room := width - 1 - ansi.StringWidth(prefix) - ansi.StringWidth(suffix)
				if room > 0 {
					fmt.Fprintf(p.out, "\r\033[2K%s%s%s", prefix, ansi.Truncate(p.label, room, ""), suffix)
					p.visible = true
				} else {
					p.clear()
				}
			} else {
				p.clear()
			}
			p.mu.Unlock()
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}(p.stop, p.done)
}

func (p *progressLine) clear() {
	if p.visible {
		fmt.Fprint(p.out, "\r\033[2K")
		p.visible = false
	}
}

func (p *progressLine) Message(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clear()
	fmt.Fprintln(p.out, terminalSafe(text))
}

func (p *progressLine) Finish(status, detail string) {
	p.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	status = terminalSafe(status)
	marker := setupGlyphs[status]
	if p.enabled {
		marker = status
		if p.color {
			color := map[string]string{"ok": "32", "changed": "32", "would_change": "36", "skipped": "33", "failed": "31"}[status]
			if color != "" {
				marker = "\033[" + color + "m" + marker + "\033[0m"
			}
		}
		fmt.Fprintf(p.out, "%s %-10s  %s (%.1fs)\n", marker, p.label, terminalSafe(detail), time.Since(p.started).Seconds())
		return
	}
	fmt.Fprintf(p.out, "%s %-10s  %s\n", marker, p.label, terminalSafe(detail))
}

func (p *progressLine) Close() {
	if p.stop != nil {
		close(p.stop)
		<-p.done
		p.stop, p.done = nil, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clear()
}
