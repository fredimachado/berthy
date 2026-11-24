//go:build unix

package cli

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/creack/pty"
)

func TestBatchComposeRedactionRefusesBeforePrompt(t *testing.T) {
	manifestText := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n  zstack:\n    kind: compose\n    file: compose.yml\n    project_name: test-token-project\n    depends_on: [api]\n"
	plan := `{"app":"zstack","image":"","roles":[],"compose":{"project":"test-token-project","services":[{"name":"side","action":"keep"}],"volumes":[]}}`
	testBatchComposeRefusesBeforePrompt(t, manifestText, plan, "cannot safely display")
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
