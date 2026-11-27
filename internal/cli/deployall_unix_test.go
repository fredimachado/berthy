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

	"github.com/creack/pty"
)

func TestProtectedProjectDeployDiffReviewsOnce(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")
	for _, mode := range []string{"declined", "confirmed", "stale later app", "blocked"} {
		t.Run(mode, func(t *testing.T) {
			agent, args := fieldProjectFixture(t, true)
			const reason = "stateful app zstack would recreate database; deploy with --allow-recreate and an admin token"
			if mode == "blocked" {
				agent.fieldPlans["zstack"] = strings.Replace(agent.fieldPlans["zstack"], `"volumes":[]`, fmt.Sprintf(`"volumes":[],"blockers":[%q]`, reason), 1)
			}
			master, tty, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			previous := os.Stdin
			os.Stdin = tty
			var errs bytes.Buffer
			var exit int
			stopped, readerDone := make(chan struct{}), make(chan struct{})
			prompted := make(chan string, 2)
			var transcript string
			defer func() {
				master.Close()
				<-stopped
				tty.Close()
				<-readerDone
				os.Stdin = previous
			}()
			go func() {
				defer close(stopped)
				exit = Run(append(args, "--diff"), tty, &errs)
			}()
			const prompt = "Proceed with this deploy? [y/N] "
			go func() {
				defer close(readerDone)
				r := bufio.NewReader(master)
				var seen strings.Builder
				defer func() { transcript = strings.ReplaceAll(seen.String(), "\r\n", "\n") }()
				for {
					c, err := r.ReadByte()
					if err != nil {
						return
					}
					seen.WriteByte(c)
					if strings.HasSuffix(seen.String(), prompt) {
						prompted <- strings.ReplaceAll(seen.String(), "\r\n", "\n")
					}
				}
			}()
			allPreviews := []string{"preview api?diff=fields", "preview redis?diff=fields", "preview zstack?diff=fields"}
			select {
			case preview := <-prompted:
				agent.mu.Lock()
				assertFieldProjectCalls(t, agent, allPreviews...)
				if len(agent.lookups) != 1 || agent.lookups[0] != "api" {
					t.Errorf("tag lookups: %v", agent.lookups)
				}
				agent.mu.Unlock()
				if mode == "blocked" {
					t.Error("prompted for a blocked plan")
				} else if strings.Count(preview, "- replicas:") != 1 || !strings.Contains(preview, "- replicas: 2") ||
					!strings.Contains(preview, "replicas 1 -> 4") || !strings.Contains(preview, "recreate database") ||
					!strings.Contains(preview, "ghcr.io/acme/api@sha256:") {
					t.Errorf("wrong review before confirmation: %q", preview)
				}
				answer := "n"
				if mode == "confirmed" || mode == "stale later app" {
					answer = "y"
				}
				if mode == "stale later app" {
					agent.mu.Lock()
					agent.guardIDs["redis"] = 5
					agent.mu.Unlock()
				}
				if _, err := fmt.Fprintln(master, answer); err != nil {
					t.Fatal(err)
				}
				select {
				case <-prompted:
					t.Fatal("deploy prompted twice")
				case <-stopped:
				}
			case <-stopped:
				if mode != "blocked" {
					t.Fatalf("exit %d before prompt: %s", exit, &errs)
				}
			}
			tty.Close()
			<-readerDone
			want := 1
			switch mode {
			case "confirmed":
				want = 0
				allPreviews = append(allPreviews, "admit api", "admit redis", "admit zstack")
			case "stale later app":
				allPreviews = append(allPreviews, "admit api", "admit redis")
				if !strings.Contains(errs.String(), "applied release changed") || !strings.Contains(errs.String(), "not run: zstack") {
					t.Errorf("stale held selection: %s", &errs)
				}
			case "blocked":
				if strings.Count(transcript, reason) != 1 || strings.Contains(transcript, prompt) {
					t.Errorf("blocked preview: %q / %s", transcript, &errs)
				}
			}
			if exit != want {
				t.Errorf("exit %d, want %d: %s", exit, want, &errs)
			}
			assertFieldProjectCalls(t, agent, allPreviews...)
			if mode != "blocked" {
				if strings.Count(transcript, "- replicas: 2") != 1 || strings.Count(transcript, prompt) != 1 {
					t.Errorf("first app or prompt repeated: %q", transcript)
				}
				later := strings.Index(transcript, "- replicas: 1")
				compose := strings.Index(transcript, "\n    recreate database\n")
				atPrompt := strings.Index(transcript, prompt)
				if mode == "declined" && (later >= 0 || compose >= 0) ||
					mode == "confirmed" && !(atPrompt < later && later < compose) ||
					mode == "stale later app" && !(atPrompt < later && compose < 0) {
					t.Errorf("later field blocks: %q", transcript)
				}
			}
		})
	}
}

func TestBatchComposeRedactionRefusesBeforePrompt(t *testing.T) {
	manifestText := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n  zstack:\n    kind: compose\n    file: compose.yml\n    project_name: test-token-project\n    depends_on: [api]\n"
	plan := `{"app":"zstack","image":"","roles":[],"compose":{"project":"test-token-project","services":[{"name":"side","action":"keep"}],"volumes":[]}}`
	testBatchComposeRefusesBeforePrompt(t, manifestText, plan, "cannot safely display")
}

