package cli

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/setup"
	"fmt"
	"github.com/charmbracelet/x/ansi"
)

func TestProgressShortLongAndClosed(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "1")
	forceProgressVT(t)
	old := progressWidth
	progressWidth = func(io.Writer) (int, error) { return 80, nil }
	t.Cleanup(func() { progressWidth = old })
	synctest.Test(t, func(t *testing.T) {
		var out bytes.Buffer
		p := newProgressLine(&out, true)
		defer p.Close()
		p.Start("quick")
		p.Finish(&out, setup.StepResult{Step: "quick", Status: "ok", Detail: "quick done"}, true)
		if strings.Contains(out.String(), "\033") {
			t.Fatalf("short work flashed: %q", out.String())
		}
		out.Reset()
		p.Start("check")
		time.Sleep(249 * time.Millisecond)
		synctest.Wait()
		p.mu.Lock()
		short := out.String()
		p.mu.Unlock()
		if short != "" {
			t.Fatalf("short wait flashed: %q", short)
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		p.mu.Lock()
		first := out.String()
		p.mu.Unlock()
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		p.mu.Lock()
		second := out.String()
		p.mu.Unlock()
		if first != "\r\x1b[2K| check (0.2s)" || second != first+"\r\x1b[2K/ check (0.3s)" {
			t.Fatalf("unexpected spinner frames: first %q, second %q", first, second)
		}
		p.Message(func() { fmt.Fprintln(&out, "a message") })
		p.Finish(&out, setup.StepResult{Step: "check", Status: "ok", Detail: "done"}, true)
		p.Close()
		before := out.String()
		time.Sleep(time.Second)
		synctest.Wait()
		if out.String() != before {
			t.Fatal("write after close")
		}
		if strings.Count(before, "a message\n") != 1 || strings.Count(before, "done (") != 1 {
			t.Fatalf("missing permanent lines: %q", before)
		}
	})
}

func TestProgressFitsLongLabel(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "1")
	forceProgressVT(t)
	old := progressWidth
	progressWidth = func(io.Writer) (int, error) { return 30, nil }
	t.Cleanup(func() { progressWidth = old })
	synctest.Test(t, func(t *testing.T) {
		var out bytes.Buffer
		p := newProgressLine(&out, true)
		defer p.Close()
		p.Start(strings.Repeat("x", 80) + "\x1b[31m")
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		p.mu.Lock()
		frame := strings.TrimPrefix(out.String(), "\r\x1b[2K")
		p.mu.Unlock()
		if !strings.Contains(frame, "s)") || ansi.StringWidth(frame) >= 30 || strings.Contains(frame, "\x1b") {
			t.Fatalf("invalid frame: %q", frame)
		}
	})
}

func TestProgressTerminalPolicy(t *testing.T) {
	old := progressWidth
	oldVT := progressVT
	progressVT = func(_ io.Writer, terminal string) bool {
		return terminal != "" && terminal != "dumb" && terminal != "unknown"
	}
	t.Cleanup(func() { progressWidth = old })
	t.Cleanup(func() { progressVT = oldVT })
	for _, tc := range []struct {
		name, term, noColor string
		width               int
		err                 error
		animate, color      bool
	}{
		{"color", "xterm", "", 80, nil, true, true},
		{"no color", "xterm", "1", 80, nil, true, false},
		{"narrow", "xterm", "", 12, nil, false, true},
		{"dumb", "dumb", "", 80, nil, false, false},
		{"unknown terminal", "unknown", "", 80, nil, false, false},
		{"unsupported", "xterm", "", 0, errors.New("size unavailable"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TERM", tc.term)
			t.Setenv("NO_COLOR", tc.noColor)
			progressWidth = func(io.Writer) (int, error) { return tc.width, tc.err }
			var out bytes.Buffer
			p := newProgressLine(&out, true)
			if p.animate != tc.animate || styleFor(&out, true).color != tc.color {
				t.Fatalf("animate=%v color=%v", p.animate, styleFor(&out, true).color)
			}
			p.Start("check")
			p.Finish(&out, setup.StepResult{Step: "check", Status: "failed", Detail: "unsafe\x1b[31m"}, true)
			got := out.String()
			if strings.Contains(got, "\x1b[31m") != tc.color {
				t.Fatalf("color policy: %q", got)
			}
			if strings.Contains(got, "unsafe\x1b") {
				t.Fatalf("unsafe detail: %q", got)
			}
		})
	}
}

func TestProgressElapsedUsesObservedTime(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "1")
	forceProgressVT(t)
	old := progressWidth
	progressWidth = func(io.Writer) (int, error) { return 80, nil }
	t.Cleanup(func() { progressWidth = old })
	synctest.Test(t, func(t *testing.T) {
		var out bytes.Buffer
		p := newProgressLine(&out, true)
		defer p.Close()
		p.Start("check")
		time.Sleep(2 * time.Second)
		synctest.Wait()
		p.Finish(&out, setup.StepResult{Step: "check", Status: "ok", Detail: "done"}, true)
		if !strings.Contains(out.String(), "2.0s") {
			t.Fatalf("elapsed missing: %q", out.String())
		}
		out.Reset()
		p = newProgressLine(&out, false)
		p.Start("check")
		time.Sleep(2 * time.Second)
		p.Finish(&out, setup.StepResult{Step: "check", Status: "ok", Detail: "done"}, false)
		if strings.Contains(out.String(), "2.0s") {
			t.Fatalf("plain output gained elapsed: %q", out.String())
		}
	})
}

