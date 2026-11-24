//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/deploy"
	"github.com/creack/pty"
)

func TestPromoteWatchesProgressOnRealTerminal(t *testing.T) {
	image := "ghcr.io/acme/api@sha256:" + strings.Repeat("a", 64)
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":12,"digest":%q,"tag":"stable"}`, image)
	}))
	defer src.Close()
	query := make(chan string, 1)
	gate := make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(gate) })
	dstServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/applied/api"):
			w.WriteHeader(404)
			fmt.Fprint(w, `{"code":"not_deployed","message":"not deployed"}`)
		case strings.HasSuffix(r.URL.Path, "/deploys"):
			var req deploy.Request
			if json.NewDecoder(r.Body).Decode(&req) != nil {
				t.Error("bad request")
				return
			}
			if req.DryRun {
				fmt.Fprintf(w, `{"app":"api","image":%q,"tag":"stable","roles":["web"]}`, image)
				return
			}
			w.WriteHeader(202)
			fmt.Fprint(w, `{"operation_id":"op1","events_url":"/v1/projects/demo/environments/prod/operations/op1/events"}`)
		case strings.HasSuffix(r.URL.Path, "/operations/op1/events"):
			query <- r.URL.RawQuery
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
			fmt.Fprint(w, "id: 1\nevent: succeeded\ndata: {}\n\n")
		case strings.HasSuffix(r.URL.Path, "/operations/op1"):
			fmt.Fprint(w, `{"state":"succeeded"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer dstServer.Close()
	t.Setenv("PROMOTE_SRC_TEST", "source-token")
	t.Setenv("PROMOTE_DST_TEST", "dest-token")
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "1")
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	config := fmt.Sprintf(`{"projects":{"demo":{"environments":{"staging":{"url":%q,"token_env":"PROMOTE_SRC_TEST"},"prod":{"url":%q,"token_env":"PROMOTE_DST_TEST"}}}}}`, src.URL, dstServer.URL)
	if err := os.WriteFile(cfg, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte("project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dst, err := agentclient.Resolve(cfg, agentclient.Selector{Project: "demo", Environment: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	master, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(tty, &pty.Winsize{Cols: 120, Rows: 40}); err != nil {
		master.Close()
		tty.Close()
		t.Fatal(err)
	}
	oldIn := os.Stdin
	os.Stdin = tty
	readDone := make(chan struct{})
	frame := make(chan struct{}, 1)
	go func() {
		defer close(readDone)
		var seen bytes.Buffer
		b := make([]byte, 1024)
		answered := false
		for {
			n, e := master.Read(b)
			if n > 0 {
				seen.Write(b[:n])
				if !answered && bytes.Contains(seen.Bytes(), []byte("Proceed with this deploy?")) {
					answered = true
					_, _ = io.WriteString(master, "y\n")
				}
				if bytes.Contains(seen.Bytes(), []byte("following deploy (")) {
					select {
					case frame <- struct{}{}:
					default:
					}
				}
			}
			if e != nil {
				return
			}
		}
	}()
	defer func() {
		os.Stdin = oldIn
		tty.Close()
		master.Close()
		<-readDone
	}()
	var errs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- runPromote(ctx, dstServer.Client(), dst, cfg, "staging", commandFlags{dir: dir, env: "prod", project: "demo", pos: []string{"api"}}, tty, &errs)
	}()
	finished := false
	defer func() {
		cancel()
		release.Do(func() { close(gate) })
		if !finished {
			<-done
		}
	}()
	select {
	case q := <-query:
		if q != "progress=1" {
			t.Fatalf("watcher query %q", q)
		}
	case got := <-done:
		finished = true
		t.Fatalf("exit %d without watcher: %s", got, errs.String())
	case <-time.After(2 * time.Minute):
		t.Fatal("watcher request did not arrive")
	}
	select {
	case <-frame:
	case <-time.After(2 * time.Minute):
		t.Fatal("watcher did not render a delayed frame")
	}
	release.Do(func() { close(gate) })
	got := <-done
	finished = true
	if got != 0 {
		t.Fatalf("exit %d: %s", got, errs.String())
	}
}
