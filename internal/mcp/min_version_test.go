package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/version"
)

// stampMCPVersion sets the CLI's version for the test and restores it afterwards.
func stampMCPVersion(t *testing.T, v string) {
	t.Helper()
	saved := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = saved })
}

func TestExecuteDeployRefusesAnOlderAgentBeforeSending(t *testing.T) {
	stampMCPVersion(t, "v0.9.0")
	agent, u := newFakeAgent(t)
	agent.on("GET", "agent", 200, `{"environment":"dev","version":"v0.8.2"}`)
	agent.on("POST", "deploys", 202, `{}`)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte("min_version: 0.9.0\n"+imageManifest), 0600); err != nil {
		t.Fatal(err)
	}
	res := call(t, connect(t, testConfig(u, dir)), "execute_resource_command", map[string]any{"resource_name": "api", "command": "deploy", "version": "1.2.3"})
	if !res.IsError || !strings.Contains(text(t, res), "this manifest needs Berthy 0.9.0 or newer; the agent of dev runs v0.8.2: upgrade it with berthy setup") {
		t.Fatalf("%v %q", res.IsError, text(t, res))
	}
	for _, r := range agent.requests() {
		if r.Method == "POST" {
			t.Fatalf("the deploy was sent to an older agent: %+v", r)
		}
	}
}

func TestExecuteHintsAnOlderAgentThatRefusesTheRequestAsInvalid(t *testing.T) {
	stampMCPVersion(t, "v0.9.0")
	agent, u := newFakeAgent(t)
	agent.on("GET", "agent", 200, `{"environment":"dev","version":"v0.8.0"}`)
	agent.on("POST", "deploys", 400, `{"code":"bad_request","message":"invalid request"}`)
	res := call(t, connect(t, testConfig(u, manifestDir(t))), "execute_resource_command", map[string]any{"resource_name": "api", "command": "deploy", "version": "1.2.3"})
	want := "HTTP 400: invalid request\nthe agent runs an older release than this CLI; upgrade it with berthy setup"
	if !res.IsError || !strings.Contains(text(t, res), want) {
		t.Fatalf("%v %q", res.IsError, text(t, res))
	}
}
