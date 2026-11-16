package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const fakeServerEnv = "MCPDRIVER_FAKE_SERVER"

// TestMain makes the test binary a small MCP server when it is started as one, so the driver has a real server process to run.
func TestMain(m *testing.M) {
	if os.Getenv(fakeServerEnv) == "1" {
		serveFake()
		return
	}
	os.Exit(m.Run())
}

func serveFake() {
	s := sdk.NewServer(&sdk.Implementation{Name: "fake", Version: "9.9"}, nil)
	type in struct {
		Text string `json:"text,omitempty"`
	}
	sdk.AddTool(s, &sdk.Tool{Name: "echo", Description: "echo"}, func(ctx context.Context, _ *sdk.CallToolRequest, i in) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{
			StructuredContent: map[string]any{"said": i.Text},
			Content:           []sdk.Content{&sdk.TextContent{Text: "said " + i.Text}},
		}, nil, nil
	})
	sdk.AddTool(s, &sdk.Tool{Name: "boom", Description: "always a tool error"}, func(context.Context, *sdk.CallToolRequest, in) (*sdk.CallToolResult, any, error) {
		return nil, nil, fmt.Errorf("refused: nothing to see")
	})
	sdk.AddTool(s, &sdk.Tool{Name: "leak", Description: "returns its own environment token"}, func(context.Context, *sdk.CallToolRequest, in) (*sdk.CallToolResult, any, error) {
		token := os.Getenv("BERTHY_FAKE_TOKEN")
		fmt.Fprintln(os.Stderr, "server log mentions", token)
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "token is " + token}}, StructuredContent: map[string]any{"t": token}}, nil, nil
	})
	if err := s.Run(context.Background(), &sdk.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "calls.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fakeCommand() string { return `"` + os.Args[0] + `"` }

func TestDriverRunsAScriptAndWritesTheResults(t *testing.T) {
	t.Setenv(fakeServerEnv, "1")
	script := writeScript(t, `[{"tool":"echo","args":{"text":"hi"}},{"tool":"boom"},{"tool":"echo"}]`)
	out := filepath.Join(t.TempDir(), "results.json")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--command", fakeCommand(), "--script", script, "--out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var got results
	raw, err := os.ReadFile(out)
	if err != nil || json.Unmarshal(raw, &got) != nil {
		t.Fatalf("results: %v %s", err, raw)
	}
	if got.Server.Name != "fake" || got.Server.Version != "9.9" || strings.Join(got.Tools, ",") != "boom,echo,leak" || got.Error != "" {
		t.Fatalf("server %+v tools %v error %q", got.Server, got.Tools, got.Error)
	}
	if len(got.Calls) != 3 {
		t.Fatalf("calls %+v", got.Calls)
	}
	if c := got.Calls[0]; c.Tool != "echo" || c.IsError || c.Text != "said hi" || c.Structured.(map[string]any)["said"] != "hi" {
		t.Errorf("echo: %+v", c)
	}
	if c := got.Calls[1]; !c.IsError || !strings.Contains(c.Text, "refused: nothing to see") {
		t.Errorf("a tool error must be recorded as one: %+v", c)
	}
	if got.Calls[2].Index != 2 || got.Calls[2].IsError {
		t.Errorf("a call without args: %+v", got.Calls[2])
	}
	if !strings.Contains(stdout.String(), "3 tools, 3 calls, 1 tool errors") {
		t.Errorf("summary %q", stdout.String())
	}
}

func TestDriverNeverWritesATokenValue(t *testing.T) {
	t.Setenv(fakeServerEnv, "1")
	t.Setenv("BERTHY_FAKE_TOKEN", "bty.dev.supersecretvalue")
	t.Setenv("OTHER_SECRET", "another-secret-value")
	script := writeScript(t, `[{"tool":"leak"}]`)
	out := filepath.Join(t.TempDir(), "results.json")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--command", fakeCommand(), "--script", script, "--out", out, "--redact-env", "OTHER_SECRET"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	raw, _ := os.ReadFile(out)
	for _, secret := range []string{"supersecretvalue", "another-secret-value"} {
		if strings.Contains(string(raw), secret) || strings.Contains(stderr.String(), secret) || strings.Contains(stdout.String(), secret) {
			t.Fatalf("%s was written:\nresults %s\nstderr %s", secret, raw, stderr.String())
		}
	}
	if !strings.Contains(string(raw), "token is [redacted]") {
		t.Fatalf("results %s", raw)
	}
}

func TestDriverFailsOnProtocolErrors(t *testing.T) {
	script := writeScript(t, `[{"tool":"echo"}]`)
	out := filepath.Join(t.TempDir(), "results.json")
	var stdout, stderr bytes.Buffer
	// A command that is not an MCP server: it exits without answering.
	if code := run([]string{"--command", fakeCommand() + " -test.run=^$", "--script", script, "--out", out}, &stdout, &stderr); code != 1 {
		t.Fatalf("a server that does not speak MCP: exit %d", code)
	}
	if raw, err := os.ReadFile(out); err != nil || !strings.Contains(string(raw), `"error"`) {
		t.Fatalf("the failure is not in the results: %v %s", err, raw)
	}
	t.Setenv(fakeServerEnv, "1")
	stderr.Reset()
	// The server answers an unknown tool with a protocol error, so that is a failure of the run, with the calls so far kept.
	if code := run([]string{"--command", fakeCommand(), "--script", writeScript(t, `[{"tool":"no_such_tool"}]`), "--out", out}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "unknown tool") {
		t.Fatalf("unknown tool: exit %d %s", code, stderr.String())
	}
}

func TestDriverRefusesBadUsage(t *testing.T) {
	good := writeScript(t, `[]`)
	out := filepath.Join(t.TempDir(), "r.json")
	for name, args := range map[string][]string{
		"no flags":          {},
		"no script":         {"--command", "x", "--out", out},
		"unterminated":      {"--command", `x "y`, "--script", good, "--out", out},
		"script not json":   {"--command", "x", "--script", writeScript(t, `nope`), "--out", out},
		"call without tool": {"--command", "x", "--script", writeScript(t, `[{"args":{}}]`), "--out", out},
		"missing script":    {"--command", "x", "--script", filepath.Join(t.TempDir(), "none.json"), "--out", out},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Errorf("%s: exit %d", name, code)
		}
	}
}

func TestSplitCommand(t *testing.T) {
	got, err := splitCommand(`/bin/berthy mcp -e "my env" --config 'C:\a b\c.json'  x`)
	want := []string{"/bin/berthy", "mcp", "-e", "my env", "--config", `C:\a b\c.json`, "x"}
	if err != nil || strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%q %v", got, err)
	}
}