func forceProgressVT(t *testing.T) {
	t.Helper()
	old := progressVT
	progressVT = func(io.Writer, string) bool { return true }
	t.Cleanup(func() { progressVT = old })
}

func TestProgressRefusesUnsupportedConsole(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "")
	oldWidth, oldVT := progressWidth, progressVT
	progressWidth = func(io.Writer) (int, error) { return 80, nil }
	progressVT = func(io.Writer, string) bool { return false }
	t.Cleanup(func() { progressWidth, progressVT = oldWidth, oldVT })
	var out bytes.Buffer
	p := newProgressLine(&out, true)
	p.Start("check")
	p.Finish(&out, setup.StepResult{Step: "check", Status: "ok", Detail: "done"}, true)
	if p.animate || styleFor(&out, true).color || strings.Contains(out.String(), "\x1b") {
		t.Fatalf("unsupported console: %q", out.String())
	}
}

type gatedProgressWriter struct {
	entered chan struct{}
	release chan struct{}
	active  atomic.Int32
	overlap atomic.Bool
}

func (w *gatedProgressWriter) Write(b []byte) (int, error) {
	if w.active.Add(1) != 1 {
		w.overlap.Store(true)
	}
	defer w.active.Add(-1)
	select {
	case w.entered <- struct{}{}:
		<-w.release
	default:
	}
	return len(b), nil
}

func TestProgressSerializesFinishAndMessage(t *testing.T) {
	w := &gatedProgressWriter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	p := newProgressLine(w, false)
	p.Start("check")
	finished, messaged := make(chan struct{}), make(chan struct{})
	go func() {
		p.Finish(w, setup.StepResult{Step: "check", Status: "ok", Detail: "done"}, false)
		close(finished)
	}()
	select {
	case <-w.entered:
	case <-finished:
		close(w.release)
		t.Fatal("final line was not written")
	}
	unlocked := p.mu.TryLock()
	if unlocked {
		p.mu.Unlock()
	}
	go func() { p.Message(func() { fmt.Fprintln(w, "message") }); close(messaged) }()
	close(w.release)
	<-finished
	<-messaged
	if unlocked || w.overlap.Load() {
		t.Fatal("final line and message writes are not serialized")
	}
}

func TestProgressWriterMatrix(t *testing.T) {
	t.Setenv("TERM", "xterm")
	oldWidth, oldVT := progressWidth, progressVT
	defer func() { progressWidth, progressVT = oldWidth, oldVT }()
	for _, tc := range []struct {
		name                          string
		outTerm, errTerm, vt, noColor bool
		width                         int
	}{
		{"both", true, true, true, false, 80}, {"stdout", true, false, true, false, 80},
		{"stderr", false, true, true, false, 80}, {"pipes", false, false, true, false, 80},
		{"narrow", true, true, true, false, 20}, {"unsupported VT", true, true, false, false, 80},
		{"no color", true, true, true, true, 80},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.noColor {
				t.Setenv("NO_COLOR", "1")
			} else {
				t.Setenv("NO_COLOR", "")
			}
			synctest.Test(t, func(t *testing.T) {
				var out, errout bytes.Buffer
				progressWidth = func(w io.Writer) (int, error) {
					if w == &out && tc.outTerm || w == &errout && tc.errTerm {
						return tc.width, nil
					}
					return 0, errors.New("pipe")
				}
				progressVT = func(io.Writer, string) bool { return tc.vt }
				p := newProgressLine(&errout, tc.errTerm)
				p.Start("check")
				time.Sleep(249 * time.Millisecond)
				synctest.Wait()
				if errout.Len() != 0 || out.Len() != 0 {
					t.Fatal("early output")
				}
				time.Sleep(time.Millisecond)
				synctest.Wait()
				frame := errout.String()
				wantFrame := tc.errTerm && tc.vt && tc.width >= 30
				if strings.Contains(frame, "\r\x1b[2K| check") != wantFrame {
					t.Fatalf("frame %q", frame)
				}
				p.Finish(&out, setup.StepResult{Step: "check", Status: "ok", Detail: "done"}, tc.outTerm)
				wantColor := tc.outTerm && tc.vt && !tc.noColor
				if strings.Contains(out.String(), "\x1b[32m") != wantColor {
					t.Fatalf("result %q", out.String())
				}
				if !tc.outTerm && out.String() != "✓ check       done\n" {
					t.Fatalf("plain result %q", out.String())
				}
				if tc.noColor && (strings.Contains(out.String(), "\x1b[0m") || strings.Contains(errout.String(), "\x1b[0m")) {
					t.Fatal("SGR under NO_COLOR")
				}
			})
		})
	}
}
