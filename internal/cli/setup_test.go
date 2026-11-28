package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"berthy/deploy/systemd"
	"berthy/internal/agent"
	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/dialtest"
	"berthy/internal/keywrap"
	"berthy/internal/manifest"
	"berthy/internal/scope"
	"berthy/internal/setup"
)

var probedNetwork = regexp.MustCompile(`docker network inspect -f '[^']*' '([^']+)'`)

// setupServer is a Runner for a server that is already set up, or lacks Docker or the agent.
type setupServer struct {
	key       []byte
	keyAnswer func(context.Context, []byte) (setup.Result, error)
	probe     string
	config    string // what reading the agent's configuration returns
	scripts   []string
	stdins    [][]byte
	target    string
	waits     []int // exit codes of the restart-and-wait scripts
	// probes, when set, are the probe's answers in turn: the server changes as setup changes it. The last one repeats.
	probes     []string
	probeReads int
}

func (s *setupServer) Target() string { return s.target }

// nextProbe is the probe's answer: the next of probes, or probe when there are none.
func (s *setupServer) nextProbe() string {
	if len(s.probes) == 0 {
		return s.probe
	}
	i := min(s.probeReads, len(s.probes)-1)
	s.probeReads++
	return s.probes[i]
}

func (s *setupServer) Run(ctx context.Context, script string, stdin io.Reader) (setup.Result, error) {
	s.scripts = append(s.scripts, script)
	var in []byte
	if stdin != nil {
		in, _ = io.ReadAll(stdin)
	}
	s.stdins = append(s.stdins, in)
	switch {
	case strings.Contains(script, "# setup listen port"):
		return setup.Result{Stdout: []byte("free\n")}, nil
	case strings.Contains(script, "# key copy"):
		if s.keyAnswer != nil {
			return s.keyAnswer(ctx, in)
		}
		answer, err := keywrap.Wrap(in, s.key)
		return setup.Result{Stdout: answer}, err
	case strings.Contains(script, "uid=$(id -u)"):
		return setup.Result{Stdout: []byte("0\nroot\n")}, nil
	case strings.Contains(script, "# hosted path probe"):
		if strings.Contains(script, "s='/var/lib/berthy/cluster/state'") {
			if s.config == "" {
				return setup.Result{}, nil
			}
			return setup.Result{Stdout: []byte("state_dir_stat=berthy:berthy 700\nstate_db=yes\nkey_stat=32 berthy:berthy 600\nparent_0=berthy:berthy 700\n")}, nil
		}
		out := s.nextProbe()
		if strings.Contains(out, "state_dir_stat=") && !strings.Contains(out, "parent_0=") {
			out += "\nparent_0=root:berthy 750\nparent_1=root:berthy 750\nparent_2=berthy:berthy 700\n"
		}
		return setup.Result{Stdout: []byte(out)}, nil
	case strings.Contains(script, "uname -s"):
		out := s.nextProbe()
		if match := probedNetwork.FindStringSubmatch(script); len(match) == 2 {
			out = strings.Replace(out, "network=overlay swarm true\n", "network=overlay swarm true "+match[1]+"\n", 1)
		}
		return setup.Result{Stdout: []byte(out)}, nil
	case strings.Contains(script, "agent setup-probe doctor"):
		return setup.Result{Stdout: []byte(`{"checks":[{"name":"agent","ok":true,"critical":true,"detail":"v0.0.1-test"}]}`)}, nil
	case script == "cat /etc/berthy/agent.json":
		return setup.Result{Stdout: []byte(s.config)}, nil
	case strings.Contains(script, "cat > /usr/local/bin/berthy.new"):
		sum := sha256.Sum256(in)
		return setup.Result{Stdout: []byte(hex.EncodeToString(sum[:]) + "  /usr/local/bin/berthy.new\n")}, nil
	case strings.Contains(script, "sha256sum ") && strings.Contains(script, "/key"):
		sum := sha256.Sum256(in)
		return setup.Result{Stdout: []byte(hex.EncodeToString(sum[:]) + "  /var/lib/berthy/key\n")}, nil
	case strings.Contains(script, "systemctl restart berthy"):
		code := 0
		if len(s.waits) > 0 {
			code, s.waits = s.waits[0], s.waits[1:]
		}
		return setup.Result{Code: code}, nil
	case strings.HasPrefix(script, "# prepare setup probe"), strings.Contains(script, "agent setup-probe health"):
		return setup.Result{}, nil
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
	saved, savedDoctor, savedClock := setupBuild, setupDoctor, keyCopyNow
	t.Cleanup(func() { setupBuild, setupDoctor, keyCopyNow = saved, savedDoctor, savedClock })
	keyCopyNow = func() time.Time { return time.Date(2026, 11, 22, 15, 30, 12, 0, time.UTC) }
	setupBuild = func() setup.Build {
		return setup.Build{OS: "linux", Arch: "amd64", Path: exe, Version: "v0.0.1-test"}
	}
	setupDoctor = func(context.Context, string, string, string, string) ([]api.Check, error) {
		return []api.Check{{Name: "agent", OK: true, Critical: true, Detail: "v0.0.1-test"}}, nil
	}
	t.Setenv("BERTHY_SHOP_P5_TOKEN", "")
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
	cfg, err := json.MarshalIndent(agent.File{
		Listen: "172.31.250.10:7710", DockerSocket: "/var/run/docker.sock",
		Cluster:      agent.ClusterConfig{StateDir: "/var/lib/berthy/cluster/state", KeyFile: "/var/lib/berthy/cluster/key"},
		Environments: []agent.Hosted{{Project: "shop", Environment: "p5", StateDir: "/var/lib/berthy/environments/shop/p5/state", KeyFile: "/var/lib/berthy/environments/shop/p5/key", Network: "p5net", Tokens: []agent.Token{{Name: "admin", Digest: strings.Repeat("a", 64), Scope: "admin"}}}},
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
	saved, savedHidden := newSetupRunner, hiddenInput
	hiddenInput = func() (func(context.Context) ([]byte, error), bool) { return nil, false }
	t.Cleanup(func() { newSetupRunner, hiddenInput = saved, savedHidden })
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
	t.Setenv(agentclient.EnvironmentEnv, "")
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
		"bad network":            {"-e", "p5", "--project", "shop", "--host", "ops@s1", "--listen", "10.0.0.1:7710", "--network", "Bad Net"},
		"unknown flag":           {"-e", "p5", "--host", "ops@s1", "--listen", "10.0.0.1:7710", "--network", "n", "--force"},
		"positional":             {"-e", "p5", "--host", "ops@s1", "--listen", "10.0.0.1:7710", "--network", "n", "extra"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			code, out, errout := runSetupCmd(append([]string{"--manifest-dir", empty}, args...)...)
			if code != 2 || out != "" || errout == "" {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errout)
			}
			if name == "both host and local" && !strings.Contains(errout, setupUsage) {
				t.Fatalf("usage missing from stderr: %q", errout)
			}
		})
	}
	if made[0] != nil {
		t.Fatalf("a runner was made for a command that did not validate")
	}
}

