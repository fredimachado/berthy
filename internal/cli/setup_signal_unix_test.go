//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"berthy/internal/agentclient"
	"berthy/internal/credstore/credtest"
	"berthy/internal/setup"
)

func TestRunSetupSIGTERMReportsRetainedToken(t *testing.T) {
	const childMode = "BERTHY_TEST_SETUP_SIGTERM"
	if os.Getenv(childMode) != "1" {
		child := exec.Command(os.Args[0], "-test.run=^TestRunSetupSIGTERMReportsRetainedToken$")
		child.Env = append(os.Environ(), childMode+"=1", "TMPDIR="+t.TempDir())
		if err := child.Run(); err != nil {
			t.Fatalf("setup child did not confirm recovery: %v", err)
		}
		return
	}

	signal.Reset(syscall.SIGTERM)
	t.Setenv("HOME", t.TempDir())
	base := freshServer(t, "ops@s1")
	withSetupRunner(t, base)
	server := &setupInterruptedServer{setupServer: base}
	sawCancellation := false
	server.interrupt = func(ctx context.Context) error {
		if len(server.staged) == 0 || ctx.Done() == nil {
			return errors.New("fixture did not reach restart with a cancellable context and staged configuration")
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			return err
		}
		<-ctx.Done()
		sawCancellation = true
		return ctx.Err()
	}
	newSetupRunner = func(string, bool) (setup.Runner, error) { return server, nil }
	store := credtest.NewMemory()
	old := agentclient.TokenStore
	agentclient.TokenStore = store
	t.Cleanup(func() { agentclient.TokenStore = old; store.Wait() })
	var out, errout bytes.Buffer
	args := []string{"-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net",
		"--manifest-dir", projectDir(t), "--config", filepath.Join(t.TempDir(), "cli.json"), "--new-token", "--json"}
	code := runSetup(args, &out, &errout)
	var report struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
	}
	if !sawCancellation || code != 1 || json.Unmarshal(out.Bytes(), &report) != nil || report.OK ||
		!strings.HasPrefix(report.Token, "bty.shop.p5.") || server.sawHash || store.Calls() != 0 {
		t.Fatal("SIGTERM setup did not report the retained token without storing it")
	}
}
