package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"berthy/deploy/systemd"
	"berthy/internal/agent"
	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/setup"
)

// setupServer is a Runner for a server that is already set up, or lacks Docker or the agent.
type setupServer struct {
	probe   string
	config  string // what reading the agent's configuration returns
	scripts []string
	stdins  [][]byte
	target  string
	waits   []int // exit codes of the restart-and-wait scripts
}

func (s *setupServer) Target() string { return s.target }

func (s *setupServer) Run(ctx context.Context, script string, stdin io.Reader) (setup.Result, error) {
	s.scripts = append(s.scripts, script)
	var in []byte
	if stdin != nil {
		in, _ = io.ReadAll(stdin)
	}
	s.stdins = append(s.stdins, in)
	switch {
	case script == "id -u; id -un":
		return setup.Result{Stdout: []byte("0\nroot\n")}, nil
	case strings.Contains(script, "uname -s"):
		return setup.Result{Stdout: []byte(s.probe)}, nil
	case script == "cat /etc/berthy/agent.json":
		return setup.Result{Stdout: []byte(s.config)}, nil
	case strings.Contains(script, "cat > /usr/local/bin/berthy.new"):
		sum := sha256.Sum256(in)
		return setup.Result{Stdout: []byte(hex.EncodeToString(sum[:]) + "  /usr/local/bin/berthy.new\n")}, nil
	case strings.Contains(script, "sha256sum /var/lib/berthy/key"):
		sum := sha256.Sum256(in)
		return setup.Result{Stdout: []byte(hex.EncodeToString(sum[:]) + "  /var/lib/berthy/key\n")}, nil
	case strings.Contains(script, "systemctl restart berthy"):
		code := 0
		if len(s.waits) > 0 {
			code, s.waits = s.waits[0], s.waits[1:]
		}
		return setup.Result{Code: code}, nil
	}
	return setup.Result{}, nil
}

func (s *setupServer) ran(marker string) bool {
	for _, sc := range s.scripts {
		if strings.Contains(sc, marker) {
			return true
		}
	}
	return false
}

const freshProbe = "os=Linux\narch=x86_64\nsystemd=yes\naddr=172.31.250.10\ncurl=/usr/bin/curl\n"

const dockerProbe = "os=Linux\narch=x86_64\nsystemd=yes\naddr=172.31.250.10\ncurl=/usr/bin/curl\ndocker=/usr/bin/docker\n" +
	"docker_client_version=29.9.3\ndocker_active=active\ndocker_enabled=enabled\ndocker_version=29.9.3\nswarm=active true\n" +
	"network=overlay swarm true\n"

