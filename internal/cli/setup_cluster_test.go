package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"berthy/internal/agent"
	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/credstore/credtest"
	"berthy/internal/setup"
	"berthy/internal/state"
)

const fixtureClusterAlerts = `reminder: 0s
telegram:
  bot_token_secret: TG
  chat_id: "-1234"
webhook:
  url_secret: HOOK
  headers_secret: TG
`

func clusterAlertFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cluster.yml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

type clusterCLIServer struct {
	*setupServer
	staged       string
	mutations    int
	writeCode    int
	transportErr bool
}

func (s *clusterCLIServer) Run(ctx context.Context, script string, stdin io.Reader) (setup.Result, error) {
	res, err := s.setupServer.Run(ctx, script, stdin)
	in := s.stdins[len(s.stdins)-1]
	switch {
	case strings.Contains(script, "agent setup-cluster-secret "):
		s.mutations++
		if s.transportErr {
			return setup.Result{}, errors.New(string(in))
		}
		return setup.Result{Code: s.writeCode, Stdout: in, Stderr: in}, nil
	case strings.Contains(script, "cat > /etc/berthy/agent.json.new"):
		s.staged = string(in)
	case strings.Contains(script, "mv -f /etc/berthy/agent.json.new /etc/berthy/agent.json"):
		s.config = s.staged
	}
	return res, err
}

func withClusterCLIServer(t *testing.T, server *clusterCLIServer) *[2]any {
	t.Helper()
	made := withSetupRunner(t, server.setupServer)
	newSetupRunner = func(host string, local bool) (setup.Runner, error) {
		made[0], made[1] = host, local
		return server, nil
	}
	return made
}

func TestSetupClusterAlertsReplacePreserveAndClear(t *testing.T) {
	server := &clusterCLIServer{setupServer: installedServer(t, "ops@s1")}
	withClusterCLIServer(t, server)
	base := []string{"--host", "ops@s1", "--project", "shop", "--manifest-dir", t.TempDir(), "--json"}
	read := func(env string, args ...string) (agent.File, map[string]any) {
		t.Helper()
		code, _, errout := runSetupCmd(append(append(slices.Clone(base), "-e", env, "--network", "net-"+env), args...)...)
		if code != 0 {
			t.Fatalf("setup %s: exit %d, %s", env, code, errout)
		}
		file, err := agent.ParseFile([]byte(server.config))
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(server.config), &raw); err != nil {
			t.Fatal(err)
		}
		cluster, ok := raw["cluster"].(map[string]any)
		if !ok {
			t.Fatal("missing cluster configuration")
		}
		return file, cluster
	}
	first, enabled := read("p5", "--cluster-alerts", clusterAlertFile(t, fixtureClusterAlerts))
	if alerts, ok := enabled["alerts"].(map[string]any); !ok || alerts["reminder"] != "0s" || !strings.Contains(server.config, `"chat_id": "-1234"`) {
		t.Fatal("setup did not activate the requested destinations")
	}
	second, preserved := read("prod")
	if len(second.Environments) != 2 || second.Environments[0].Tokens[0] != first.Environments[0].Tokens[0] || !jsonEqual(t, enabled, preserved) {
		t.Fatal("adding an environment changed the first environment or the cluster configuration")
	}
	_, replaced := read("p5", "--cluster-alerts", clusterAlertFile(t, "webhook:\n  url_secret: OTHER\n"))
	if alerts, ok := replaced["alerts"].(map[string]any); !ok || len(alerts) != 1 || !strings.Contains(server.config, `"url_secret": "OTHER"`) {
		t.Fatal("replacing destinations retained settings from the earlier file")
	}
	clearedFile, cleared := read("p5", "--cluster-alerts", "none")
	if len(clearedFile.Environments) != 2 || len(cleared) != 2 || cleared["key_file"] != enabled["key_file"] || cleared["state_dir"] != enabled["state_dir"] {
		t.Fatal("clearing destinations changed ownership paths or hosted environments")
	}
	if server.ran("head -c 32 /dev/urandom") {
		t.Fatal("setup generated a replacement key")
	}
}

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	aJSON, aErr := json.Marshal(a)
	bJSON, bErr := json.Marshal(b)
	if err := errors.Join(aErr, bErr); err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(aJSON, bJSON)
}

