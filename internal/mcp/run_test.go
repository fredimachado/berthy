package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"berthy/internal/agentclient"
	"berthy/internal/credstore/credtest"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// lockedBuffer is a bytes.Buffer that several goroutines may write.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func writeConfig(t *testing.T, url string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	body := fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_MCP_TEST_TOKEN"}}}}}`, url)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunRefusesBadArguments(t *testing.T) {
	t.Setenv("BERTHY_MCP_TEST_TOKEN", testToken)
	good := writeConfig(t, "http://127.0.0.1:1")
	for _, tc := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{"no environment", []string{"--config", good}, 2, "valid -e environment required"},
		{"bad environment", []string{"-e", "Dev!", "--config", good}, 2, "valid -e environment required"},
		{"unknown flag", []string{"-e", "dev", "--config", good, "--follow"}, 2, "unknown argument --follow"},
		{"missing value", []string{"-e", "dev", "--config"}, 2, "missing value for --config"},
		{"unknown environment", []string{"-e", "prod", "--config", good}, 1, "unknown environment"},
		{"missing config file", []string{"-e", "dev", "--config", filepath.Join(t.TempDir(), "none.json")}, 1, "cannot read config"},
	} {
		var out, errs bytes.Buffer
		if code := Run(tc.args, strings.NewReader(""), &out, &errs); code != tc.code || !strings.Contains(errs.String(), tc.want) || out.Len() != 0 || strings.Contains(errs.String(), testToken) {
			t.Errorf("%s: exit %d stdout %q stderr %q", tc.name, code, out.String(), errs.String())
		}
	}
	t.Setenv("BERTHY_MCP_TEST_TOKEN", "")
	var out, errs bytes.Buffer
	if code := Run([]string{"-e", "dev", "--config", good}, strings.NewReader(""), &out, &errs); code != 1 || !strings.Contains(errs.String(), "no token for demo/dev: its token variable is empty") {
		t.Errorf("empty token: exit %d %q", code, errs.String())
	}
}

// isolatedUserConfig points the user configuration directory at a new directory for the rest of the test, clears
// BERTHY_CONFIG, and returns that directory as os.UserConfigDir reports it.
func isolatedUserConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv(agentclient.ConfigEnv, "")
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("HOME", home)
	t.Setenv("AppData", home)
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunFindsTheDefaultConfigFile(t *testing.T) {
	t.Setenv("BERTHY_MCP_TEST_TOKEN", testToken)
	dir := isolatedUserConfig(t)
	agent, u := newFakeAgent(t)
	agent.on("GET", "apps", 200, `[]`)
	b, err := os.ReadFile(writeConfig(t, u.String()))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "berthy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "berthy", "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if code := Run([]string{"-e", "dev"}, strings.NewReader(""), &out, &errs); code != 0 || errs.Len() != 0 {
		t.Fatalf("exit %d, stderr %q", code, errs.String())
	}
}

func TestRunNamesTheMissingDefaultFile(t *testing.T) {
	dir := isolatedUserConfig(t)
	var out, errs bytes.Buffer
	want := filepath.Join(dir, "berthy", "config.json")
	if code := Run([]string{"-e", "dev"}, strings.NewReader(""), &out, &errs); code != 1 || !strings.Contains(errs.String(), want) || out.Len() != 0 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errs.String())
	}
}

func TestRunServesOnlyProtocolOnStdout(t *testing.T) {
	t.Setenv("BERTHY_MCP_TEST_TOKEN", testToken)
	agent, u := newFakeAgent(t)
	agent.on("GET", "apps", 200, `[{"ref":{"kind":"image","name":"api"}}]`)
	config := writeConfig(t, u.String())
	dir := manifestDir(t)

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var wire, stderr lockedBuffer
	done := make(chan int, 1)
	go func() {
		code := Run([]string{"-e", "dev", "--config", config, "--manifest-dir", dir}, inR, outW, &stderr)
		outW.Close()
		done <- code
	}()
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v1"}, nil)
	cs, err := client.Connect(context.Background(), &sdk.IOTransport{Reader: io.NopCloser(io.TeeReader(outR, &wire)), Writer: inW}, nil)
	if err != nil {
		t.Fatalf("connect: %v\nstderr: %s", err, stderr.String())
	}
	res := call(t, cs, "list_resources", nil)
	if res.IsError || !strings.Contains(text(t, res), "api") {
		t.Fatalf("list_resources: %v %q", res.IsError, text(t, res))
	}
	cs.Close()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	for _, line := range strings.Split(strings.TrimSpace(wire.String()), "\n") {
		var frame struct {
			JSONRPC string `json:"jsonrpc"`
		}
		if json.Unmarshal([]byte(line), &frame) != nil || frame.JSONRPC != "2.0" {
			t.Errorf("stdout has a line that is not a protocol frame: %q", line)
		}
	}
	if strings.Contains(wire.String(), testToken) || strings.Contains(stderr.String(), testToken) {
		t.Fatal("the token was printed")
	}
	if got := agent.requests(); len(got) != 1 || got[0].Auth != "Bearer "+testToken {
		t.Fatalf("agent requests %+v", got)
	}
}

func TestRunLooksForTheProjectInTheManifestDirectory(t *testing.T) {
	t.Setenv("BERTHY_MCP_TEST_TOKEN", testToken)
	good := writeConfig(t, "http://127.0.0.1:1")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte("project: other\nnetwork: demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if code := Run([]string{"-e", "dev", "--config", good, "--manifest-dir", dir}, strings.NewReader(""), &out, &errs); code != 1 || !strings.Contains(errs.String(), "no environment dev for project other in the CLI configuration") || out.Len() != 0 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errs.String())
	}
}

func TestRunNeverUsesTheStoredToken(t *testing.T) {
	t.Setenv("BERTHY_MCP_TEST_TOKEN", "")
	s := credtest.NewMemory()
	_ = s.Set(agentclient.StoreKey("demo", "dev", agentclient.Environment{URL: "http://127.0.0.1:1", TokenEnv: "BERTHY_MCP_TEST_TOKEN"}), "admin")
	before := s.Calls()
	old := agentclient.TokenStore
	agentclient.TokenStore = s
	t.Cleanup(func() { agentclient.TokenStore = old })
	var out, errs bytes.Buffer
	code := Run([]string{"-e", "dev", "--config", writeConfig(t, "http://127.0.0.1:1")}, strings.NewReader(""), &out, &errs)
	want := "no token for demo/dev: its token variable is empty (berthy context list names it)\nberthy mcp reads the token variable only: give it an agent token made with berthy token create --agent\n"
	if code != 1 || errs.String() != want || s.Calls() != before {
		t.Fatalf("exit %d stderr %q store calls %d", code, errs.String(), s.Calls()-before)
	}
}
