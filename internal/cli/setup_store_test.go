package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/credstore/credtest"
)

type setupTokenWire struct {
	Token       string   `json:"token"`
	TokenStored bool     `json:"token_stored"`
	NextSteps   []string `json:"next_steps"`
}

func setupWithStore(t *testing.T, flags ...string) (setupTokenWire, string, *credtest.Memory, string) {
	t.Helper()
	store := credtest.NewMemory()
	old := agentclient.TokenStore
	agentclient.TokenStore = store
	t.Cleanup(func() { agentclient.TokenStore = old; store.Wait() })
	server := freshServer(t, "ops@s1")
	withSetupRunner(t, server)
	cfg := filepath.Join(t.TempDir(), "cli.json")
	config := `{"projects":{"shop":{"environments":{"p5":{"url":"http://old:7710","token_env":"CUSTOM_TOKEN"}}}}}`
	if err := os.WriteFile(cfg, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CUSTOM_TOKEN", "")
	args := []string{"-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", projectDir(t), "--config", cfg, "--json"}
	args = append(args, flags...)
	code, out, _ := runSetupCmd(args...)
	if code != 0 {
		t.Fatalf("setup exit %d", code)
	}
	var report setupTokenWire
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	return report, out, store, cfg
}

func TestSetupSecondRunFindsTheStoredToken(t *testing.T) {
	_, _, store, cfg := setupWithStore(t)
	key := agentclient.StoreKey("shop", "p5", agentclient.Environment{URL: "http://172.31.250.10:7710", TokenEnv: "CUSTOM_TOKEN"})
	before, err := store.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	withSetupRunner(t, installedServer(t, "ops@s1"))
	var verified string
	setupDoctor = func(_ context.Context, _, _, _, token string) ([]api.Check, error) {
		verified = token
		return []api.Check{{Name: "agent", OK: true, Critical: true}}, nil
	}
	beforeCalls := store.Calls()
	code, out, _ := runSetupCmd("-e", "p5", "--host", "ops@s1", "--network", "p5net", "--manifest-dir", projectDir(t), "--config", cfg)
	lookupCalls := store.Calls() - beforeCalls
	if lookupCalls != 1 {
		t.Errorf("repeat setup made %d store calls, want one lookup", lookupCalls)
	}
	after, err := store.Get(key)
	if code != 0 || err != nil || before != after || verified != before || store.Len() != 1 || !strings.Contains(out, "the admin token is in the test store") || strings.Contains(out, before) {
		t.Fatal("second setup did not retain the stored token")
	}
	beforeCalls = store.Calls()
	code, out, _ = runSetupCmd("-e", "p5", "--host", "ops@s1", "--network", "p5net", "--manifest-dir", projectDir(t), "--config", cfg, "--dry-run")
	if code != 0 || store.Calls()-beforeCalls != 1 || !strings.Contains(out, "the admin token is in the test store") {
		t.Fatal("dry run did not reuse the stored-token lookup")
	}
}

func TestSetupASaveThatCompletesLateStillShowsTheToken(t *testing.T) {
	authorization := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization <- r.Header.Get("Authorization")
		fmt.Fprint(w, `[]`)
	}))
	defer server.Close()
	store := credtest.NewMemory()
	oldStore, oldWait := agentclient.TokenStore, agentclient.StoreWait
	agentclient.TokenStore, agentclient.StoreWait = store, time.Millisecond
	defer func() { agentclient.TokenStore, agentclient.StoreWait = oldStore, oldWait }()
	runner := freshServer(t, "ops@s1")
	runner.probe = strings.ReplaceAll(runner.probe, "172.31.250.10", "127.0.0.1")
	withSetupRunner(t, runner)
	cfg := filepath.Join(t.TempDir(), "cli.json")
	dir := projectDir(t)
	listen := strings.TrimPrefix(server.URL, "http://")
	var report setupReport
	synctest.Test(t, func(t *testing.T) {
		block := make(chan struct{})
		started := make(chan struct{})
		store.Block = block
		store.OnCall = func() {
			select {
			case <-started:
			default:
				close(started)
			}
		}
		defer func() { close(block); store.Wait() }()
		var out, errout bytes.Buffer
		code := runSetupWithContext(context.Background(), []string{"-e", "p5", "--host", "ops@s1", "--listen", listen, "--network", "p5net", "--manifest-dir", dir, "--config", cfg, "--json"}, &out, &errout)
		select {
		case <-started:
		default:
			t.Fatal("store call did not start")
		}
		if code != 0 || json.Unmarshal(out.Bytes(), &report) != nil || report.Token == "" || report.TokenStored || !strings.Contains(out.String(), "could not confirm the save in the test store") {
			t.Fatal("unconfirmed save did not show the token")
		}
	})
	store.Block, store.OnCall = nil, nil
	code, _, _ := runCLI("ps", "-e", "p5", "--project", "shop", "--config", cfg)
	if code != 0 {
		t.Fatal("later command failed")
	}
	select {
	case got := <-authorization:
		if got != "Bearer "+report.Token {
			t.Fatal("later command did not authenticate with the saved token")
		}
	default:
		t.Fatal("later command did not reach the server")
	}
}

func TestSetupSavesTheTokenAndDoesNotPrintIt(t *testing.T) {
	r, out, store, _ := setupWithStore(t)
	key := agentclient.StoreKey("shop", "p5", agentclient.Environment{URL: "http://172.31.250.10:7710", TokenEnv: "CUSTOM_TOKEN"})
	token, err := store.Get(key)
	if err != nil || token == "" || !r.TokenStored || r.Token != "" || strings.Contains(out, token) {
		t.Fatal("saved token was missing or appeared in output")
	}
}

func TestSetupShowTokenPrintsItToo(t *testing.T) {
	r, _, store, _ := setupWithStore(t, "--show-token")
	key := agentclient.StoreKey("shop", "p5", agentclient.Environment{URL: "http://172.31.250.10:7710", TokenEnv: "CUSTOM_TOKEN"})
	token, _ := store.Get(key)
	if !r.TokenStored || r.Token == "" || r.Token != token {
		t.Fatal("shown token did not match saved token")
	}
	for _, step := range r.NextSteps {
		if strings.Contains(step, "store the admin token above") {
			t.Fatal("stored token was described as unstored")
		}
	}
}

func TestSetupJSONOmitsAStoredToken(t *testing.T) {
	r, out, _, _ := setupWithStore(t)
	if !r.TokenStored || strings.Contains(out, `"token":`) || !strings.Contains(out, `"token_stored": true`) {
		t.Fatal("wrong JSON token fields")
	}
}

func TestSetupAFailedSaveStillEndsOK(t *testing.T) {
	store := credtest.NewMemory()
	store.SetErr = errors.New("failed")
	old := agentclient.TokenStore
	agentclient.TokenStore = store
	t.Cleanup(func() { agentclient.TokenStore = old })
	withSetupRunner(t, freshServer(t, "ops@s1"))
	code, out, _ := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", projectDir(t), "--config", filepath.Join(t.TempDir(), "cli.json"))
	if code != 0 || !strings.Contains(out, "admin token for shop/p5") || !strings.Contains(out, "no credential store on this machine") {
		t.Fatal("failed save did not show token")
	}
}
