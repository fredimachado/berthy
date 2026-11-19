package mcp

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// promptNames are the recipes the server offers.
var promptNames = []string{"explain_failed_deploy", "health_report", "troubleshoot_resource"}

func listPrompts(t *testing.T, cs *sdk.ClientSession) []*sdk.Prompt {
	t.Helper()
	list, err := cs.ListPrompts(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return list.Prompts
}

// promptText returns the text of the messages of a prompt, failing the test when the server refuses it.
func promptText(t *testing.T, cs *sdk.ClientSession, name string, args map[string]string) string {
	t.Helper()
	res, err := cs.GetPrompt(context.Background(), &sdk.GetPromptParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("prompts/get %s: %v", name, err)
	}
	var b strings.Builder
	for _, m := range res.Messages {
		tc, ok := m.Content.(*sdk.TextContent)
		if !ok {
			t.Fatalf("prompt message content is %T", m.Content)
		}
		b.WriteString(tc.Text)
	}
	return b.String()
}

// promptRefusal returns the JSON-RPC error that the server answers a prompts/get with, failing the test when it has none.
func promptRefusal(t *testing.T, cs *sdk.ClientSession, name string, args map[string]string) *jsonrpc.Error {
	t.Helper()
	_, err := cs.GetPrompt(context.Background(), &sdk.GetPromptParams{Name: name, Arguments: args})
	var rpc *jsonrpc.Error
	if !errors.As(err, &rpc) {
		t.Fatalf("prompts/get %s: want a JSON-RPC error, got %v", name, err)
	}
	return rpc
}

func TestPromptsListTheRecipesWithTheirArguments(t *testing.T) {
	_, u := newFakeAgent(t)
	cs := connect(t, testConfig(u, t.TempDir()))
	got := map[string]string{}
	for _, p := range listPrompts(t, cs) {
		if p.Description == "" {
			t.Errorf("%s has no description", p.Name)
		}
		var args []string
		for _, a := range p.Arguments {
			if a.Required {
				args = append(args, a.Name+":required")
			} else {
				args = append(args, a.Name)
			}
		}
		got[p.Name] = strings.Join(args, ",")
	}
	want := map[string]string{"explain_failed_deploy": "resource_name:required", "troubleshoot_resource": "resource_name:required", "health_report": ""}
	if len(got) != len(want) {
		t.Fatalf("prompts %v, want %v", got, want)
	}
	for name, args := range want {
		if got[name] != args {
			t.Errorf("%s takes %q, want %q", name, got[name], args)
		}
	}
}

func TestPromptGetFillsTheResourceNameAndNeverCallsTheAgent(t *testing.T) {
	agent, u := newFakeAgent(t)
	cs := connect(t, testConfig(u, t.TempDir()))
	text := promptText(t, cs, "explain_failed_deploy", map[string]string{"resource_name": "web"})
	if !strings.Contains(text, "newest failed deploy of web failed") || strings.Contains(text, "{resource_name}") {
		t.Fatalf("the argument is not filled in: %q", text)
	}
	if i, j := strings.Index(text, "`list_deploys`"), strings.Index(text, "`get_deploy_diagnostics`"); i < 0 || j < i {
		t.Fatalf("the recipe does not walk list_deploys then get_deploy_diagnostics: %q", text)
	}
	if text := promptText(t, cs, "troubleshoot_resource", map[string]string{"resource_name": "web"}); !strings.Contains(text, "wrong with web") {
		t.Fatalf("troubleshoot_resource does not name the app: %q", text)
	}
	if n := len(agent.requests()); n != 0 {
		t.Fatalf("a prompt called the agent %d times", n)
	}
}

func TestPromptGetRefusesAnUnknownPromptOrAMissingArgument(t *testing.T) {
	_, u := newFakeAgent(t)
	cs := connect(t, testConfig(u, t.TempDir()))
	for name, tc := range map[string]struct {
		args map[string]string
		// msg is a word the error must name: the unknown prompt, or the argument that is missing.
		msg string
	}{
		"no_such_prompt":        {nil, "no_such_prompt"},
		"explain_failed_deploy": {nil, "resource_name"},
		"troubleshoot_resource": {map[string]string{"resource_name": ""}, "resource_name"},
	} {
		rpc := promptRefusal(t, cs, name, tc.args)
		if rpc.Code != jsonrpc.CodeInvalidParams || !strings.Contains(rpc.Message, tc.msg) {
			t.Errorf("%s: code %d %q, want invalid params naming %q", name, rpc.Code, rpc.Message, tc.msg)
		}
	}
}

func TestServerInstructionsListThePromptsAndTheCapabilityIsAdvertised(t *testing.T) {
	_, u := newFakeAgent(t)
	cs := connect(t, testConfig(u, t.TempDir()))
	init := cs.InitializeResult()
	if init.Capabilities == nil || init.Capabilities.Prompts == nil {
		t.Fatal("the server does not advertise the prompts capability")
	}
	for _, name := range promptNames {
		if !strings.Contains(init.Instructions, name) {
			t.Errorf("the instructions do not list %s: %q", name, init.Instructions)
		}
	}
}

// TestPromptsNameOnlyRegisteredTools reads every recipe as a client does and checks each tool it names is one the server has.
func TestPromptsNameOnlyRegisteredTools(t *testing.T) {
	_, u := newFakeAgent(t)
	cs := connect(t, testConfig(u, t.TempDir()))
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, tool := range list.Tools {
		registered[tool.Name] = true
	}
	toolName := regexp.MustCompile("`([a-z_]+)`")
	for _, p := range listPrompts(t, cs) {
		args := map[string]string{}
		for _, a := range p.Arguments {
			args[a.Name] = "web"
		}
		named := toolName.FindAllStringSubmatch(promptText(t, cs, p.Name, args), -1)
		if len(named) == 0 {
			t.Errorf("%s names no tool", p.Name)
		}
		for _, m := range named {
			if !registered[m[1]] {
				t.Errorf("%s names %q, which is not a registered tool", p.Name, m[1])
			}
		}
	}
}

func TestEveryPromptSaysWhatToDoOnA403(t *testing.T) {
	_, u := newFakeAgent(t)
	cs := connect(t, testConfig(u, t.TempDir()))
	for _, p := range listPrompts(t, cs) {
		args := map[string]string{}
		for _, a := range p.Arguments {
			args[a.Name] = "web"
		}
		text := promptText(t, cs, p.Name, args)
		if !strings.Contains(text, "HTTP 403") || !strings.Contains(text, "scope") {
			t.Errorf("%s does not say what to do on a 403: %q", p.Name, text)
		}
	}
	if text := promptText(t, cs, "explain_failed_deploy", map[string]string{"resource_name": "web"}); !strings.Contains(text, "deploy scope") {
		t.Errorf("explain_failed_deploy does not say that diagnostics need the deploy scope: %q", text)
	}
}
