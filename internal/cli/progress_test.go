package cli

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestProgressShortLongAndClosed(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "1")
	old := progressWidth
	progressWidth = func(io.Writer) (int, error) { return 80, nil }
	t.Cleanup(func() { progressWidth = old })
	synctest.Test(t, func(t *testing.T) {
		var out bytes.Buffer
		p := newProgressLine(&out, true)
		defer p.Close()
		p.Start("quick")
		p.Finish("ok", "quick done")
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
		if second == first {
			t.Fatal("spinner did not advance")
		}
		p.Message("a message")
		p.Finish("ok", "done")
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
	t.Cleanup(func() { progressWidth = old })
	for _, tc := range []struct {
		name, term, noColor string
		width               int
		err                 error
		animate, color      bool
	}{
		{"color", "xterm", "", 80, nil, true, true},
		{"no color", "xterm", "1", 80, nil, true, false},
		{"narrow", "xterm", "", 12, nil, false, false},
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
			if p.animate != tc.animate || p.color != tc.color {
				t.Fatalf("animate=%v color=%v", p.animate, p.color)
			}
			p.Start("check")
			p.Finish("failed", "unsafe\x1b[31m")
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
		p.mu.Lock()
		pending := out.String()
		p.mu.Unlock()
		if !strings.Contains(pending, "1.") {
			t.Fatalf("pending elapsed missing: %q", pending)
		}
		p.Finish("ok", "done")
		if !strings.Contains(out.String(), "2.0s") {
			t.Fatalf("elapsed missing: %q", out.String())
		}
		out.Reset()
		p = newProgressLine(&out, false)
		p.Start("check")
		time.Sleep(2 * time.Second)
		p.Finish("ok", "done")
		if strings.Contains(out.String(), "2.0s") {
			t.Fatalf("plain output gained elapsed: %q", out.String())
		}
	})
}
