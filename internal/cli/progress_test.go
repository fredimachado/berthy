package cli

import (
	"bytes"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestProgressShortLongAndClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var out bytes.Buffer
		p := newProgressLine(&out, true)
		p.Start("quick")
		p.Finish("ok", "quick done")
		if strings.Contains(out.String(), "\033") {
			t.Fatalf("short work flashed: %q", out.String())
		}
		out.Reset()
		p.Start("check")
		time.Sleep(249 * time.Millisecond)
		synctest.Wait()
		if out.Len() != 0 {
			t.Fatalf("short wait flashed: %q", out.String())
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		first := out.String()
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		if out.String() == first {
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
		if strings.Count(before, "a message\n") != 1 || strings.Count(before, "done\n") != 1 {
			t.Fatalf("missing permanent lines: %q", before)
		}
	})
}