// elfBinary writes the header of an executable ELF file for x86-64 and returns its path.
func elfBinary(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	b.Write([]byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	for _, v := range []any{uint16(2), uint16(62), uint32(1), uint64(0x400000), uint64(0), uint64(0), uint32(0),
		uint16(64), uint16(56), uint16(0), uint16(64), uint16(0), uint16(0)} {
		if err := binary.Write(&b, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	b.WriteString("agent")
	path := filepath.Join(t.TempDir(), "berthy")
	if err := os.WriteFile(path, b.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func sha(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

// withSetupBuild makes setup see a Linux amd64 CLI that is the executable at exe.
func withSetupBuild(t *testing.T, exe string) {
	t.Helper()
	saved, savedDoctor := setupBuild, setupDoctor
	t.Cleanup(func() { setupBuild, setupDoctor = saved, savedDoctor })
	setupBuild = func() setup.Build {
		return setup.Build{OS: "linux", Arch: "amd64", Path: exe, Version: "v0.0.1-test"}
	}
	setupDoctor = func(context.Context, string, string, string) ([]api.Check, error) {
		return []api.Check{{Name: "agent", OK: true, Critical: true, Detail: "v0.0.1-test"}}, nil
	}
	t.Setenv("BERTHY_P5_TOKEN", "")
	t.Setenv("BERTHY_RELEASE_URL", "")
}

// installedServer is a server whose agent is set up with the CLI of withSetupBuild.
func installedServer(t *testing.T, target string) *setupServer {
	t.Helper()
	exe := elfBinary(t)
	withSetupBuild(t, exe)
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := json.MarshalIndent(agent.Config{
		Environment: "p5", Listen: "172.31.250.10:7710", DockerSocket: "/var/run/docker.sock", StateDir: "/var/lib/berthy/state", KeyFile: "/var/lib/berthy/key",
		Network: "p5net", Tokens: []agent.Token{{Name: "admin", Digest: strings.Repeat("a", 64), Scope: "admin"}},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	probe := dockerProbe + "agent_config=yes\nagent_config_stat=berthy:berthy 600\nagent_env=p5\nbin_sha=" + sha(b) + "\nuser=999\nuser_groups=berthy docker\n" +
		"unit_sha=" + sha(bytes.ReplaceAll(systemd.Unit, []byte("\r\n"), []byte("\n"))) + "\nservice_active=active\nservice_enabled=enabled\n" +
		"data_dir_stat=berthy:berthy 700\nstate_dir_stat=berthy:berthy 700\nstate_db=yes\nkey_sha=" + strings.Repeat("b", 64) + "\nkey_stat=32 berthy:berthy 600\n"
	return &setupServer{probe: probe, config: string(cfg) + "\n", target: target}
}

// freshServer is a server that has Docker, Swarm and the network but no agent, with this CLI as the one to install.
func freshServer(t *testing.T, target string) *setupServer {
	t.Helper()
	withSetupBuild(t, elfBinary(t))
	return &setupServer{probe: dockerProbe, target: target}
}

// withSetupRunner makes setup use server and returns the arguments the runner was made with.
func withSetupRunner(t *testing.T, server *setupServer) *[2]any {
	t.Helper()
	// Setup writes the CLI configuration when it is not told where, so each test gets a user configuration directory of its own.
	isolatedUserConfig(t)
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
	server := installedServer(t, "ops@s1")
	server.config = strings.ReplaceAll(server.config, "p5net", "demo")
	withSetupRunner(t, server)
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--manifest-dir", dir, "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errout)
	}
	if !strings.Contains(out, "demo (attachable overlay)") {
		t.Fatalf("the manifest's network was not used:\n%s", out)
	}
	// The manifest says protected: the configuration is rewritten with it.
	if !strings.Contains(out, `"config"`) || !server.ran("cat > /etc/berthy/agent.json.new") {
		t.Fatalf("the configuration was not updated:\n%s", out)
	}
	var written []byte
	for i, sc := range server.scripts {
		if strings.Contains(sc, "cat > /etc/berthy/agent.json.new") {
			written = server.stdins[i]
		}
	}
	if !bytes.Contains(written, []byte(`"protected": true`)) || !bytes.Contains(written, []byte(`"network": "demo"`)) {
		t.Fatalf("configuration = %s", written)
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

func TestSetupTextOutputOfASetUpServer(t *testing.T) {
	server := installedServer(t, "ops@s1")
	made := withSetupRunner(t, server)
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", t.TempDir())
	if code != 0 || errout != "" {
		t.Fatalf("exit %d: %s", code, errout)
	}
	if made[0] != "ops@s1" || made[1] != false {
		t.Fatalf("runner made with %v", made)
	}
	want := "✓ connect     ops@s1\n" +
		"✓ check       linux amd64, systemd, 172.31.250.10 assigned\n" +
		"✓ docker      Docker 29.9.3, running, enabled\n" +
		"✓ swarm       manager\n" +
		"✓ network     p5net (attachable overlay)\n" +
		"✓ user        berthy, in the docker group\n" +
		"✓ binary      this CLI (v0.0.1-test) is installed\n" +
		"✓ key         the key and the state directory are in place\n" +
		"✓ token       the tokens of /etc/berthy/agent.json are kept\n" +
		"✓ config      /etc/berthy/agent.json (listen 172.31.250.10:7710, 1 token)\n" +
		"✓ service     berthy.service is running and enabled\n" +
		"✓ cli_config  wrote p5 at http://172.31.250.10:7710 to config.json (token in $BERTHY_P5_TOKEN)\n" +
		"✓ verify      the agent answers; run berthy doctor with an admin token to check it fully\n" +
		"setup of p5 on ops@s1: done\n" +
		"\nnext steps:\n" +
		"  1. keep a copy of the encryption key off the server, in a password manager or your provisioning vault: ssh ops@s1 sudo cat /var/lib/berthy/key > p5.key\n" +
		"  2. set the secrets the manifest names: berthy secret set NAME -e p5\n" +
		"  3. deploy every app in depends_on order, with --version APP=REF for each image app: berthy deploy -e p5 --version APP=REF (or one app: berthy deploy APP -e p5 --version REF)\n"
	if out != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out, want)
	}
	for _, sc := range server.scripts {
		if strings.Contains(sc, "mktemp") || strings.Contains(sc, "systemctl restart") || strings.Contains(sc, "chown") {
			t.Errorf("a set-up server was changed: %q", sc)
		}
	}
}

func TestSetupOfAFreshServerPrintsTheTokenOnceAndNeverSendsIt(t *testing.T) {
	server := freshServer(t, "ops@s1")
	withSetupRunner(t, server)
	cfgPath := filepath.Join(t.TempDir(), "cli.json")
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", t.TempDir(), "--config", cfgPath)
	if code != 0 || errout != "" {
		t.Fatalf("exit %d: %s\n%s", code, errout, out)
	}
	var token string
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "admin token for p5 (shown once, store it now): "); ok {
			token = rest
		}
	}
	if !strings.HasPrefix(token, "bty.p5.") || len(token) != len("bty.p5.")+64 || strings.Count(out, token) != 1 {
		t.Fatalf("token = %q, output:\n%s", token, out)
	}
	for _, want := range []string{"✓ user        created the berthy user, added it to the docker group", "✓ token       created an admin token named admin; it is shown once, at the end",
		"✓ service     installed, enabled and started berthy.service", "✓ cli_config  wrote p5 at http://172.31.250.10:7710 to cli.json (token in $BERTHY_P5_TOKEN)",
		"2. store the admin token above, and set it in $BERTHY_P5_TOKEN when you run berthy",
		"3. set the secrets the manifest names: berthy secret set NAME -e p5 --config " + shellWord(cfgPath),
		"4. deploy every app in depends_on order, with --version APP=REF for each image app: berthy deploy -e p5 --config " + shellWord(cfgPath) + " --version APP=REF (or one app: berthy deploy APP -e p5 --config " + shellWord(cfgPath) + " --version REF)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// The token reached no script and no stdin; the configuration holds its digest.
	for i, sc := range server.scripts {
		if strings.Contains(sc, token) || bytes.Contains(server.stdins[i], []byte(token)) {
			t.Errorf("script %d holds the token", i)
		}
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil || bytes.Contains(data, []byte(token)) || !bytes.Contains(data, []byte(`"token_env": "BERTHY_P5_TOKEN"`)) {
		t.Fatalf("CLI config %s: %v", data, err)
	}
	found := false
	for _, in := range server.stdins {
		if bytes.Contains(in, []byte(agent.Digest(token))) {
			found = true
		}
	}
	if !found {
		t.Errorf("the digest was not sent")
	}
}

func TestSetupWritesTheDefaultConfigWhenNotTold(t *testing.T) {
	server := freshServer(t, "ops@s1")
	withSetupRunner(t, server)
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", t.TempDir())
	if code != 0 || errout != "" {
		t.Fatalf("exit %d: %s\n%s", code, errout, out)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "berthy", "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the default CLI configuration was not written: %v\n%s", err, out)
	}
	c, err := agentclient.ParseConfig(data)
	if err != nil || c.Environments["p5"] != (agentclient.Environment{URL: "http://172.31.250.10:7710", TokenEnv: "BERTHY_P5_TOKEN"}) {
		t.Fatalf("configuration %s: %v", data, err)
	}
	if strings.Contains(out, "--config") {
		t.Errorf("the next steps name --config although it was not given:\n%s", out)
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(filepath.Dir(path)); err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("directory of the configuration: %v %v", st, err)
		}
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("configuration file: %v %v", st, err)
		}
	}
}

func TestSetupSkipsTheConfigWhenThereIsNoDefaultLocation(t *testing.T) {
	server := freshServer(t, "ops@s1")
	withSetupRunner(t, server)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("AppData", "")
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", t.TempDir())
	if code != 0 || errout != "" || !strings.Contains(out, "– cli_config  add {") || !strings.Contains(out, "give --config or set BERTHY_CONFIG") {
		t.Fatalf("exit %d: %s\n%s", code, errout, out)
	}
}

func TestSetupJSONHasTheTokenAndTheNextSteps(t *testing.T) {
	server := freshServer(t, "ops@s1")
	withSetupRunner(t, server)
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", t.TempDir(), "--json")
	if code != 0 || errout != "" {
		t.Fatalf("exit %d: %s\n%s", code, errout, out)
	}
	var got setupReport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if !got.OK || !strings.HasPrefix(got.Token, "bty.p5.") || strings.Count(out, got.Token) != 1 || len(got.NextSteps) != 4 || len(got.Steps) != 13 {
		t.Fatalf("report = %+v", got)
	}
	if !strings.Contains(got.NextSteps[0], "ssh ops@s1 sudo cat /var/lib/berthy/key > p5.key") {
		t.Errorf("next step = %q", got.NextSteps[0])
	}
	steps := strings.Join(got.NextSteps, "\n")
	if !strings.Contains(steps, "berthy secret set NAME -e p5") || !strings.Contains(steps, "berthy deploy APP -e p5 --version REF") {
		t.Errorf("next steps = %q", got.NextSteps)
	}
	if strings.Contains(steps, "--config") {
		t.Errorf("next steps name --config although it was not given: %q", got.NextSteps)
	}
	for _, step := range got.NextSteps {
		if strings.Contains(step, got.Token) {
			t.Errorf("a next step holds the token")
		}
	}
	// A second run reports no token.
	second := installedServer(t, "ops@s1")
	withSetupRunner(t, second)
	_, out, _ = runSetupCmd("-e", "p5", "--host", "ops@s1", "--network", "p5net", "--manifest-dir", t.TempDir(), "--json")
	if strings.Contains(out, `"token": `) || !strings.Contains(out, `"next_steps"`) {
		t.Fatalf("second report:\n%s", out)
	}
}

func TestSetupNextStepsMentionTheIngressOnlyWhenTheManifestHasOne(t *testing.T) {
	steps := setupNextSteps("prod", "", false, "ops@s1", "BERTHY_PROD_TOKEN", true, true)
	if len(steps) != 5 || !strings.Contains(steps[3], "berthy ingress apply -e prod") || strings.Contains(steps[3], "--config") {
		t.Fatalf("steps = %q", steps)
	}
	local := setupNextSteps("prod", "", true, "this server", "X", false, false)
	if len(local) != 3 || strings.Contains(local[0], "ssh") || !strings.Contains(local[0], "sudo cat /var/lib/berthy/key > prod.key") {
		t.Fatalf("local steps = %q", local)
	}
}

func TestSetupNextStepsNameTheConfigGivenToSetup(t *testing.T) {
	steps := setupNextSteps("prod", "/etc/berthy/cli.json", false, "ops@s1", "BERTHY_PROD_TOKEN", false, false)
	want := []string{
		"berthy secret set NAME -e prod --config /etc/berthy/cli.json",
		"berthy deploy APP -e prod --config /etc/berthy/cli.json",
	}
	if len(steps) != 3 || !strings.Contains(steps[1], want[0]) || !strings.Contains(steps[2], want[1]) {
		t.Fatalf("steps = %q", steps)
	}
	for _, step := range steps {
		if strings.Contains(step, "CONFIG") {
			t.Errorf("a step names the placeholder CONFIG although the config was given: %q", step)
		}
	}
}

func TestSetupNextStepsQuoteTheConfigWhenItNeedsIt(t *testing.T) {
	tests := map[string]string{
		"/etc/berthy/cli.json": "/etc/berthy/cli.json",
		"~/berthy/cli.json":    "~/berthy/cli.json",
		"my cli.json":          "'my cli.json'",
		"/tmp/my dir/cli.json": "'/tmp/my dir/cli.json'",
		`C:\Users\me\cli.json`: `'C:\Users\me\cli.json'`,
		"it's.json":            `'it'\''s.json'`,
		"$HOME/cli.json":       "'$HOME/cli.json'",
	}
	for cfg, want := range tests {
		// The ingress step is among the steps: it takes the config too.
		steps := setupNextSteps("prod", cfg, false, "ops@s1", "BERTHY_PROD_TOKEN", false, true)
		if len(steps) != 4 {
			t.Fatalf("steps = %q", steps)
		}
		for _, step := range steps[1:] {
			if !strings.Contains(step, " --config "+want) {
				t.Errorf("config %q: step %q does not name --config %s", cfg, step, want)
			}
		}
	}
}

func TestSetupListenIsOptionalWhenTheServerHasAConfiguration(t *testing.T) {
	server := installedServer(t, "ops@s1")
	withSetupRunner(t, server)
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--network", "p5net", "--manifest-dir", t.TempDir())
	if code != 0 || !strings.Contains(out, "172.31.250.10 assigned") {
		t.Fatalf("exit %d: %s\n%s", code, errout, out)
	}
	// On a first run it is needed, and the step that finds out says so.
	fresh := freshServer(t, "ops@s1")
	withSetupRunner(t, fresh)
	code, out, _ = runSetupCmd("-e", "p5", "--host", "ops@s1", "--network", "p5net", "--manifest-dir", t.TempDir())
	if code != 1 || !strings.Contains(out, "✖ check") || !strings.Contains(out, "--listen ADDRESS:PORT is needed") || !strings.HasSuffix(out, "failed at check\n") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	// Another address than the agent's is refused.
	other := installedServer(t, "ops@s1")
	other.probe += "addr=172.31.250.11\n"
	withSetupRunner(t, other)
	code, out, _ = runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.11:7710", "--network", "p5net", "--manifest-dir", t.TempDir())
	if code != 1 || !strings.Contains(out, "the agent listens on 172.31.250.10:7710; setup does not move it") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestSetupPassesItsFlagsToTheSteps(t *testing.T) {
	server := freshServer(t, "ops@s1")
	withSetupRunner(t, server)
	key := filepath.Join(t.TempDir(), "saved.key")
	if err := os.WriteFile(key, bytes.Repeat([]byte{7}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	other := elfBinary(t)
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", t.TempDir(),
		"--binary", other, "--key-file", key, "--new-token", "--protected")
	if code != 0 || errout != "" {
		t.Fatalf("exit %d: %s\n%s", code, errout, out)
	}
	if !strings.Contains(out, "uploaded --binary berthy") || !strings.Contains(out, "installed the key from --key-file") {
		t.Errorf("output:\n%s", out)
	}
	var cfg []byte
	for i, sc := range server.scripts {
		if strings.Contains(sc, "cat > /etc/berthy/agent.json.new") {
			cfg = server.stdins[i]
		}
		if bytes.Contains([]byte(sc), bytes.Repeat([]byte{7}, 32)) {
			t.Errorf("the key is in a script")
		}
	}
	if !bytes.Contains(cfg, []byte(`"protected": true`)) {
		t.Errorf("configuration = %s", cfg)
	}
	// --new-token on a server with no configuration is the same as the first token.
	if strings.Contains(out, "another admin token") {
		t.Errorf("output:\n%s", out)
	}
}

func TestSetupDryRunOutput(t *testing.T) {
	server := freshServer(t, "ops@s1")
	server.probe = freshProbe
	withSetupRunner(t, server)
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", t.TempDir(), "--dry-run")
	if code != 0 || errout != "" {
		t.Fatalf("exit %d: %s", code, errout)
	}
	for _, want := range []string{"· docker      would install Docker with the official script", "· swarm       would turn on Swarm advertising 172.31.250.10",
		"· network     would create network p5net", "· token       would create an admin token", "· cli_config  would write p5 to config.json", "– verify      would check the agent"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "dry run, 10 steps would change\n") || strings.Contains(out, "next steps") || strings.Contains(out, "bty.") {
		t.Errorf("output:\n%s", out)
	}
	for _, s := range server.scripts {
		if strings.Contains(s, "get.docker.com") || strings.Contains(s, "swarm init") || strings.Contains(s, "network create") || strings.Contains(s, "useradd") || strings.Contains(s, "mktemp") {
			t.Errorf("a dry run sent %q", s)
		}
	}
}

func TestSetupDryRunOfASetUpServer(t *testing.T) {
	withSetupRunner(t, installedServer(t, "ops@s1"))
	code, out, _ := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", t.TempDir(), "--dry-run")
	// The agent is set up, but this machine has no CLI configuration yet, so the local step would change.
	if code != 0 || !strings.Contains(out, "· cli_config  would write p5 to config.json\n") || !strings.HasSuffix(out, "setup of p5 on ops@s1: dry run, 1 step would change\n") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestSetupFailureNamesTheStep(t *testing.T) {
	server := installedServer(t, "ops@s1")
	server.probe += "agent_env=staging\n"
	withSetupRunner(t, server)
	code, out, _ := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	want := "✓ connect     ops@s1\n✖ check       this server runs the agent of environment staging\nsetup of p5 on ops@s1: failed at check\n"
	if out != want {
		t.Fatalf("output:\n%s", out)
	}
}

func TestSetupAFailedFirstInstallStillShowsTheToken(t *testing.T) {
	server := freshServer(t, "ops@s1")
	server.waits = []int{1}
	withSetupRunner(t, server)
	code, out, _ := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", t.TempDir())
	if code != 1 || !strings.Contains(out, "✖ service") || !strings.Contains(out, "failed at service") || !strings.Contains(out, "admin token for p5 (shown once, store it now): bty.p5.") || strings.Contains(out, "next steps") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestSetupJSON(t *testing.T) {
	server := freshServer(t, "ops@s1")
	server.probe = freshProbe
	withSetupRunner(t, server)
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
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if got.Environment != "p5" || got.Target != "ops@s1" || !got.DryRun || !got.OK || len(got.Steps) != 13 || got.Token != "" {
		t.Fatalf("report = %+v", got)
	}
	if s := got.Steps[2]; s.Step != "docker" || s.Status != "would_change" || s.Detail == "" {
		t.Fatalf("docker step = %+v", s)
	}
	if s := got.Steps[12]; s.Step != "verify" || s.Status != "skipped" {
		t.Fatalf("verify step = %+v", s)
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
	want := "✖ connect     ssh was not found on PATH\nsetup of p5 on ops@s1: failed at connect\n"
	if code != 1 || out != want {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestSetupLocalTargetsThisServer(t *testing.T) {
	made := withSetupRunner(t, installedServer(t, "this server"))
	code, out, _ := runSetupCmd("-e", "p5", "--local", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", t.TempDir())
	if code != 0 || made[1] != true || !strings.Contains(out, "✓ connect     this server\n") || !strings.Contains(out, "setup of p5 on this server: done\n") ||
		!strings.Contains(out, "sudo cat /var/lib/berthy/key > p5.key") || strings.Contains(out, "ssh this server") {
		t.Fatalf("exit %d, made %v:\n%s", code, made, out)
	}
}

func TestSetupIsInTheUsage(t *testing.T) {
	var out, errout bytes.Buffer
	if code := Run(nil, &out, &errout); code != 2 || !strings.Contains(errout.String(), "setup") {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
}

func TestSetupUsageNamesTheNewFlags(t *testing.T) {
	for _, flag := range []string{"--binary", "--key-file", "--new-token", "--config", "[--listen ADDRESS:PORT]"} {
		if !strings.Contains(setupUsage, flag) {
			t.Errorf("the usage lacks %s", flag)
		}
	}
}

func TestSetupJSONKeepsShellRedirectionsReadable(t *testing.T) {
	withSetupRunner(t, installedServer(t, "ops@s1"))
	_, out, _ := runSetupCmd("-e", "p5", "--host", "ops@s1", "--network", "p5net", "--manifest-dir", t.TempDir(), "--json")
	if !strings.Contains(out, "> p5.key") || strings.Contains(out, "\x5cu003e") {
		t.Fatalf("output:\n%s", out)
	}
}
