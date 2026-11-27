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
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
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
				safe := result.text
				for _, key := range []string{"app", "image", "notes", "roles", "tag"} {
					span := "\x1b[2m" + key + "\x1b[0m: "
					if strings.Count(safe, span) != 1 {
						t.Errorf("expected one styled %s key in %q", key, result.text)
					}
					safe = strings.Replace(safe, span, key+": ", 1)
				}
				if strings.ContainsRune(safe, '\x1b') || strings.Contains(result.text, "test-token") || !strings.Contains(result.text, "[redacted]") || !strings.Contains(result.text, `start\x1b[2Kend`) {
					t.Errorf("interactive preview contains unsafe response text: %q", result.text)
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

func TestProtectedDeployDiffPrecedesConfirmation(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")
	for _, tc := range []struct {
		answer string
		code   int
	}{{"n", 1}, {"y", 0}} {
		t.Run(tc.answer, func(t *testing.T) {
			var moved atomic.Bool
			var lookups atomic.Int32
			requests := make(chan fieldDeployCall, 8)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/agent"):
					fmt.Fprint(w, `{"project":"demo","environment":"dev","protected":true,"effective_protected":true}`)
				case strings.HasSuffix(r.URL.Path, "/images/resolve"):
					lookups.Add(1)
					image := fieldDiffImage
					if moved.Load() {
						image = strings.Replace(image, strings.Repeat("a", 64), strings.Repeat("b", 64), 1)
					}
					fmt.Fprintf(w, `{"image":%q,"tag":"stable"}`, image)
				case strings.HasSuffix(r.URL.Path, "/deploys"):
					var req deploy.Request
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
						return
					}
					requests <- fieldDeployCall{r.URL.RawQuery, req}
					if req.DryRun {
						detail := ""
						if r.URL.RawQuery == "diff=fields" {
							detail = replicaFieldDiff
						}
						fmt.Fprint(w, fieldImagePlan("stable", changedFieldDiff, detail))
					} else {
						fmt.Fprint(w, `{}`)
					}
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			cfg := ingressCLIConfig(t, server.URL)
			dir, head := checkoutRepo(t, map[string]string{"berthy.yml": "protected: true\n" + versionsManifest})
			args := []string{"deploy", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", "stable"}
			var control, withFlag, jsonErrs bytes.Buffer
			if code := Run(append(append([]string(nil), args...), "--json", "--dry-run"), &control, &jsonErrs); code != 0 {
				t.Fatalf("JSON control: exit %d: %s", code, &jsonErrs)
			}
			if code := Run(append(append([]string(nil), args...), "--json", "--dry-run", "--diff"), &withFlag, &jsonErrs); code != 0 || withFlag.String() != control.String() {
				t.Errorf("JSON --diff: exit %d: %s", code, &jsonErrs)
			}
			for len(requests) > 0 {
				if call := <-requests; call.query != "" || !call.req.DryRun {
					t.Errorf("JSON changed request: %+v", call)
				}
			}
			lookups.Store(0)

			master, tty, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			previous := os.Stdin
			os.Stdin = tty
			ready, stopped, readerDone := make(chan string, 2), make(chan struct{}), make(chan struct{})
			var code int
			var errs bytes.Buffer
			defer func() {
				master.Close()
				<-stopped
				tty.Close()
				<-readerDone
				os.Stdin = previous
			}()
			go func() {
				defer close(stopped)
				code = Run(append(args, "--diff"), tty, &errs)
			}()
			const prompt = "Proceed with this deploy? [y/N] "
			go func() {
				defer close(readerDone)
				reader := bufio.NewReader(master)
				var seen strings.Builder
				for {
					c, err := reader.ReadByte()
					if err != nil {
						return
					}
					seen.WriteByte(c)
					if strings.HasSuffix(seen.String(), prompt) {
						ready <- seen.String()
					}
				}
			}()
			select {
			case output := <-ready:
				output = strings.ReplaceAll(output, "\r\n", "\n")
				if strings.Count(output, fieldHeading) != 1 || !strings.Contains(output, "- replicas: 2\n      + replicas: 3") || !strings.Contains(output, fieldDiffImage) {
					t.Errorf("preview before prompt: %q", output)
				}
			case <-stopped:
				t.Fatalf("exit %d before confirmation: %s", code, &errs)
			}
			moved.Store(true)
			if _, err := fmt.Fprintln(master, tc.answer); err != nil {
				t.Fatal(err)
			}
			select {
			case <-ready:
				t.Fatal("deploy prompted twice")
			case <-stopped:
			}
			wantCalls := 1
			if tc.code == 0 {
				wantCalls++
			}
			if code != tc.code || lookups.Load() != 1 || len(requests) != wantCalls {
				t.Fatalf("exit %d, lookups %d, requests %d: %s", code, lookups.Load(), len(requests), &errs)
			}
			preview := <-requests
			if preview.query != "diff=fields" || !preview.req.DryRun || preview.req.Version != fieldDiffImage {
				t.Errorf("preview: %+v", preview)
			}
			if tc.code == 0 {
				actual := <-requests
				req := actual.req
				if actual.query != "" || req.DryRun || req.Version != fieldDiffImage || req.VersionTag != "stable" || req.ExpectedApplied == nil || *req.ExpectedApplied != 17 || req.Commit != head {
					t.Errorf("admitted: %+v", actual)
				}
			}
		})
	}
}