func TestInvalidClusterAlertsFailBeforeSSH(t *testing.T) {
	server := installedServer(t, "ops@s1")
	made := withSetupRunner(t, server)
	base := []string{"-e", "p5", "--host", "ops@s1", "--manifest-dir", projectDir(t), "--cluster-alerts"}
	for _, tc := range []struct{ name, body, want string }{
		{"bad reference", "webhook:\n  url_secret: 'bad reference'\n", "url_secret must name a secret"},
		{"unknown key", "telegram:\n  extra: true\n", "cluster alerts"},
		{"deploy policy", "deploys: all\n", "cluster alerts"},
		{"heartbeat", "heartbeat:\n  url_secret: PING\n", "cluster alerts"},
		{"multiple documents", fixtureClusterAlerts + "---\n{}\n", "one YAML mapping"},
		{"null", "null\n", "one YAML mapping"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, diag := runSetupCmd(append(slices.Clone(base), clusterAlertFile(t, tc.body))...)
			if code != 2 || made[0] != nil || !strings.Contains(diag, tc.want) {
				t.Errorf("exit %d, SSH=%v, diagnostic %s", code, made[0], strings.SplitN(diag, "\n", 2)[0])
			}
		})
	}
	if code, _, diag := runSetupCmd(append(base, clusterAlertFile(t, fixtureClusterAlerts))...); code != 0 || made[0] != "ops@s1" {
		t.Fatalf("valid alerts file: exit %d, SSH=%v, %s", code, made[0], diag)
	}
}

func TestSetupClusterSecretUsesOnlyPrivilegedStdin(t *testing.T) {
	const value = "cluster-value-with-special=$characters"
	for _, tc := range []struct {
		name, input, flag string
		hidden, transport bool
		code, sent        int
	}{
		{name: "pipe CRLF", input: value + "\r\n", sent: 1},
		{name: "hidden", hidden: true, input: value, sent: 1},
		{name: "empty", input: "\r\n", code: 1},
		{name: "two lines", input: value + "\nmore\n", code: 1},
		{name: "too large", input: strings.Repeat("x", api.MaxSecretValue+1), code: 1},
		{name: "dry run never reads", hidden: true, flag: "--dry-run"},
		{name: "transport diagnostics omitted", input: value, transport: true, code: 1, sent: 1},
		{name: "installation flag", input: value, flag: "--new-token", code: 2},
		{name: "CLI config flag", input: value, flag: "--config=unused.json", code: 2},
		{name: "removal flag", input: value, flag: "--remove", code: 2},
		{name: "destination update", input: value, flag: "--cluster-alerts=none", code: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &clusterCLIServer{setupServer: installedServer(t, "ops@s1"), transportErr: tc.transport}
			withClusterCLIServer(t, server)
			oldStore := agentclient.TokenStore
			store := credtest.NewMemory()
			agentclient.TokenStore = store
			t.Cleanup(func() { agentclient.TokenStore = oldStore; store.Wait() })
			setupDoctor = func(context.Context, string, string, string, string) ([]api.Check, error) {
				t.Error("cluster setup called the authenticated doctor")
				return nil, errors.New("unexpected doctor")
			}
			var reads func() int32
			if tc.hidden {
				reads = fakeTerminal(t, tc.input).Load
			} else {
				withStdinFile(t, tc.input)
			}
			args := []string{"-e", "p5", "--host", "ops@s1", "--manifest-dir", projectDir(t), "--cluster-secret", "HOOK", "--json", "--verbose"}
			if tc.flag != "" {
				args = append(args, tc.flag)
			}
			code, out, diag := runSetupCmd(args...)
			if store.Calls() != 0 {
				t.Error("setup accessed the API token store")
			}
			if code != tc.code || server.mutations != tc.sent {
				t.Fatalf("exit %d, writer calls %d, diagnostic %s", code, server.mutations, strings.SplitN(diag, "\n", 2)[0])
			}
			if reads != nil && reads() != int32(tc.sent) {
				t.Fatal("hidden input was read the wrong number of times")
			}
			if strings.Contains(out+diag+strings.Join(server.scripts, "\n"), value) {
				t.Fatal("the value escaped privileged stdin")
			}
			for i, in := range server.stdins {
				if len(in) != 0 && (string(in) != value || !strings.Contains(server.scripts[i], "setpriv --reuid=berthy --regid=berthy --clear-groups")) {
					t.Fatal("stdin went to a command other than the privileged writer")
				}
			}
			if tc.flag == "--dry-run" && (!strings.Contains(out, "would_change") || !strings.Contains(out, "berthy.service")) {
				t.Fatal("dry run omitted the service interruption")
			}
		})
	}
}