// projectDir is a manifest directory of the project shop on the network p5net, the project that setup writes the CLI
// configuration under.
func projectDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	text := "project: shop\nnetwork: p5net\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
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
	server.config = strings.ReplaceAll(strings.ReplaceAll(server.config, "p5net", "demo"), `"project": "shop"`, `"project": "demo"`)
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

func TestSetupResolvesTheHostFromTheEnvironmentManifest(t *testing.T) {
	dir := projectDir(t)
	base := filepath.Join(dir, "berthy.yml")
	data, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, "servers:\n  pg:\n    ssh: ops@base\n"...)
	if err := os.WriteFile(base, data, 0o600); err != nil {
		t.Fatal(err)
	}
	baseOnly := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseOnly, "berthy.yml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	multi := t.TempDir()
	if err := os.WriteFile(filepath.Join(multi, "berthy.yml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(multi, "berthy.p5.yml"), []byte("servers:\n  api1:\n    ssh: ops@api1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for file, contents := range map[string]string{
		"berthy.p5.yml":      "servers:\n  pg:\n    ssh: ops@p5\n",
		"berthy.cleared.yml": "servers:\n  pg: null\n",
		"berthy.invalid.yml": "servers:\n  pg:\n    ssh: -oProxyCommand=x\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(agentclient.EnvironmentEnv, "p5")
	server := installedServer(t, "fixture")
	made := withSetupRunner(t, server)
	for _, tc := range []struct {
		name    string
		args    []string
		host    string
		local   bool
		problem string
		exit    int
	}{
		{name: "environment overlay", host: "ops@p5"},
		{name: "base", args: []string{"--manifest-dir", baseOnly}, host: "ops@base"},
		{name: "override", args: []string{"--host", "ops@replacement"}, host: "ops@replacement"},
		{name: "local", args: []string{"--local"}, local: true},
		{name: "explicitly empty host", args: []string{"--host="}, problem: "invalid --host"},
		{name: "cleared", args: []string{"-e", "cleared"}, problem: "a server in the manifest, or --local"},
		{name: "invalid", args: []string{"-e", "invalid"}, problem: "servers.pg.ssh", exit: 1},
		{name: "multiple", args: []string{"--manifest-dir", multi}, problem: "lists servers api1, pg: pass --server NAME"},
		{name: "named", args: []string{"--manifest-dir", multi, "--server", "api1"}, host: "ops@api1"},
		{name: "named base", args: []string{"--manifest-dir", multi, "--server", "pg"}, host: "ops@base"},
		{name: "unknown name", args: []string{"--manifest-dir", multi, "--server", "db"}, problem: "has no server \"db\": use one of api1, pg"},
		{name: "no servers named", args: []string{"-e", "cleared", "--server", "pg"}, problem: "lists no servers"},
		{name: "server and host", args: []string{"--server", "pg", "--host", "ops@x"}, problem: "only one of --host, --server and --local"},
		{name: "server and local", args: []string{"--server", "pg", "--local"}, problem: "only one of --host, --server and --local"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			*made = [2]any{}
			code, out, errout := runSetupCmd(append([]string{"--manifest-dir", dir, "--dry-run", "--json"}, tc.args...)...)
			if tc.problem != "" {
				want := 2
				if tc.exit != 0 {
					want = tc.exit
				}
				if code != want || !strings.Contains(errout, tc.problem) || made[0] != nil {
					t.Fatalf("exit %d, output %q, error %q, runner %v", code, out, errout, made)
				}
				return
			}
			if code != 0 || made[0] != tc.host || made[1] != tc.local {
				t.Fatalf("exit %d, output %q, error %q, runner %v", code, out, errout, made)
			}
		})
	}
}

func TestSetupRefusesAnInvalidManifest(t *testing.T) {
	dir := t.TempDir()
	// projectDir shape with bogus_knob on line 3, after network: and before apps:.
	text := "project: shop\nnetwork: p5net\nbogus_knob: true\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	made := withSetupRunner(t, &setupServer{})
	code, out, errout := runSetupCmd("--local", "--dry-run", "-e", "scratch", "--manifest-dir", dir)
	wantErr := "berthy.yml:3:1: bogus_knob: unknown field \"bogus_knob\"\n1 problem\n"
	if code != 1 || out != "" || errout != wantErr || made[0] != nil {
		t.Fatalf("exit %d, stdout %q, stderr %q, runner %v", code, out, errout, made)
	}
}

func TestSetupTextOutputOfASetUpServer(t *testing.T) {
	server := installedServer(t, "ops@s1")
	made := withSetupRunner(t, server)
	dir := projectDir(t)
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", dir)
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
		"✓ config      /etc/berthy/agent.json (listen 172.31.250.10:7710, 1 environment)\n" +
		"✓ service     berthy.service is running and enabled\n" +
		"✓ cli_config  wrote p5 at http://172.31.250.10:7710 to config.json (token in $BERTHY_SHOP_P5_TOKEN)\n" +
		"– cli_token   no admin token on this machine: set $BERTHY_SHOP_P5_TOKEN, or run setup with --new-token\n" +
		"✓ verify      the agent answers; run berthy doctor with an admin token to check it fully\n" +
		"– cli_key     not saved: this machine has no home folder; run setup again once that is fixed\n" +
		"setup of shop/p5 on ops@s1: done\n" +
		"\nnext steps:\n" +
		"  1. " + setupNextSteps(nextSteps{id: scope.ID{Project: "shop", Environment: "p5"}, config: "", local: false, tokenEnv: "BERTHY_SHOP_P5_TOKEN", storeToken: false, ingress: false, manifestDir: dir})[0] + "\n" +
		"  2. set the secrets the manifest names: berthy secret set NAME -e p5 --manifest-dir " + shellWord(dir) + "\n" +
		"  3. deploy every app in depends_on order, with --version APP=REF for each image app: berthy deploy -e p5 --manifest-dir " + shellWord(dir) + " --version APP=REF (or one app: berthy deploy APP -e p5 --manifest-dir " + shellWord(dir) + " --version REF)\n"
	if out != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out, want)
	}
	for _, sc := range server.scripts {
		command := strings.Replace(sc, "marker=$(mktemp)", "", 1)
		if strings.Contains(command, "mktemp") || strings.Contains(command, "systemctl restart") || strings.Contains(command, "chown") {
			t.Errorf("a set-up server was changed: %q", sc)
		}
	}
}

