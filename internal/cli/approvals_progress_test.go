package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestApproveReplanProgressAndRedaction(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "1")
	forceStderrTerminal(t, true)
	forceProgressVT(t)
	old := progressWidth
	progressWidth = func(io.Writer) (int, error) { return 100, nil }
	t.Cleanup(func() { progressWidth = old })
	synctest.Test(t, func(t *testing.T) {
		var out, errs bytes.Buffer
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
		defer func() { close(release); <-done }()
		synctest.Wait()
		if !strings.Contains(out.String(), "plan:") || strings.Contains(out.String(), "\x1b") {
			t.Fatalf("before delay: %q", out.String())
		}
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		if !strings.Contains(out.String(), "approving") {
			t.Fatalf("missing approval wait: %q", out.String())
		}
		close(release)
		release = make(chan struct{})
		synctest.Wait()
		if !strings.Contains(errs.String(), "[redacted]") || strings.Contains(errs.String(), "secret") {
			t.Fatalf("diagnostic: %q", errs.String())
		}
		before := out.String()
		time.Sleep(time.Second)
		synctest.Wait()
		if out.String() != before {
			t.Fatal("write after response")
		}
	})
}
