package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/setup"
)

// setupServer is a Runner for a server that is already set up, or lacks Docker.
type setupServer struct {
	probe   string
	scripts []string
	target  string
}

func (s *setupServer) Target() string { return s.target }

func (s *setupServer) Run(ctx context.Context, script string, stdin io.Reader) (setup.Result, error) {
	s.scripts = append(s.scripts, script)
	switch {
	case script == "id -u; id -un":
		return setup.Result{Stdout: []byte("0\nroot\n")}, nil
	case strings.Contains(script, "uname -s"):
		return setup.Result{Stdout: []byte(s.probe)}, nil
	}
	return setup.Result{}, nil
}

const setUpProbe = "os=Linux\narch=x86_64\nsystemd=yes\naddr=172.31.250.10\ncurl=/usr/bin/curl\ndocker=/usr/bin/docker\n" +
	"docker_client_version=29.9.3\ndocker_active=active\ndocker_enabled=enabled\ndocker_version=29.9.3\nswarm=active true\n" +
	"network=overlay swarm true\n"

const freshProbe = "os=Linux\narch=x86_64\nsystemd=yes\naddr=172.31.250.10\ncurl=/usr/bin/curl\n"

// withSetupRunner makes setup use server and returns the arguments the runner was made with.
func withSetupRunner(t *testing.T, server *setupServer) *[2]any {
	t.Helper()
	saved := newSetupRunner
	t.Cleanup(func() { newSetupRunner = saved })
	made := &[2]any{}
	newSetupRunner = func(host string, local bool) (setup.Runner, error) {
		made[0], made[1] = host, local
		return server, nil
	}
	return made
}

func runSetupCmd(args ...string) (int, string, string) {
	var out, errout bytes.Buffer
	code := Run(append([]string{"setup"}, args...), &out, &errout)
	return code, out.String(), errout.String()
}

func TestSetupUsageErrors(t *testing.T) {
	made := withSetupRunner(t, &setupServer{})
	empty := t.TempDir()
	tests := map[string][]string{
		"no environment":         {"--host", "ops@s1", "--listen", "10.0.0.1:7710", "--network", "n"},
		"bad environment":        {"-e", "Prod", "--host", "ops@s1", "--listen", "10.0.0.1:7710", "--network", "n"},
		"neither host nor local": {"-e", "p5", "--listen", "10.0.0.1:7710", "--network", "n"},
		"both host and local":    {"-e", "p5", "--host", "ops@s1", "--local", "--listen", "10.0.0.1:7710", "--network", "n"},
		"host as an option":      {"-e", "p5", "--host", "-oProxyCommand=x", "--listen", "10.0.0.1:7710", "--network", "n"},
		"host with a space":      {"-e", "p5", "--host", "ops@s1 id", "--listen", "10.0.0.1:7710", "--network", "n"},
		"no listen":              {"-e", "p5", "--host", "ops@s1", "--network", "n"},
		"wildcard listen":        {"-e", "p5", "--host", "ops@s1", "--listen", "0.0.0.0:7710", "--network", "n"},
		"listen without port":    {"-e", "p5", "--host", "ops@s1", "--listen", "10.0.0.1", "--network", "n"},
		"no network":             {"-e", "p5", "--host", "ops@s1", "--listen", "10.0.0.1:7710"},
		"bad network":            {"-e", "p5", "--host", "ops@s1", "--listen", "10.0.0.1:7710", "--network", "Bad Net"},
		"unknown flag":           {"-e", "p5", "--host", "ops@s1", "--listen", "10.0.0.1:7710", "--network", "n", "--force"},
		"positional":             {"-e", "p5", "--host", "ops@s1", "--listen", "10.0.0.1:7710", "--network", "n", "extra"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			code, out, errout := runSetupCmd(append([]string{"--manifest-dir", empty}, args...)...)
			if code != 2 || out != "" || errout == "" {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errout)
			}
		})
	}
	if made[0] != nil {
		t.Fatalf("a runner was made for a command that did not validate")
	}
}

func writeManifest(t *testing.T, dir, network string) {
	t.Helper()
	text := "project: demo\nnetwork: " + network + "\nprotected: true\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSetupReadsTheNetworkFromTheManifest(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "demo")
	server := &setupServer{probe: setUpProbe, target: "ops@s1"}
	withSetupRunner(t, server)
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--manifest-dir", dir, "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errout)
	}
	if !strings.Contains(out, "demo (attachable overlay)") {
		t.Fatalf("the manifest's network was not used:\n%s", out)
	}
	// The same network by flag is accepted; another one is a usage error.
	code, _, _ = runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--manifest-dir", dir, "--network", "demo")
	if code != 0 {
		t.Fatalf("a flag equal to the manifest: exit %d", code)
	}
	code, out, errout = runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--manifest-dir", dir, "--network", "other")
	if code != 2 || !strings.Contains(errout, "differs") || out != "" {
		t.Fatalf("exit %d, %q, %q", code, out, errout)
	}
}

func TestSetupRefusesAnInvalidManifest(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte("network: demo\nunknown_field: 1\n"), 0o600)
	withSetupRunner(t, &setupServer{})
	code, _, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--manifest-dir", dir, "--network", "demo")
	if code != 2 || !strings.Contains(errout, "manifest of p5") {
		t.Fatalf("exit %d: %s", code, errout)
	}
}