func TestSetupWithoutAStorePrintsTheTokenAsBefore(t *testing.T) {
	server := freshServer(t, "ops@s1")
	withSetupRunner(t, server)
	cfgPath := filepath.Join(t.TempDir(), "cli.json")
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", projectDir(t), "--config", cfgPath)
	if code != 0 || errout != "" {
		t.Fatalf("exit %d: %s\n%s", code, errout, out)
	}
	var token string
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "admin token for shop/p5 (shown once, store it now): "); ok {
			token = rest
		}
	}
	if !strings.HasPrefix(token, "bty.shop.p5.") || len(token) != len("bty.shop.p5.")+64 || strings.Count(out, token) != 1 {
		t.Fatalf("token = %q, output:\n%s", token, out)
	}
	for _, want := range []string{"✓ user        created the berthy user, added it to the docker group", "✓ token       created an admin token named admin",
		"✓ service     installed, enabled and started berthy.service", "✓ cli_config  wrote p5 at http://172.31.250.10:7710 to cli.json (token in $BERTHY_SHOP_P5_TOKEN)",
		"– cli_token   no credential store on this machine: the token is shown below; set it in $BERTHY_SHOP_P5_TOKEN",
		"2. store the admin token above, and set it in $BERTHY_SHOP_P5_TOKEN when you run berthy",
		"3. set the secrets the manifest names: berthy secret set NAME -e p5 --config " + shellWord(cfgPath),
		"4. deploy every app in depends_on order, with --version APP=REF for each image app: berthy deploy -e p5 --config " + shellWord(cfgPath)} {
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
	if err != nil || bytes.Contains(data, []byte(token)) || !bytes.Contains(data, []byte(`"token_env": "BERTHY_SHOP_P5_TOKEN"`)) {
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
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", projectDir(t))
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
	if err != nil || c.Projects["shop"].Environments["p5"] != (agentclient.Environment{URL: "http://172.31.250.10:7710", TokenEnv: "BERTHY_SHOP_P5_TOKEN"}) {
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
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", projectDir(t))
	if code != 0 || errout != "" || !strings.Contains(out, "– cli_config  add {") || !strings.Contains(out, "give --config or set BERTHY_CONFIG") {
		t.Fatalf("exit %d: %s\n%s", code, errout, out)
	}
}

func TestSetupJSONHasTheTokenAndTheNextSteps(t *testing.T) {
	server := freshServer(t, "ops@s1")
	withSetupRunner(t, server)
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", projectDir(t), "--json")
	if code != 0 || errout != "" {
		t.Fatalf("exit %d: %s\n%s", code, errout, out)
	}
	var got setupReport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if !got.OK || !strings.HasPrefix(got.Token, "bty.shop.p5.") || strings.Count(out, got.Token) != 1 || len(got.NextSteps) != 4 || len(got.Steps) != 15 {
		t.Fatalf("report = %+v", got)
	}
	if !strings.Contains(got.NextSteps[0], "run the same berthy setup command again") {
		t.Errorf("next step = %q", got.NextSteps[0])
	}
	steps := strings.Join(got.NextSteps, "\n")
	if !strings.Contains(steps, "berthy secret set NAME -e p5") || !strings.Contains(steps, "berthy deploy APP -e p5 --manifest-dir") || strings.Contains(steps, " --project ") {
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
	_, out, _ = runSetupCmd("-e", "p5", "--host", "ops@s1", "--network", "p5net", "--manifest-dir", projectDir(t), "--json")
	if strings.Contains(out, `"token": `) || !strings.Contains(out, `"next_steps"`) {
		t.Fatalf("second report:\n%s", out)
	}
}

func TestSetupNextStepsMentionTheIngressOnlyWhenTheManifestHasOne(t *testing.T) {
	steps := setupNextSteps(nextSteps{id: scope.ID{Project: "shop", Environment: "prod"}, config: "", local: false, tokenEnv: "BERTHY_PROD_TOKEN", storeToken: true, ingress: true, manifestDir: ""})
	if len(steps) != 5 || !strings.Contains(steps[3], "berthy ingress apply -e prod") || strings.Contains(steps[3], "--config") {
		t.Fatalf("steps = %q", steps)
	}
	local := setupNextSteps(nextSteps{id: scope.ID{Project: "shop", Environment: "prod"}, config: "", local: true, tokenEnv: "X", storeToken: false, ingress: false, manifestDir: ""})
	if len(local) != 3 || !strings.Contains(local[0], "interrupted key rotation") {
		t.Fatalf("local steps = %q", local)
	}
}

func TestSetupNextStepsNameTheConfigGivenToSetup(t *testing.T) {
	steps := setupNextSteps(nextSteps{id: scope.ID{Project: "shop", Environment: "prod"}, config: "/etc/berthy/cli.json", local: false, tokenEnv: "BERTHY_PROD_TOKEN", storeToken: false, ingress: false, manifestDir: ""})
	want := []string{
		"berthy secret set NAME -e prod --project shop --config /etc/berthy/cli.json",
		"berthy deploy APP -e prod --project shop --config /etc/berthy/cli.json",
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
		steps := setupNextSteps(nextSteps{id: scope.ID{Project: "shop", Environment: "prod"}, config: cfg, local: false, tokenEnv: "BERTHY_PROD_TOKEN", storeToken: false, ingress: true, manifestDir: ""})
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
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--network", "p5net", "--manifest-dir", projectDir(t))
	if code != 0 || !strings.Contains(out, "172.31.250.10 assigned") {
		t.Fatalf("exit %d: %s\n%s", code, errout, out)
	}
	// On a first run it is needed, and the step that finds out says so.
	fresh := freshServer(t, "ops@s1")
	withSetupRunner(t, fresh)
	code, out, _ = runSetupCmd("-e", "p5", "--host", "ops@s1", "--network", "p5net", "--manifest-dir", projectDir(t))
	if code != 1 || !strings.Contains(out, "✖ check") || !strings.Contains(out, "--listen ADDRESS:PORT is needed") || !strings.HasSuffix(out, "failed at check\n") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	// Another address than the agent's is refused.
	other := installedServer(t, "ops@s1")
	other.probe += "addr=172.31.250.11\n"
	withSetupRunner(t, other)
	code, out, _ = runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.11:7710", "--network", "p5net", "--manifest-dir", projectDir(t))
	if code != 1 || !strings.Contains(out, "the agent listens on 172.31.250.10:7710; setup does not move it") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestSetupDefaultsListenFromTheManifestAddress(t *testing.T) {
	dir := projectDir(t)
	if err := os.WriteFile(filepath.Join(dir, "berthy.p5.yml"), []byte("servers:\n  pg:\n    ssh: ops@s1\n    address: 172.31.250.10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := freshServer(t, "ops@s1")
	withSetupRunner(t, server)
	code, out, errout := runSetupCmd("-e", "p5", "--manifest-dir", dir, "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, errout, out)
	}
	var got setupReport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	var check setup.StepResult
	for _, s := range got.Steps {
		if s.Step == "check" {
			check = s
		}
	}
	if check.Status != "ok" || !strings.Contains(check.Detail, "172.31.250.10 assigned") {
		t.Fatalf("check = %+v\nreport = %s", check, out)
	}
	// --host and --local do not take the manifest address: a first install still needs --listen.
	needListen := "--listen ADDRESS:PORT is needed: this server has no agent configuration yet and the manifest gives no address for it"
	withSetupRunner(t, freshServer(t, "ops@s1"))
	code, out, _ = runSetupCmd("-e", "p5", "--host", "ops@s1", "--manifest-dir", dir, "--dry-run", "--json")
	if code != 1 || !strings.Contains(out, needListen) {
		t.Fatalf("--host: exit %d:\n%s", code, out)
	}
	withSetupRunner(t, freshServer(t, "this server"))
	code, out, _ = runSetupCmd("-e", "p5", "--local", "--manifest-dir", dir, "--dry-run", "--json")
	if code != 1 || !strings.Contains(out, needListen) {
		t.Fatalf("--local: exit %d:\n%s", code, out)
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
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", projectDir(t),
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

// TestSetupPrintsTheBannerBeforeTheFirstStep: on a terminal, setup prints the banner on stderr before connect on stdout,
// and the runner has run no script yet when that line appears. Under --json the banner is alone on stderr and stdout is
// one report with no extra bytes.
func TestSetupPrintsTheBannerBeforeTheFirstStep(t *testing.T) {
	server := installedServer(t, "ops@s1")
	withSetupRunner(t, server)
	forceStderrTerminal(t, true)
	var out, errout bytes.Buffer
	var order []string
	scriptsAtBanner := -1
	outW := writeCatch{Writer: &out, onWrite: func(p []byte) {
		if strings.Contains(string(p), "connect") {
			order = append(order, "connect")
		}
	}}
	errW := writeCatch{Writer: &errout, onWrite: func(p []byte) {
		if strings.Contains(string(p), "berthy: shop/p5 on ops@s1") {
			order = append(order, "banner")
			scriptsAtBanner = len(server.scripts)
		}
	}}
	code := Run([]string{"setup", "-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net",
		"--manifest-dir", projectDir(t), "--dry-run"}, outW, errW)
	if code != 0 {
		t.Fatalf("exit %d: out=%q err=%q", code, out.String(), errout.String())
	}
	if errout.String() != "berthy: shop/p5 on ops@s1\n" {
		t.Fatalf("stderr %q", errout.String())
	}
	if !strings.Contains(out.String(), "connect") {
		t.Fatalf("stdout lacks connect:\n%s", out.String())
	}
	if len(order) < 2 || order[0] != "banner" || order[1] != "connect" {
		t.Fatalf("write order %v, want banner then connect", order)
	}
	if scriptsAtBanner != 0 {
		t.Fatalf("runner had run %d scripts when the banner printed", scriptsAtBanner)
	}

	server = installedServer(t, "ops@s1")
	withSetupRunner(t, server)
	forceStderrTerminal(t, true)
	out.Reset()
	errout.Reset()
	code = Run([]string{"setup", "-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net",
		"--manifest-dir", projectDir(t), "--dry-run", "--json"}, &out, &errout)
	if code != 0 {
		t.Fatalf("json exit %d: %s", code, errout.String())
	}
	if errout.String() != "berthy: shop/p5 on ops@s1\n" {
		t.Fatalf("json stderr %q", errout.String())
	}
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	var got setupReport
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("stdout JSON: %v (%q)", err, out.String())
	}
	if trailing := bytes.TrimSpace(out.Bytes()[dec.InputOffset():]); len(trailing) != 0 {
		t.Fatalf("extra bytes after JSON report: %q", trailing)
	}
	if !got.OK || !got.DryRun || got.Project != "shop" || got.Environment != "p5" || got.Target != "ops@s1" {
		t.Fatalf("report = %+v", got)
	}
}

// TestSetupPrintsNoBannerWhenStderrIsNotATerminal: redirected stderr gets no banner from setup.
func TestSetupPrintsNoBannerWhenStderrIsNotATerminal(t *testing.T) {
	withSetupRunner(t, installedServer(t, "ops@s1"))
	forceStderrTerminal(t, false)
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net",
		"--manifest-dir", projectDir(t), "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errout)
	}
	if strings.Contains(errout, "berthy:") || strings.Contains(out, "berthy:") {
		t.Fatalf("banner appeared without a terminal: out=%q err=%q", out, errout)
	}
}

func TestSetupDryRunOutput(t *testing.T) {
	server := freshServer(t, "ops@s1")
	server.probe = freshProbe
	withSetupRunner(t, server)
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", projectDir(t), "--dry-run")
	if code != 0 || errout != "" {
		t.Fatalf("exit %d: %s", code, errout)
	}
	for _, want := range []string{"· docker      would install Docker with the official script", "· swarm       would turn on Swarm advertising 172.31.250.10",
		"· network     would create network p5net", "· token       would create an admin token", "· cli_config  would write p5 to config.json", "– verify      would check the agent"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "dry run, 11 steps would change\n") || strings.Contains(out, "next steps") || strings.Contains(out, "bty.") {
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
	code, out, _ := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", projectDir(t), "--dry-run")
	// The agent is set up, but this machine has no CLI configuration yet, so the local step would change.
	if code != 0 || !strings.Contains(out, "· cli_config  would write p5 to config.json\n") || !strings.HasSuffix(out, "setup of shop/p5 on ops@s1: dry run, 1 step would change\n") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestSetupFailureNamesTheStep(t *testing.T) {
	server := installedServer(t, "ops@s1")
	server.probe = strings.Replace(server.probe, "systemd=yes", "systemd=", 1)
	withSetupRunner(t, server)
	code, out, _ := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", projectDir(t))
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	want := "✓ connect     ops@s1\n✖ check       systemd is not running on this server\nsetup of shop/p5 on ops@s1: failed at check\n"
	if out != want {
		t.Fatalf("output:\n%s", out)
	}
}

func TestSetupAFailedFirstInstallStillShowsTheToken(t *testing.T) {
	server := freshServer(t, "ops@s1")
	server.waits = []int{1}
	withSetupRunner(t, server)
	code, out, _ := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", projectDir(t))
	if code != 1 || !strings.Contains(out, "✖ service") || !strings.Contains(out, "failed at service") || !strings.Contains(out, "admin token for shop/p5 (shown once, store it now): bty.shop.p5.") || strings.Contains(out, "next steps") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestSetupJSON(t *testing.T) {
	server := freshServer(t, "ops@s1")
	server.probe = freshProbe
	withSetupRunner(t, server)
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", projectDir(t), "--dry-run", "--json")
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
	if got.Environment != "p5" || got.Target != "ops@s1" || !got.DryRun || !got.OK || len(got.Steps) != 15 || got.Token != "" {
		t.Fatalf("report = %+v", got)
	}
	if s := got.Steps[2]; s.Step != "docker" || s.Status != "would_change" || s.Detail == "" {
		t.Fatalf("docker step = %+v", s)
	}
	if s := got.Steps[13]; s.Step != "verify" || s.Status != "skipped" {
		t.Fatalf("verify step = %+v", s)
	}
}

func TestSetupJSONOfAFailure(t *testing.T) {
	withSetupRunner(t, &setupServer{probe: "os=Darwin\n", target: "ops@s1"})
	code, out, _ := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", projectDir(t), "--json")
	var got setupReport
	if code != 1 || json.Unmarshal([]byte(out), &got) != nil || got.OK || got.Steps[len(got.Steps)-1].Status != "failed" || got.Steps[len(got.Steps)-1].Step != "check" {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if strings.Contains(out, "setup of") {
		t.Fatalf("the summary line is in the JSON output:\n%s", out)
	}
}

func TestSetupWithoutARunnerFailsTheConnectStep(t *testing.T) {
	isolatedUserConfig(t)
	saved := newSetupRunner
	t.Cleanup(func() { newSetupRunner = saved })
	newSetupRunner = func(string, bool) (setup.Runner, error) { return nil, errors.New("ssh was not found on PATH") }
	code, out, _ := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", projectDir(t))
	want := "✖ connect     ssh was not found on PATH\nsetup of shop/p5 on ops@s1: failed at connect\n"
	if code != 1 || out != want {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestSetupLocalTargetsThisServer(t *testing.T) {
	made := withSetupRunner(t, installedServer(t, "this server"))
	code, out, _ := runSetupCmd("-e", "p5", "--local", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", projectDir(t))
	if code != 0 || made[1] != true || !strings.Contains(out, "✓ connect     this server\n") || !strings.Contains(out, "setup of shop/p5 on this server: done\n") ||
		!strings.Contains(out, "interrupted key rotation") || strings.Contains(out, "sudo cat") {
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
	for _, flag := range []string{"--binary", "--key-file", "--new-token", "--config", "[--listen ADDRESS:PORT]", "--verbose"} {
		if !strings.Contains(setupUsage, flag) {
			t.Errorf("the usage lacks %s", flag)
		}
	}
}

func TestSetupJSONContainsNoRawKeyCommand(t *testing.T) {
	withSetupRunner(t, installedServer(t, "ops@s1"))
	_, out, _ := runSetupCmd("-e", "p5", "--host", "ops@s1", "--network", "p5net", "--manifest-dir", projectDir(t), "--json")
	if strings.Contains(out, "sudo cat") || !strings.Contains(out, "run the same berthy setup command again") {
		t.Fatal("incorrect fallback")
	}
}

func TestSetupRefusesAPairOverTheLengthBoundBeforeAnyServerStep(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(longPairManifest), 0o600)
	made := withSetupRunner(t, &setupServer{})
	code, out, errout := runSetupCmd("-e", strings.Repeat("e", 30), "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--manifest-dir", dir)
	if code != 1 || out != "" || !strings.Contains(errout, longPairMessage) || made[0] != nil {
		t.Fatalf("exit %d, runner made %v: %q %q", code, made[0] != nil, out, errout)
	}
}

func TestSetupWithAManifestWithoutANetworkUsesTheDefault(t *testing.T) {
	dir := projectDir(t)
	p := filepath.Join(dir, "berthy.yml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), "network: p5net\n", "", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(dir, "prod")
	if err != nil {
		t.Fatal(err)
	}
	got, err := setupSettings(dir, "prod", "", "", false, m)
	if err != nil || got.network != "shop-prod" {
		t.Fatalf("settings %+v: %v", got, err)
	}
	server := installedServer(t, "ops@s1")
	server.config = strings.ReplaceAll(server.config, "p5net", "shop-p5")
	withSetupRunner(t, server)
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--manifest-dir", dir, "--json")
	if code != 0 || !strings.Contains(out, "shop-p5 (attachable overlay)") {
		t.Fatalf("exit %d: %s %s", code, out, errout)
	}
	if _, err := setupSettings(dir, "prod", "", "other", false, m); err == nil {
		t.Fatal("accepted a different network flag")
	}
}

func TestSetupWithoutAManifestNeedsAProject(t *testing.T) {
	dir := t.TempDir()
	_, err := setupSettings(dir, "prod", "", "", false, nil)
	if err == nil || err.Error() != "there is no berthy.yml in "+dir+": pass --project NAME" {
		t.Fatalf("error: %v", err)
	}
}

func TestSetupNeedsAProjectWithoutAManifest(t *testing.T) {
	made := withSetupRunner(t, &setupServer{})
	code, _, errout := runSetupCmd("-e", "prod", "--host", "ops@s1", "--manifest-dir", t.TempDir(), "--network", "test")
	if code != 2 || !strings.Contains(errout, "--project") || made[0] != nil {
		t.Fatalf("exit %d, runner %v, stderr %q", code, made[0], errout)
	}
}

func TestSetupProjectMustAgreeWithTheManifest(t *testing.T) {
	made := withSetupRunner(t, &setupServer{})
	code, _, errout := runSetupCmd("-e", "prod", "--project", "other", "--host", "ops@s1", "--manifest-dir", projectDir(t))
	if code != 2 || !strings.Contains(errout, "--project other differs from the project shop") || made[0] != nil {
		t.Fatalf("exit %d, runner %v, stderr %q", code, made[0], errout)
	}
}

func TestSetupRefusesALongPairWithoutAManifestBeforeAnyServerStep(t *testing.T) {
	made := withSetupRunner(t, &setupServer{})
	code, _, errout := runSetupCmd("-e", strings.Repeat("e", 30), "--project", strings.Repeat("p", 30), "--host", "ops@s1", "--manifest-dir", t.TempDir())
	if code != 2 || !strings.Contains(errout, longPairMessage) || made[0] != nil {
		t.Fatalf("exit %d, runner %v, stderr %q", code, made[0], errout)
	}
}

func TestSetupWithoutAManifestDefaultsTheNetwork(t *testing.T) {
	server := &setupServer{target: "ops@s1"}
	withSetupRunner(t, server)
	runSetupCmd("-e", "prod", "--project", "shop", "--host", "ops@s1", "--manifest-dir", t.TempDir())
	if !server.ran("shop-prod") {
		t.Fatalf("network not probed: %v", server.scripts)
	}
}

func TestSetupNextStepsDoNotOfferRawKey(t *testing.T) {
	steps := setupNextSteps(nextSteps{id: scope.ID{Project: "shop", Environment: "prod"}})
	if strings.Contains(strings.Join(steps, "\n"), "sudo cat") {
		t.Fatal("raw copy offered")
	}
}

func TestTheKeyCopyNamesAFileNoOtherEnvironmentCanShare(t *testing.T) {
	ids := []scope.ID{{Project: "cardiotennis", Environment: "prod"}, {Project: "sliceup", Environment: "prod"}, {Project: "shop", Environment: "prod-eu"}, {Project: "shop-prod", Environment: "eu"}}
	seen := map[string]bool{}
	for _, id := range ids {
		name := id.Slug() + ".key"
		if seen[name] {
			t.Fatalf("duplicate name %s", name)
		}
		seen[name] = true
		var out, errout bytes.Buffer
		if printKeyRotation(&out, &errout, []byte(`{"old_key_id":"old","new_key_id":"new","key_file":"/key"}`), id) != 0 || !strings.Contains(out.String(), "> ~/"+name+".") {
			t.Errorf("rotation for %v: %s %s", id, out.String(), errout.String())
		}
	}
}

func TestSetupReportNamesTheProject(t *testing.T) {
	server := freshServer(t, "ops@s1")
	withSetupRunner(t, server)
	code, out, errout := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--manifest-dir", projectDir(t))
	if code != 0 || !strings.Contains(out, "setup of shop/p5 on ops@s1: done") || !strings.Contains(out, "admin token for shop/p5") {
		t.Fatalf("exit %d: %s %s", code, out, errout)
	}
	withSetupRunner(t, freshServer(t, "ops@s1"))
	code, out, errout = runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--manifest-dir", projectDir(t), "--json")
	if code != 0 || !strings.Contains(out, `"project": "shop"`) {
		t.Fatalf("exit %d: %s %s", code, out, errout)
	}
}

func TestSetupNextStepsDoNotRevealExistingKeyPath(t *testing.T) {
	server := installedServer(t, "ops@s1")
	server.config = strings.Replace(server.config, "/var/lib/berthy/environments/shop/p5/key", "/srv/keys/my key", 1)
	withSetupRunner(t, server)
	code, out, _ := runSetupCmd("-e", "p5", "--host", "ops@s1", "--manifest-dir", projectDir(t))
	if code != 0 || strings.Contains(out, "/srv/keys/my key") || strings.Contains(out, "sudo cat") {
		t.Fatal("unsafe fallback")
	}
}

func TestSetupRefusesTokenVariableUsedByAnotherProjectBeforeChanges(t *testing.T) {
	server := freshServer(t, "ops@s1")
	withSetupRunner(t, server)
	cfg := filepath.Join(t.TempDir(), "cli.json")
	original := `{"projects":{"my-shop":{"environments":{"prod":{"url":"http://127.0.0.1:7710","token_env":"BERTHY_MY_SHOP_PROD_TOKEN"}}}}}`
	if err := os.WriteFile(cfg, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errout := runSetupCmd("-e", "prod", "--project", "my_shop", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--manifest-dir", t.TempDir(), "--config", cfg)
	if code == 0 || !strings.Contains(out+errout, "my-shop/prod") || !strings.Contains(out+errout, "my_shop/prod") || !strings.Contains(out+errout, "--token-env") {
		t.Fatalf("exit %d: %s %s", code, out, errout)
	}
	for _, script := range server.scripts {
		if strings.Contains(script, "install -d") || strings.Contains(script, "useradd") || strings.Contains(script, "cat >") || strings.Contains(script, "network create") {
			t.Fatalf("server changed: %s", script)
		}
	}
	b, _ := os.ReadFile(cfg)
	if string(b) != original {
		t.Fatal("CLI config changed")
	}
}

func TestSetupPreflightFailuresAreReportedAsJSONWithoutRunner(t *testing.T) {
	for _, tc := range []struct{ name, content, want string }{
		{"collision", `{"projects":{"my-shop":{"environments":{"prod":{"url":"http://127.0.0.1:7710","token_env":"BERTHY_MY_SHOP_PROD_TOKEN"}}}}}`, "my-shop/prod"},
		{"earlier layout", `{"environments":{"prod":{"url":"http://127.0.0.1:7710","token_env":"OLD"}}}`, "move it aside, or give --config with a new file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := freshServer(t, "ops@s1")
			made := withSetupRunner(t, server)
			cfg := filepath.Join(t.TempDir(), "cli.json")
			if err := os.WriteFile(cfg, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			code, out, errout := runSetupCmd("-e", "prod", "--project", "my_shop", "--host", "ops@s1", "--manifest-dir", t.TempDir(), "--config", cfg, "--json")
			var report setupReport
			err := json.Unmarshal([]byte(out), &report)
			if code != 1 || err != nil || report.OK || len(report.Steps) != 1 || report.Steps[0].Status != setup.StatusFailed || !strings.Contains(report.Steps[0].Detail, tc.want) || made[0] != nil || errout != "" {
				t.Fatalf("code %d report %+v runner %v stdout %q stderr %q parse %v", code, report, made[0], out, errout, err)
			}
		})
	}
}

type setupTokenOutput struct {
	bytes.Buffer
	seen atomic.Bool
}

func (w *setupTokenOutput) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("admin token for ")) {
		w.seen.Store(true)
	}
	return w.Buffer.Write(p)
}

func TestSetupAtATerminalRunsTheSecretsGuide(t *testing.T) {
	server := freshServer(t, "ops@s1")
	server.probe = strings.ReplaceAll(server.probe, "172.31.250.10", "127.0.0.1")
	withSetupRunner(t, server)
	reads := fakeTerminal(t, "private")
	var paths []string
	var output setupTokenOutput
	var requestBeforeToken atomic.Bool
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !output.seen.Load() {
			requestBeforeToken.Store(true)
		}
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/secrets/generate-declared"):
			fmt.Fprint(w, declaredReply)
		case r.Method == "GET":
			fmt.Fprint(w, `[{"name":"DB_PASS"},{"name":"GEN_PASS"}]`)
		default:
			fmt.Fprint(w, `{"ok":true}`)
		}
	}))
	defer agent.Close()
	dir := declaredManifestDir(t)
	b, err := os.ReadFile(filepath.Join(dir, "berthy.yml"))
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.ReplaceAll(b, []byte("project: demo"), []byte("project: shop"))
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), b, 0600); err != nil {
		t.Fatal(err)
	}
	code := Run([]string{"setup", "-e", "p5", "--host", "ops@s1", "--listen", strings.TrimPrefix(agent.URL, "http://"), "--manifest-dir", dir, "--config", filepath.Join(t.TempDir(), "config.json")}, &output, &output)
	text := output.String()
	token, guide, prompt := strings.Index(text, "admin token for"), strings.Index(text, "secrets:"), strings.Index(text, "Value of OTHER")
	if code != 0 || reads.Load() != 1 || len(paths) != 3 || requestBeforeToken.Load() || token < 0 || guide <= token || prompt <= guide || !strings.Contains(text, "generated GEN_PASS") || !strings.Contains(text, "stored OTHER") {
		t.Fatalf("exit %d reads %d paths %v output %q", code, reads.Load(), paths, text)
	}
	for _, step := range []string{"set the secrets the manifest names:", "set the secrets still missing (", "generate the secrets the manifest declares:"} {
		if strings.Contains(text, step) {
			t.Errorf("unexpected secret step %q in output %q", step, text)
		}
	}
}

func TestSetupDryRunAtATerminalDoesNotRunTheSecretsGuide(t *testing.T) {
	var requests atomic.Int32
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}))
	defer agent.Close()
	addr := strings.TrimPrefix(agent.URL, "http://")
	server := installedServer(t, "ops@s1")
	server.probe = strings.ReplaceAll(server.probe, "172.31.250.10", "127.0.0.1")
	server.config = strings.ReplaceAll(server.config, "172.31.250.10:7710", addr)
	withSetupRunner(t, server)
	t.Setenv("BERTHY_TOKEN_STORE", "none")
	t.Setenv("BERTHY_SHOP_P5_TOKEN", "synthetic-admin-token")
	reads := fakeTerminal(t, "unused")
	dir := declaredManifestDir(t)
	path := filepath.Join(dir, "berthy.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.ReplaceAll(b, []byte("project: demo"), []byte("project: shop"))
	b = bytes.ReplaceAll(b, []byte("network: overlay"), []byte("network: p5net"))
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", addr, "--manifest-dir", dir, "--config", filepath.Join(t.TempDir(), "config.json"), "--dry-run")
	if code != 0 || requests.Load() != 0 || reads.Load() != 0 || !strings.Contains(out, "the admin token is in $BERTHY_SHOP_P5_TOKEN") || strings.Contains(out, "\nsecrets:") {
		t.Fatalf("exit %d requests %d reads %d stdout %q stderr %q", code, requests.Load(), reads.Load(), out, stderr)
	}
}

func TestSetupGuideOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name              string
		terminal, json    bool
		status            int
		body, input, want string
		reads, calls      int
	}{
		{"skipped", true, false, 200, "", "", "set the secrets still missing (OTHER):", 1, 2},
		{"failed", true, false, 400, `{"message":"refused"}`, "", "generate the secrets the manifest declares:", 0, 1},
		{"held", true, false, 202, `{"approval_id":"ap1","expires":"2030-01-01T00:00:00Z","plan":{"action":"secret generate declared"}}`, "", "generate the secrets the manifest declares:", 0, 1},
		{"no terminal", false, false, 200, "", "", "generate the secrets the manifest declares:", 0, 0},
		{"json", true, true, 200, "", "", "generate the secrets the manifest declares:", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := freshServer(t, "ops@s1")
			server.probe = strings.ReplaceAll(server.probe, "172.31.250.10", "127.0.0.1")
			withSetupRunner(t, server)
			reads := fakeTerminal(t, tc.input)
			if !tc.terminal {
				hiddenInput = func() (func(context.Context) ([]byte, error), bool) { return nil, false }
			}
			calls := 0
			agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if tc.status != 200 {
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
					return
				}
				if r.Method == "GET" {
					fmt.Fprint(w, `[{"name":"DB_PASS"},{"name":"GEN_PASS"}]`)
				} else {
					fmt.Fprint(w, declaredReply)
				}
			}))
			defer agent.Close()
			dir := declaredManifestDir(t)
			path := filepath.Join(dir, "berthy.yml")
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, bytes.ReplaceAll(b, []byte("project: demo"), []byte("project: shop")), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"-e", "p5", "--host", "ops@s1", "--listen", strings.TrimPrefix(agent.URL, "http://"), "--manifest-dir", dir, "--config", filepath.Join(t.TempDir(), "config.json")}
			if tc.json {
				args = append(args, "--json")
			}
			code, out, stderr := runSetupCmd(args...)
			if code != 0 || reads.Load() != int32(tc.reads) || calls != tc.calls || !strings.Contains(out, tc.want) {
				t.Fatalf("exit %d reads %d calls %d stdout %q stderr %q", code, reads.Load(), calls, out, stderr)
			}
			if tc.name == "failed" || tc.name == "held" {
				if !strings.Contains(out, "set the secrets the manifest names:") {
					t.Fatal(out)
				}
			}
		})
	}
}

func TestSetupThatCannotReachTheAgentKeepsTheOldNextSteps(t *testing.T) {
	addr := strings.TrimPrefix(dialtest.HangUp(t), "http://")
	server := freshServer(t, "ops@s1")
	server.probe = strings.ReplaceAll(server.probe, "172.31.250.10", "127.0.0.1")
	withSetupRunner(t, server)
	reads := fakeTerminal(t, "unused")
	code, out, stderr := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", addr, "--manifest-dir", projectDir(t), "--config", filepath.Join(t.TempDir(), "config.json"))
	if code != 0 || reads.Load() != 0 || strings.Count(stderr, "agent connection failed at "+addr+": connection failed") != 1 || !strings.Contains(out, "set the secrets the manifest names:") {
		t.Fatalf("exit %d reads %d stderr %q", code, reads.Load(), stderr)
	}
}
