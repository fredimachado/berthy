package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type progressTestBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *progressTestBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *progressTestBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }
func (b *progressTestBuffer) Len() int       { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.Len() }

func TestApproveReplanProgressAndRedaction(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "1")
	forceStderrTerminal(t, true)
	forceProgressVT(t)
	old := progressWidth
	progressWidth = func(io.Writer) (int, error) { return 100, nil }
	t.Cleanup(func() { progressWidth = old })
	synctest.Test(t, func(t *testing.T) {
		var out progressTestBuffer
		var errs progressTestBuffer
		release := make(chan struct{})
		done := make(chan int, 1)
		call := func(method, endpoint, query string, body []byte) ([]byte, int, error) {
			if method == "GET" {
				return []byte(`[{"id":"ap1","kind":"deploy web","summary":"web","actor":"alice","plan":{"action":"deploy"}}]`), http.StatusOK, nil
			}
			<-release
			return []byte(`{"message":"denied se\u0063ret"}`), http.StatusForbidden, nil
		}
		base, _ := url.Parse("http://agent.invalid")
		go func() {
			done <- runApprove(context.Background(), call, &http.Client{}, commandFlags{pos: []string{"ap1"}}, base, "/v1/", "secret", &out, &errs)
		}()
		var releaseOnce sync.Once
		returned := false
		defer func() {
			releaseOnce.Do(func() { close(release) })
			if !returned {
				<-done
			}
		}()
		synctest.Wait()
		if !strings.Contains(out.String(), "plan:") || strings.Contains(out.String(), "\x1b") {
			t.Fatalf("before delay: %q", out.String())
		}
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		if !strings.Contains(errs.String(), "approving") {
			t.Fatalf("missing approval wait: %q", errs.String())
		}
		releaseOnce.Do(func() { close(release) })
		got := <-done
		returned = true
		if got != 1 {
			t.Fatalf("exit = %d", got)
		}
		want := "\r\x1b[2K| approving (0.2s)\r\x1b[2Kagent returned HTTP 403: denied [redacted]\n"
		if errs.String() != want {
			t.Fatalf("diagnostic: %q, want %q", errs.String(), want)
		}
		beforeOut, beforeErr := out.String(), errs.String()
		time.Sleep(time.Second)
		synctest.Wait()
		if out.String() != beforeOut || errs.String() != beforeErr {
			t.Fatalf("write after response: stdout %q, stderr %q", out.String(), errs.String())
		}
	})
}