func TestSetupTextOutput(t *testing.T) {
	server := &setupServer{probe: setUpProbe, target: "ops@s1"}
	made := withSetupRunner(t, server)
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", t.TempDir())
	if code != 0 || errout != "" {
		t.Fatalf("exit %d: %s", code, errout)
	}
	if made[0] != "ops@s1" || made[1] != false {
		t.Fatalf("runner made with %v", made)
	}
	want := "✓ connect  ops@s1\n" +
		"✓ check    linux amd64, systemd, 172.31.250.10 assigned\n" +
		"✓ docker   Docker 29.9.3, running, enabled\n" +
		"✓ swarm    manager\n" +
		"✓ network  p5net (attachable overlay)\n" +
		"– agent    not installed by this build yet\n" +
		"setup of p5 on ops@s1: done\n"
	if out != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out, want)
	}
}

func TestSetupDryRunOutput(t *testing.T) {
	server := &setupServer{probe: freshProbe, target: "ops@s1"}
	withSetupRunner(t, server)
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", t.TempDir(), "--dry-run")
	if code != 0 || errout != "" {
		t.Fatalf("exit %d: %s", code, errout)
	}
	for _, want := range []string{"· docker   would install Docker with the official script", "· swarm    would turn on Swarm advertising 172.31.250.10",
		"· network  would create network p5net", "setup of p5 on ops@s1: dry run, 3 steps would change"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, s := range server.scripts {
		if strings.Contains(s, "get.docker.com") || strings.Contains(s, "swarm init") || strings.Contains(s, "network create") {
			t.Errorf("a dry run sent %q", s)
		}
	}
}

func TestSetupDryRunOfASetUpServer(t *testing.T) {
	withSetupRunner(t, &setupServer{probe: setUpProbe, target: "ops@s1"})
	code, out, _ := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", t.TempDir(), "--dry-run")
	if code != 0 || !strings.HasSuffix(out, "setup of p5 on ops@s1: dry run, nothing would change\n") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestSetupFailureNamesTheStep(t *testing.T) {
	withSetupRunner(t, &setupServer{probe: setUpProbe + "agent_env=staging\n", target: "ops@s1"})
	code, out, _ := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	want := "✓ connect  ops@s1\n✖ check    this server runs the agent of environment staging\nsetup of p5 on ops@s1: failed at check\n"
	if out != want {
		t.Fatalf("output:\n%s", out)
	}
}

func TestSetupJSON(t *testing.T) {
	withSetupRunner(t, &setupServer{probe: freshProbe, target: "ops@s1"})
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", t.TempDir(), "--dry-run", "--json")
	if code != 0 || errout != "" {
		t.Fatalf("exit %d: %s", code, errout)
	}
	var got struct {
		Environment string `json:"environment"`
		Target      string `json:"target"`
		DryRun      bool   `json:"dry_run"`
		OK          bool   `json:"ok"`
		Steps       []struct {
			Step   string `json:"step"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if got.Environment != "p5" || got.Target != "ops@s1" || !got.DryRun || !got.OK || len(got.Steps) != 6 {
		t.Fatalf("report = %+v", got)
	}
	if s := got.Steps[2]; s.Step != "docker" || s.Status != "would_change" || s.Detail == "" {
		t.Fatalf("docker step = %+v", s)
	}
	if s := got.Steps[5]; s.Step != "agent" || s.Status != "skipped" {
		t.Fatalf("agent step = %+v", s)
	}
}

func TestSetupJSONOfAFailure(t *testing.T) {
	withSetupRunner(t, &setupServer{probe: "os=Darwin\n", target: "ops@s1"})
	code, out, _ := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", t.TempDir(), "--json")
	var got setupReport
	if code != 1 || json.Unmarshal([]byte(out), &got) != nil || got.OK || got.Steps[len(got.Steps)-1].Status != "failed" || got.Steps[len(got.Steps)-1].Step != "check" {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if strings.Contains(out, "setup of") {
		t.Fatalf("the summary line is in the JSON output:\n%s", out)
	}
}

func TestSetupWithoutARunnerFailsTheConnectStep(t *testing.T) {
	saved := newSetupRunner
	t.Cleanup(func() { newSetupRunner = saved })
	newSetupRunner = func(string, bool) (setup.Runner, error) { return nil, errors.New("ssh was not found on PATH") }
	code, out, _ := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", t.TempDir())
	want := "✖ connect  ssh was not found on PATH\nsetup of p5 on ops@s1: failed at connect\n"
	if code != 1 || out != want {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestSetupLocalTargetsThisServer(t *testing.T) {
	made := withSetupRunner(t, &setupServer{probe: setUpProbe, target: "this server"})
	code, out, _ := runSetupCmd("-e", "p5", "--local", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", t.TempDir())
	if code != 0 || made[1] != true || !strings.Contains(out, "✓ connect  this server\n") || !strings.HasSuffix(out, "setup of p5 on this server: done\n") {
		t.Fatalf("exit %d, made %v:\n%s", code, made, out)
	}
}

func TestSetupIsInTheUsage(t *testing.T) {
	var out, errout bytes.Buffer
	if code := Run(nil, &out, &errout); code != 2 || !strings.Contains(errout.String(), "setup") {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
}
