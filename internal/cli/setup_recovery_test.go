package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	cancel  context.CancelFunc
	staged  []byte
	sawHash bool
}

type setupUncertainServer struct {
	*setupServer
	mode             string
	cancel           context.CancelFunc
	sawSwap, sawHash bool
	restarts         int
	swapStarted      chan struct{}
}

func (s *setupUncertainServer) Run(ctx context.Context, script string, stdin io.Reader) (setup.Result, error) {
	if s.mode == "cancel before swap" && strings.Contains(script, "cat > /etc/berthy/agent.json.new") {
		result, err := s.setupServer.Run(ctx, script, stdin)
		s.cancel()
		return result, err
	}
	if strings.Contains(script, "sha256sum /etc/berthy/agent.json") {
		s.sawHash = true
	}
	if strings.Contains(script, "mv -f /etc/berthy/agent.json.new /etc/berthy/agent.json") {
		s.sawSwap = true
		if s.swapStarted != nil {
			close(s.swapStarted)
		}
		if strings.HasPrefix(s.mode, "rollback") {
			return setup.Result{Stdout: []byte("prev_cfg=yes\n")}, nil
		}
		if s.mode == "cancel" {
			s.cancel()
			return setup.Result{}, context.Canceled
		}
		return setup.Result{Code: 255}, nil
	}
	if strings.HasPrefix(s.mode, "rollback") && strings.Contains(script, "systemctl restart berthy") {
		s.restarts++
		if s.restarts == 1 {
			return setup.Result{Code: 1}, nil
		}
		return setup.Result{}, errors.New("restart transport lost")
	}
	if strings.HasPrefix(s.mode, "rollback") && strings.Contains(script, "mv -f /etc/berthy/agent.json.prev /etc/berthy/agent.json") {
		switch s.mode {
		case "rollback 255":
			return setup.Result{Code: 255}, nil
		case "rollback error":
			return setup.Result{}, errors.New("rollback transport lost")
		}
	}
	return s.setupServer.Run(ctx, script, stdin)
}

func TestSetupShowsUnconfirmedTokenAfterInterruptedSwap(t *testing.T) {
	for _, tc := range []struct {
		mode                   string
		token, swap, uncertain bool
	}{
		{"cancel", true, true, true}, {"255", true, true, true},
		{"rollback 255", true, true, true}, {"rollback error", true, true, true},
		{"rollback complete", false, true, false}, {"cancel before swap", false, false, false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			base := freshServer(t, "ops@s1")
			withSetupRunner(t, base)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := &setupUncertainServer{setupServer: base, mode: tc.mode, cancel: cancel}
			var release, landed, abort, done chan struct{}
			if tc.mode == "cancel" || tc.mode == "255" {
				server.swapStarted = make(chan struct{})
				release, landed, abort, done = make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
				go func() {
					defer close(done)
					select {
					case <-server.swapStarted:
					case <-abort:
						return
					}
					select {
					case <-release:
						close(landed)
					case <-abort:
					}
				}()
				defer func() { close(abort); <-done }()
			}
			newSetupRunner = func(string, bool) (setup.Runner, error) { return server, nil }
			store := credtest.NewMemory()
			old := agentclient.TokenStore
			agentclient.TokenStore = store
			t.Cleanup(func() { agentclient.TokenStore = old; store.Wait() })
			var out, errout bytes.Buffer
			args := []string{"-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", projectDir(t), "--config", filepath.Join(t.TempDir(), "cli.json"), "--json"}
			if tc.mode != "255" {
				args = append(args, "--show-token")
			}
			code := runSetupWithContext(ctx, args, &out, &errout)
			var report struct {
				OK    bool   `json:"ok"`
				Token string `json:"token"`
				Steps []struct {
					Detail string `json:"detail"`
				} `json:"steps"`
			}
			if code != 1 || json.Unmarshal(out.Bytes(), &report) != nil || report.OK || (report.Token != "") != tc.token || server.sawSwap != tc.swap || server.sawHash || store.Calls() != 0 {
				t.Fatal("unconfirmed swap did not safely report its token")
			}
			if strings.Contains(report.Steps[len(report.Steps)-1].Detail, "token is shown in case the server kept the configuration") != tc.uncertain {
				t.Fatal("uncertain state was not explained")
			}
			if release != nil {
				select {
				case <-landed:
					t.Fatal("remote swap completed before the report")
				default:
				}
				close(release)
				<-landed
			}
		})
	}
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
		s.sawHash = true
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
	if code != 1 || json.Unmarshal(out.Bytes(), &report) != nil || report.OK || report.Token == "" || server.sawHash || store.Calls() != 0 {
		t.Fatal("interrupted setup did not show the retained token")
	}
}