func TestSetupRestartsTheAgentAfterAClusterSecretWriteFails(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		code       int
	}{
		{"write failure", "credential was not stored", 22},
		{"start failure after commit", "credential was stored, but berthy.service", 23},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &clusterCLIServer{setupServer: installedServer(t, "ops@s1"), writeCode: tc.code}
			withClusterCLIServer(t, server)
			withStdinFile(t, "hidden-from-error\n")
			code, out, diag := runSetupCmd("-e", "p5", "--host", "ops@s1", "--manifest-dir", projectDir(t), "--cluster-secret", "HOOK")
			if code != 1 || !strings.Contains(out, tc.want) || strings.Contains(out+diag, "hidden-from-error") {
				t.Fatalf("exit %d: %s", code, strings.SplitN(diag, "\n", 2)[0])
			}
			if !server.ran("systemctl stop berthy") || !server.ran("systemctl start berthy") || !server.ran("agent setup-probe health") {
				t.Fatal("maintenance did not arrange stop, restart and health verification")
			}
			if runtime.GOOS != "windows" {
				for _, script := range server.scripts {
					if strings.Contains(script, "agent setup-cluster-secret ") {
						assertClusterServiceRecovery(t, script, tc.code)
					}
				}
			}
		})
	}
}

func assertClusterServiceRecovery(t *testing.T, script string, code int) {
	t.Helper()
	writeCode, startCode := 1, 0
	want := "stop\nwrite\nstart\nhealth\n"
	if code == 23 {
		writeCode, startCode = 0, 1
		want = "stop\nwrite\nstart\n"
	}
	trace := filepath.Join(t.TempDir(), "service-trace")
	prelude := fmt.Sprintf(`
systemctl() {
  case "$1" in
    is-active) return 0 ;;
    stop) echo stop >> "$TRACE" ;;
    start) echo start >> "$TRACE"; return %d ;;
    show) echo active ;;
    *) return 1 ;;
  esac
}
setpriv() {
  case "$6" in
    setup-cluster-secret) echo write >> "$TRACE"; return %d ;;
    setup-probe) echo health >> "$TRACE"; return 0 ;;
    *) return 1 ;;
  esac
}
date() { echo 0; }
sleep() { exit 99; }
`, startCode, writeCode)
	cmd := exec.Command("sh", "-c", prelude+strings.ReplaceAll(script, "test -x /usr/local/bin/berthy", ":"))
	cmd.Env = append(os.Environ(), "TRACE="+trace)
	cmd.Stdin = strings.NewReader("fixture-credential")
	output, runErr := cmd.CombinedOutput()
	got, err := os.ReadFile(trace)
	if runErr == nil || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != code {
		t.Fatalf("maintenance shell should report status %d: %v, trace %q, output %q", code, runErr, got, output)
	}
	if err != nil || string(got) != want {
		t.Fatalf("maintenance command order = %q, want %q (%v)", got, want, err)
	}
}

