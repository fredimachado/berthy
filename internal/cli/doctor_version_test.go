package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"berthy/internal/version"
)

// doctorVersionServer answers the doctor with the given agent version as the agent check's detail, or without an agent
// check when agentDetail is empty. When failing is set, the answer also holds a critical check that is not OK.
func doctorVersionServer(t *testing.T, agentDetail string, failing bool) *httptest.Server {
	t.Helper()
	checks := []string{}
	if agentDetail != "" {
		checks = append(checks, fmt.Sprintf(`{"name":"agent","ok":true,"critical":true,"detail":%q}`, agentDetail))
	}
	checks = append(checks, `{"name":"host","ok":true,"critical":true,"detail":"lab"}`)
	if failing {
		checks = append(checks, `{"name":"network","ok":false,"critical":true,"detail":"lab-net: missing"}`)
	}
	body := `{"checks":[` + strings.Join(checks, ",") + `]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

// stampCLI sets the CLI's version for the test and returns the function that restores it.
func stampCLI(t *testing.T, v string) {
	t.Helper()
	saved := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = saved })
}

func TestDoctorPrintsTheCLIAndTheAgentVersion(t *testing.T) {
	stampCLI(t, "v1.0.0")
	server := doctorVersionServer(t, "v1.0.0", false)
	var out, errout bytes.Buffer
	if code := Run([]string{"doctor", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if !strings.HasPrefix(out.String(), "cli: v1.0.0\nagent: v1.0.0\n") || strings.Contains(out.String(), "note:") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestDoctorNotesADifferentReleaseOfTheAgent(t *testing.T) {
	stampCLI(t, "v1.0.0")
	server := doctorVersionServer(t, "v0.9.0", false)
	var out, errout bytes.Buffer
	if code := Run([]string{"doctor", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	want := "note: the agent runs v0.9.0, this CLI is v1.0.0; berthy setup upgrades the agent"
	if !strings.Contains(out.String(), "agent: v0.9.0\n") || !strings.Contains(out.String(), want+"\n") {
		t.Fatalf("output:\n%s", out.String())
	}
	if strings.Contains(errout.String(), "note:") {
		t.Fatalf("the note went to stderr in text mode: %q", errout.String())
	}
}

func TestDoctorNeverWarnsAboutADevBuild(t *testing.T) {
	for _, tc := range []struct{ cli, agent string }{
		{"dev", "v0.9.0"},
		{"v1.0.0", "dev (8649d6e)"},
		{"v1.0.0", "dev"},
		{"dev", "dev (8649d6e)"},
		{"v1.0.0", "v1.0.0"},
	} {
		stampCLI(t, tc.cli)
		server := doctorVersionServer(t, tc.agent, false)
		var out, errout bytes.Buffer
		if code := Run([]string{"doctor", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, &out, &errout); code != 0 {
			t.Fatalf("cli %s agent %s: exit %d: %s", tc.cli, tc.agent, code, errout.String())
		}
		if strings.Contains(out.String(), "note:") || strings.Contains(errout.String(), "note:") {
			t.Errorf("cli %s agent %s: warned: %q", tc.cli, tc.agent, out.String())
		}
	}
}

func TestDoctorSaysWhenAnOldAgentHasNoVersion(t *testing.T) {
	stampCLI(t, "v1.0.0")
	server := doctorVersionServer(t, "", false)
	var out, errout bytes.Buffer
	if code := Run([]string{"doctor", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if !strings.Contains(out.String(), "agent: unknown\n") || strings.Contains(out.String(), "note:") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestDoctorNoteKeepsTheExitCode(t *testing.T) {
	stampCLI(t, "v1.0.0")
	server := doctorVersionServer(t, "v0.9.0", true)
	var out, errout bytes.Buffer
	if code := Run([]string{"doctor", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, &out, &errout); code != 1 {
		t.Fatalf("critical failure: exit %d, want 1: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "note: the agent runs v0.9.0") {
		t.Fatalf("the note is missing with a failing check:\n%s", out.String())
	}
}

func TestDoctorJSONKeepsItsShapeAndSendsTheNoteToStderr(t *testing.T) {
	stampCLI(t, "v1.0.0")
	server := doctorVersionServer(t, "v0.9.0", false)
	var out, errout bytes.Buffer
	if code := Run([]string{"doctor", "-e", "dev", "--config", ingressCLIConfig(t, server.URL), "--json"}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	var v struct {
		Checks []struct {
			Name string `json:"name"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(out.Bytes(), &v); err != nil || len(v.Checks) != 2 || v.Checks[0].Name != "agent" {
		t.Fatalf("stdout is not the agent's JSON: %v %q", err, out.String())
	}
	if strings.Contains(out.String(), "note:") || errout.String() != "note: the agent runs v0.9.0, this CLI is v1.0.0; berthy setup upgrades the agent\n" {
		t.Fatalf("stdout %q stderr %q", out.String(), errout.String())
	}
}
