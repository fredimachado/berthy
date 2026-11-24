//go:build unix

package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"berthy/internal/deploy"
	"github.com/creack/pty"
)

func TestProtectedTagConfirmFreezesDigestAndCheckout(t *testing.T) {
	for _, tc := range []struct {
		answer               string
		wantCode, admissions int
	}{{"n\n", 1, 0}, {"y\n", 0, 1}} {
		t.Run(strings.TrimSpace(tc.answer), func(t *testing.T) {
			a := "docker.io/library/alpine@sha256:" + strings.Repeat("a", 64)
			b := "docker.io/library/alpine@sha256:" + strings.Repeat("b", 64)
			var resolved atomic.Value
			resolved.Store(a)
			var lookups atomic.Int32
			requests := make(chan deploy.Request, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/agent"):
					fmt.Fprint(w, `{"project":"demo","environment":"dev","protected":true,"effective_protected":true}`)
				case strings.HasSuffix(r.URL.Path, "/images/resolve"):
					lookups.Add(1)
					fmt.Fprintf(w, `{"image":%q,"tag":"stable"}`, resolved.Load().(string))
				case strings.HasSuffix(r.URL.Path, "/deploys"):
					var req deploy.Request
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
						return
					}
					requests <- req
					if req.DryRun {
						fmt.Fprintf(w, `{"app":"api","image":%q,"tag":"stable","roles":["web"],"notes":["start\u001b[2Kend","\u0074est-token"]}`, req.Version)
					} else {
						fmt.Fprint(w, `{}`)
					}
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			cfg := ingressCLIConfig(t, server.URL)
			dir, head := checkoutRepo(t, map[string]string{"berthy.yml": "protected: true\n" + versionsManifest, "berthy.dev.yml": "apps:\n  api:\n    env:\n      plain:\n        MARKER: before\n"})
			master, tty, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			defer tty.Close()
			previous := os.Stdin
			os.Stdin = tty
			defer func() { os.Stdin = previous }()
			var errs bytes.Buffer
			done := make(chan int, 1)
			go func() {
				done <- Run([]string{"deploy", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", "stable"}, tty, &errs)
			}()
			// Drain the preview and wait for the prompt, without a clock-based sleep.
			type promptResult struct {
				text string
				err  error
			}
			prompt := make(chan promptResult, 1)
			go func() {
				reader := bufio.NewReader(master)
				var seen strings.Builder
				for !strings.Contains(seen.String(), "Proceed with this deploy? [y/N] ") {
					c, err := reader.ReadByte()
					if err != nil {
						prompt <- promptResult{err: err}
						return
					}
					seen.WriteByte(c)
				}
				prompt <- promptResult{text: seen.String()}
			}()
			select {
			case result := <-prompt:
				if result.err != nil {
					t.Fatal(result.err)
				}
				if strings.ContainsRune(result.text, '\x1b') || strings.Contains(result.text, "test-token") || !strings.Contains(result.text, "[redacted]") {
					t.Error("interactive preview contains unsafe response text")
				}
			case code := <-done:
				t.Fatalf("deploy exited %d before confirmation: %s", code, errs.String())
			}
			resolved.Store(b)
			if err := os.WriteFile(filepath.Join(dir, "berthy.dev.yml"), []byte("apps:\n  api:\n    env:\n      plain:\n        MARKER: after\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(master, tc.answer); err != nil {
				t.Fatal(err)
			}
			code := <-done
			if code != tc.wantCode || lookups.Load() != 1 {
				t.Fatalf("exit %d lookup %d stderr %q", code, lookups.Load(), errs.String())
			}
			first := <-requests
			if !first.DryRun || first.Version != a || first.VersionTag != "stable" || first.Commit != head || first.Modified {
				t.Fatalf("bad preview %+v", first)
			}
			if got := first.Manifest.Apps["api"].Env.Plain["MARKER"]; got != "before" {
				t.Fatalf("preview manifest marker %q", got)
			}
			if tc.admissions == 1 {
				actual := <-requests
				if actual.DryRun || actual.Version != first.Version || actual.VersionTag != first.VersionTag || actual.Commit != first.Commit || actual.Modified || actual.Manifest.Apps["api"].Env.Plain["MARKER"] != "before" {
					t.Fatalf("changed request %+v", actual)
				}
			}
			select {
			case r := <-requests:
				t.Fatalf("unexpected extra request %+v", r)
			default:
			}
		})
	}
}