func TestSetupReportsClusterSecretCommands(t *testing.T) {
	server := &clusterCLIServer{setupServer: installedServer(t, "ops@s1")}
	withClusterCLIServer(t, server)
	dir := projectDir(t)
	if err := os.WriteFile(filepath.Join(dir, "berthy.p5.yml"), []byte("servers:\n  actual:\n    ssh: ops@s1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	base := []string{"-e", "p5", "--server", "actual", "--manifest-dir", dir}
	code, out, diag := runSetupCmd(append(slices.Clone(base), "--cluster-alerts", clusterAlertFile(t, fixtureClusterAlerts), "--json")...)
	if code != 0 {
		t.Fatalf("cluster setup: exit %d, %s", code, strings.SplitN(diag, "\n", 2)[0])
	}
	var report setupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	var commands []string
	for _, line := range report.NextSteps {
		if strings.Contains(line, "--cluster-secret") {
			commands = append(commands, line)
		}
	}
	if len(commands) != 2 || !strings.HasSuffix(commands[0], "--cluster-secret HOOK") || !strings.HasSuffix(commands[1], "--cluster-secret TG") {
		t.Fatalf("next steps = %q", report.NextSteps)
	}
	code, out, _ = runSetupCmd(base...)
	for _, command := range commands {
		if code != 0 || !strings.Contains(command, " --server actual") || !strings.Contains(command, " --manifest-dir "+shellWord(dir)) || !strings.Contains(out, command) {
			t.Fatalf("text did not report a usable command: %q", command)
		}
	}
	code, out, _ = runSetupCmd(append(base, "--cluster-alerts", "none", "--json")...)
	if code != 0 || !strings.Contains(out, "host problems alert nobody yet: run setup again with --cluster-alerts FILE") || strings.Contains(out, "--cluster-secret") {
		t.Fatal("cleared channels did not produce the no-channel guidance")
	}
}

func TestSetupClusterSecretWritesOnlyTheClaimedStore(t *testing.T) {
	const value = "cluster-fixture-credential"
	ctx := context.Background()
	root := t.TempDir()
	cluster := agent.ClusterConfig{StateDir: filepath.Join(root, "cluster-state"), KeyFile: filepath.Join(root, "cluster-key")}
	env := agent.Hosted{Project: "shop", Environment: "prod", StateDir: filepath.Join(root, "env-state"), KeyFile: filepath.Join(root, "env-key")}
	for i, path := range []string{cluster.KeyFile, env.KeyFile} {
		if err := os.WriteFile(path, bytes.Repeat([]byte{byte(i)}, 32), 0600); err != nil {
			t.Fatal(err)
		}
	}
	open := func(dir, key, environment string) *state.Store {
		t.Helper()
		store, err := state.Open(dir, key, environment)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}
	s := open(env.StateDir, env.KeyFile, env.Environment)
	if err := s.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: "HOOK"}, []byte("environment")); err != nil {
		t.Fatal(err)
	}
	c := open(cluster.StateDir, cluster.KeyFile, "cluster")
	if err := c.Claim(ctx, "(cluster)"); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	var out, diag bytes.Buffer
	if code := Run([]string{"agent", "setup-cluster-secret", "not a name"}, &out, &diag); code != 2 || !strings.Contains(diag.String(), "setup-cluster-secret: expected one valid secret name") {
		t.Fatalf("cluster writer dispatch: exit %d, %s", code, diag.String())
	}
	if err := agent.WriteClusterSecret(ctx, cluster, "HOOK", []byte(value)); err != nil {
		t.Fatal(err)
	}
	c = open(cluster.StateDir, cluster.KeyFile, "cluster")
	if _, got, err := c.Secret(ctx, "HOOK"); err != nil || string(got) != value {
		t.Fatalf("cluster secret read failed: %v", err)
	}
	if _, got, err := s.Secret(ctx, "HOOK"); err != nil || string(got) != "environment" {
		t.Fatal("the environment secret was changed")
	}
	audit, err := c.AuditLog(ctx, 10)
	if err != nil || len(audit) != 1 || audit[0].ActorID != "setup" || audit[0].Action != "secret_set:HOOK" {
		t.Fatalf("cluster audit: %v, %v", audit, err)
	}
	for _, path := range []string{"state.db", "state.db-wal"} {
		raw, err := os.ReadFile(filepath.Join(cluster.StateDir, path))
		if err != nil && !(path == "state.db-wal" && errors.Is(err, os.ErrNotExist)) {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(value)) {
			t.Fatal("the cluster credential was stored unsealed")
		}
	}
	if err := errors.Join(s.Claim(ctx, env.Project), s.Close()); err != nil {
		t.Fatal(err)
	}
	var owner *state.OwnerError
	if err := agent.WriteClusterSecret(ctx, agent.ClusterConfig{StateDir: env.StateDir, KeyFile: env.KeyFile}, "HOOK", []byte(value)); !errors.As(err, &owner) {
		t.Fatalf("credential writer accepted an environment-owned store: %v", err)
	}
	s = open(env.StateDir, env.KeyFile, env.Environment)
	if _, got, err := s.Secret(ctx, "HOOK"); err != nil || string(got) != "environment" {
		t.Fatal("the refused write changed the environment secret")
	}
}
