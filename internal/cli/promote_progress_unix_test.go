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
	"testing"

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
	oldIn := os.Stdin
	os.Stdin = tty
	defer func() { os.Stdin = oldIn; tty.Close() }()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		var seen bytes.Buffer
		b := make([]byte, 1024)
		answered := false
		for {
			n, e := master.Read(b)
			if n > 0 && !answered {
				seen.Write(b[:n])
				if bytes.Contains(seen.Bytes(), []byte("Proceed with this deploy?")) {
					answered = true
					_, _ = io.WriteString(master, "y\n")
				}
			}
			if e != nil {
				return
			}
		}
	}()
	defer func() { master.Close(); <-readDone }()
	var errs bytes.Buffer
	got := runPromote(context.Background(), dstServer.Client(), dst, cfg, "staging", commandFlags{dir: dir, env: "prod", project: "demo", pos: []string{"api"}}, tty, &errs)
	if got != 0 {
		t.Fatalf("exit %d: %s", got, errs.String())
	}
	select {
	case q := <-query:
		if q != "progress=1" {
			t.Fatalf("watcher query %q", q)
		}
	default:
		t.Fatal("no watcher request")
	}
}
