package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/agentclient"
	"berthy/internal/credstore/credtest"
	"berthy/internal/setup"
)

type setupInterruptedServer struct {
	*setupServer
	cancel context.CancelFunc
	staged []byte
}

func (s *setupInterruptedServer) Run(ctx context.Context, script string, stdin io.Reader) (setup.Result, error) {
	if strings.Contains(script, "cat > /etc/berthy/agent.json.new") {
		data, _ := io.ReadAll(stdin)
		s.staged = data
		stdin = bytes.NewReader(data)
	}
	if strings.Contains(script, "systemctl restart berthy") {
		s.cancel()
		return setup.Result{}, context.Canceled
	}
	if script == "sha256sum /etc/berthy/agent.json" {
		return setup.Result{Stdout: []byte(sha(s.staged) + "  /etc/berthy/agent.json\n")}, nil
	}
	return s.setupServer.Run(ctx, script, stdin)
}

func TestSetupShowsRetainedTokenAfterInterruptedActivation(t *testing.T) {
	base := freshServer(t, "ops@s1")
	withSetupRunner(t, base)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := &setupInterruptedServer{setupServer: base, cancel: cancel}
	newSetupRunner = func(string, bool) (setup.Runner, error) { return server, nil }
	store := credtest.NewMemory()
	old := agentclient.TokenStore
	agentclient.TokenStore = store
	t.Cleanup(func() { agentclient.TokenStore = old; store.Wait() })
	var out, errout bytes.Buffer
	code := runSetupWithContext(ctx, []string{"-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", projectDir(t), "--config", filepath.Join(t.TempDir(), "cli.json"), "--json"}, &out, &errout)
	var report struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
	}
	if code != 1 || json.Unmarshal(out.Bytes(), &report) != nil || report.OK || report.Token == "" || store.Calls() != 0 {
		t.Fatal("interrupted setup did not show the retained token")
	}
}