func TestProtectedBatchKeptReleaseChangesAtConfirmation(t *testing.T) {
	digest := "ghcr.io/acme/zlast@sha256:" + strings.Repeat("b", 64)
	manifestText := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n  zlast:\n    kind: image\n    image: ghcr.io/acme/zlast\n    depends_on: [api]\n    roles:\n      web: {}\n"
	agent := &fakeDeployAgent{protected: true, mixedPlans: true, applied: map[string]string{"zlast": fmt.Sprintf(`{"id":4,"digest":%q,"tag":"stable"}`, digest)}, plans: map[string]string{"zlast": fmt.Sprintf(`{"app":"zlast","image":%q,"tag":"stable","roles":["web"]}`, digest)}}
	var current atomic.Int64
	current.Store(4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/deploys") {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var req map[string]any
			if json.Unmarshal(body, &req) != nil {
				t.Error("invalid request")
				return
			}
			if req["app"] == "zlast" && req["dry_run"] != true && req["expected_applied"] != nil && req["expected_applied"] != float64(current.Load()) {
				agent.mu.Lock()
				agent.deploys = append(agent.deploys, req)
				agent.mu.Unlock()
				w.WriteHeader(http.StatusConflict)
				fmt.Fprint(w, `{"message":"applied release changed"}`)
				return
			}
		}
		agent.serve(w, r)
	}))
	defer server.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(manifestText), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := ingressCLIConfig(t, server.URL)
	master, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer tty.Close()
	prior := os.Stdin
	os.Stdin = tty
	defer func() { os.Stdin = prior }()
	var errs bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Run([]string{"deploy", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", "api=v1"}, tty, &errs)
	}()
	prompt := make(chan bool, 1)
	go func() {
		r := bufio.NewReader(master)
		var seen strings.Builder
		for {
			c, err := r.ReadByte()
			if err != nil {
				prompt <- false
				return
			}
			seen.WriteByte(c)
			if strings.Contains(seen.String(), "Proceed with this deploy?") {
				prompt <- true
				return
			}
		}
	}()
	select {
	case ok := <-prompt:
		if !ok {
			<-done
			t.Fatal("terminal closed before confirmation")
		}
	case code := <-done:
		t.Fatalf("exit %d before prompt: %s", code, errs.String())
	}
	agent.mu.Lock()
	agent.applied["zlast"] = fmt.Sprintf(`{"id":5,"digest":%q,"tag":"candidate"}`, digest)
	agent.mu.Unlock()
	current.Store(5)
	if _, err := io.WriteString(master, "y\n"); err != nil {
		t.Fatal(err)
	}
	code := <-done
	if code != 1 || !strings.Contains(errs.String(), "applied release changed") {
		t.Fatalf("exit %d stderr %q", code, errs.String())
	}
	if len(agent.deploys) != 4 {
		t.Fatalf("requests %v", agent.deploys)
	}
	if !strings.Contains(agent.applied["zlast"], `"tag":"candidate"`) {
		t.Fatal("the newer release was not retained")
	}
	preview, actual := agent.deploys[1], agent.deploys[3]
	for _, req := range []map[string]any{preview, actual} {
		if req["version"] != digest || req["version_tag"] != "stable" || req["expected_applied"] != float64(4) || req["keep_version"] != nil || req["keep_digest"] != nil {
			t.Fatalf("selection %v", req)
		}
	}
	if preview["dry_run"] != true || actual["dry_run"] == true || preview["request_id"] != actual["request_id"] {
		t.Fatalf("preview %v actual %v", preview, actual)
	}
}

func TestBatchR2RemoveOnlyRefusesBeforePrompt(t *testing.T) {
	manifestText := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n  zstack:\n    kind: compose\n    file: compose.yml\n    depends_on: [api]\n"
	plan := `{"app":"zstack","image":"","roles":[],"compose":{"project":"demo-dev-zstack","services":[{"name":"orphan","action":"remove"}],"volumes":[]}}`
	testBatchComposeRefusesBeforePrompt(t, manifestText, plan, "invalid agent deploy plan")
}

func testBatchComposeRefusesBeforePrompt(t *testing.T, manifestText, plan, wantErr string) {
	t.Helper()
	agent := &fakeDeployAgent{protected: true, mixedPlans: true, plans: map[string]string{"zstack": plan}}
	dir, cfg := deployAllFixture(t, manifestText, agent)
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	master, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer tty.Close()
	prior := os.Stdin
	os.Stdin = tty
	defer func() { os.Stdin = prior }()
	var errs bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Run([]string{"deploy", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", "api=v1"}, tty, &errs)
	}()
	prompt := make(chan bool, 1)
	go func() {
		r := bufio.NewReader(master)
		var seen strings.Builder
		for {
			c, e := r.ReadByte()
			if e != nil {
				prompt <- false
				return
			}
			seen.WriteByte(c)
			if strings.Contains(seen.String(), "Proceed with this deploy?") {
				prompt <- true
				return
			}
		}
	}()
	select {
	case prompted := <-prompt:
		if prompted {
			_, _ = io.WriteString(master, "n\n")
			<-done
			t.Fatal("prompted for an invalid Compose decision")
		}
		t.Fatal("terminal closed before deploy returned")
	case code := <-done:
		if code == 0 || !strings.Contains(errs.String(), wantErr) {
			t.Fatalf("exit %d stderr %q", code, errs.String())
		}
	}
	for _, d := range agent.deploys {
		if d["dry_run"] != true {
			t.Fatalf("mutation: %v", d)
		}
	}
}
