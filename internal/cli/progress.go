package cli

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// progressLine owns the output while a step is pending.
type progressLine struct {
	out     io.Writer
	enabled bool
	mu      sync.Mutex
	label   string
	visible bool
	stop    chan struct{}
	done    chan struct{}
}

func newProgressLine(out io.Writer, enabled bool) *progressLine {
	return &progressLine{out: out, enabled: enabled}
}

func (p *progressLine) Start(label string) {
	p.Close()
	p.label = terminalSafe(label)
	if !p.enabled {
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
			fmt.Fprintf(p.out, "\r\033[2K%c %s", frames[i%len(frames)], p.label)
			p.visible = true
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
	marker := setupGlyphs[status]
	if p.enabled {
		marker = status
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
