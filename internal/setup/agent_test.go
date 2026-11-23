package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"berthy/deploy/systemd"
	"berthy/internal/agent"
	"berthy/internal/agentclient"
	"berthy/internal/api"
)

// call is one script the fake server received, with what arrived on its standard input.
type call struct {
	script string
	stdin  []byte
}

// agentFake is a server for the agent's steps. It answers the scripts of setup by what they contain.
type agentFake struct {
	probe   []string
	config  string // what reading the agent's configuration returns
	version string // what the installed binary's version command prints

	prevBin, prevCfg bool  // what the swap finds already installed
	waits            []int // exit codes of the successive restart-and-wait scripts; the rest are 0
	journal          string
	corrupt          bool // the upload arrives with another hash
	healthCode       int  // exit code of a health check alone
	doctorCode       int
	doctorBody       string

	calls    []call
	restarts int
}

func (f *agentFake) Target() string { return "ops@s1" }

func (f *agentFake) Run(ctx context.Context, script string, stdin io.Reader) (Result, error) {
	var in []byte
	if stdin != nil {
		in, _ = io.ReadAll(stdin)
	}
	f.calls = append(f.calls, call{script, in})
	hash := func(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
	switch {
	case script == "id -u; id -un":
		return Result{Stdout: []byte("0\nroot\n")}, nil
	case strings.Contains(script, "# hosted path probe"):
		out := strings.Join(f.probe, "\n")
		if strings.Contains(out, "state_dir_stat=") && !strings.Contains(out, "parent_0=") {
			out += "\nparent_0=root:berthy 750\nparent_1=root:berthy 750\nparent_2=berthy:berthy 700"
		}
		return Result{Stdout: []byte(out + "\n")}, nil
	case strings.Contains(script, "uname -s"):
		return Result{Stdout: []byte(strings.Join(f.probe, "\n") + "\n")}, nil
	case script == "cat "+configPath:
		return Result{Stdout: []byte(f.config)}, nil
	case script == binPath+" version 2>/dev/null":
		return Result{Stdout: []byte(f.version)}, nil
	case strings.Contains(script, "cat > "+binPath+".new"):
		sum := hash(in)
		if f.corrupt {
			sum = strings.Repeat("0", 64)
		}
		return Result{Stdout: []byte(sum + "  " + binPath + ".new\n")}, nil
	case strings.Contains(script, "sha256sum ") && strings.Contains(script, keyPath):
		return Result{Stdout: []byte(hash(in) + "  " + keyPath + "\n")}, nil
	case strings.Contains(script, "mv -f "+binPath+".new "+binPath) || strings.Contains(script, "mv -f "+configPath+".new "+configPath):
		var out string
		if f.prevBin && strings.Contains(script, binPath+".new") {
			out += "prev_bin=yes\n"
		}
		if f.prevCfg && strings.Contains(script, configPath+".new") {
			out += "prev_cfg=yes\n"
		}
		return Result{Stdout: []byte(out)}, nil
	case strings.Contains(script, "systemctl restart berthy"):
		f.restarts++
		code := 0
		if len(f.waits) > 0 {
			code, f.waits = f.waits[0], f.waits[1:]
		}
		return Result{Code: code}, nil
	case strings.HasPrefix(script, "journalctl"):
		return Result{Stdout: []byte(f.journal)}, nil
	case strings.Contains(script, "-H @-"):
		return Result{Stdout: []byte(f.doctorBody), Code: f.doctorCode}, nil
	case strings.Contains(script, "systemctl is-failed"):
		return Result{Code: f.healthCode}, nil
	}
	return Result{}, nil
}

func (f *agentFake) scripts() []string {
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.script
	}
	return out
}

// find returns the first call whose script contains marker.
func (f *agentFake) find(marker string) (call, bool) {
	for _, c := range f.calls {
		if strings.Contains(c.script, marker) {
			return c, true
		}
	}
	return call{}, false
}

func (f *agentFake) ran(marker string) bool { _, ok := f.find(marker); return ok }

// elfFile writes the header of an executable ELF file for machine and returns its path.
func elfFile(t *testing.T, machine uint16) string {
	t.Helper()
	var b bytes.Buffer
	b.Write([]byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	for _, v := range []any{uint16(2), machine, uint32(1), uint64(0x400000), uint64(0), uint64(0), uint32(0),
		uint16(64), uint16(56), uint16(0), uint16(64), uint16(0), uint16(0)} {
		if err := binary.Write(&b, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	b.WriteString("agent body " + t.Name())
	path := filepath.Join(t.TempDir(), "berthy")
	if err := os.WriteFile(path, b.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func fileHash(t *testing.T, path string) string {
	t.Helper()
	sum, err := fileSHA(path)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

const (
	stateDir = "/var/lib/berthy/environments/shop/p5/state"
	keyPath  = "/var/lib/berthy/environments/shop/p5/key"
	emX86    = 62
	emArm    = 183
)

var fixedNow = time.Date(2026, 11, 16, 12, 30, 45, 0, time.UTC)

func okChecks() []api.Check {
	return []api.Check{
		{Name: "agent", OK: true, Critical: true, Detail: "v0.0.1-test"},
		{Name: "docker", OK: true, Critical: true, Detail: "29.9.3"},
		{Name: "network", OK: true, Critical: true, Detail: "p5net"},
	}
}

// existingConfig is the configuration of a set-up server.
func existingConfig(t *testing.T, mod func(*agent.Config)) (string, agent.Config) {
	t.Helper()
	c := agent.Config{
		Environment: "p5", Listen: listenIP + ":7710", DockerSocket: dockerSocket, StateDir: stateDir, KeyFile: keyPath,
		Network: "p5net", Tokens: []agent.Token{{Name: "admin", Digest: strings.Repeat("a", 64), Scope: "admin"}},
	}
	if mod != nil {
		mod(&c)
	}
	b, err := marshalConfig(agent.File{Listen: c.Listen, DockerSocket: c.DockerSocket, CloudflareAPIURL: c.CloudflareAPIURL, TelegramAPIURL: c.TelegramAPIURL, ComposeCommand: c.ComposeCommand, ApprovalTTL: c.ApprovalTTL, Environments: []agent.Hosted{{Project: "shop", Environment: c.Environment, StateDir: c.StateDir, KeyFile: c.KeyFile, Network: c.Network, Protected: c.Protected, Tokens: c.Tokens}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b), c
}

// setUpAgent is a server with the agent installed by exe, running.
func setUpAgent(t *testing.T, exe string) *agentFake {
	t.Helper()
	cfg, _ := existingConfig(t, nil)
	probe := with(setUpServer(),
		"agent_config=yes", "agent_config_stat="+wantPrivate, "agent_env=p5",
		"bin_sha="+fileHash(t, exe), "user=999", "user_groups=berthy docker",
		"unit_sha="+sha256Hex(unitContent()), "service_active=active", "service_enabled=enabled",
		"data_dir_stat="+wantPrivateDir, "state_dir_stat="+wantPrivateDir, "state_db=yes",
		"key_sha="+strings.Repeat("b", 64), "key_stat=32 "+wantPrivate)
	return &agentFake{probe: probe, config: cfg, version: "berthy v0.0.1-test linux/amd64\n"}
}

func freshAgentServer(t *testing.T) *agentFake { return &agentFake{probe: setUpServer()} }

// isolateUserConfig points the user configuration directory at a new temporary directory for the rest of the test, and clears
// BERTHY_CONFIG, so a step that writes the default CLI configuration never touches the configuration of the person who runs
// the tests.
func isolateUserConfig(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv(agentclient.ConfigEnv, "")
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("HOME", home)
	t.Setenv("AppData", home)
}

// agentSession is a run of setup against f, as the CLI would make it from an executable exe.
func agentSession(t *testing.T, f Runner, exe string, dry bool, mod func(*Options)) *Session {
	t.Helper()
	isolateUserConfig(t)
	o := Options{
		Env: "p5", Project: "shop", Network: "p5net", ListenIP: net.ParseIP(listenIP), ListenPort: 7710,
		Build:  Build{OS: "linux", Arch: "amd64", Path: exe, Version: "v0.0.1-test"},
		Now:    func() time.Time { return fixedNow },
		Getenv: func(string) string { return "" },
		Doctor: func(context.Context, string, string, string, string) ([]api.Check, error) { return okChecks(), nil },
	}
	if mod != nil {
		mod(&o)
	}
	return &Session{R: f, Opts: o, DryRun: dry}
}

func runSteps(s *Session) ([]StepResult, bool) {
	var got []StepResult
	ok := Run(context.Background(), s, Steps(), func(r StepResult) { got = append(got, r) })
	return got, ok
}

func stepOf(t *testing.T, got []StepResult, name string) StepResult {
	t.Helper()
	for _, r := range got {
		if r.Step == name {
			return r
		}
	}
	t.Fatalf("no step %s in %s", name, statuses(got))
	return StepResult{}
}

// agentChange reports whether a script changes the server in a way only the agent's steps do.
func agentChange(script string) bool {
	for _, m := range []string{"useradd", "usermod", "install -d", "mktemp", "systemctl restart", "systemctl enable", "daemon-reload",
		"mv -f", "chown", "chmod", "cat >", "journalctl", "systemctl stop"} {
		if strings.Contains(script, m) {
			return true
		}
	}
	return isChange(script)
}

func assertNoToken(t *testing.T, f *agentFake, got []StepResult, token string) {
	t.Helper()
	if token == "" {
		t.Fatal("no token was made")
	}
	for _, c := range f.calls {
		if strings.Contains(c.script, token) {
			t.Errorf("a script holds the token: %q", c.script)
		}
	}
	for _, r := range got {
		if strings.Contains(r.Detail, token) {
			t.Errorf("the %s detail holds the token", r.Step)
		}
	}
}

func TestFreshServerSetsUpTheAgent(t *testing.T) {
	exe := elfFile(t, emX86)
	f := freshAgentServer(t)
	cli := filepath.Join(t.TempDir(), "cli.json")
	s := agentSession(t, f, exe, false, func(o *Options) { o.CLIConfig = cli })
	got, ok := runSteps(s)
	if !ok {
		t.Fatalf("setup failed: %+v", got)
	}
	want := "connect=ok check=ok docker=ok swarm=ok network=ok user=changed binary=changed key=changed token=changed config=changed service=changed cli_config=changed cli_token=skipped verify=ok"
	if statuses(got) != want {
		t.Fatalf("statuses = %s\nwant       %s", statuses(got), want)
	}
	if s.Token == "" || !s.TokenLive || !strings.HasPrefix(s.Token, "bty.shop.p5.") || len(s.Token) != len("bty.shop.p5.")+64 {
		t.Fatalf("token = %q live=%v", s.Token, s.TokenLive)
	}
	assertNoToken(t, f, got, s.Token)

	// The changes come in this order.
	var order []string
	for _, c := range f.calls {
		switch {
		case strings.Contains(c.script, "useradd"):
			order = append(order, "user")
		case strings.Contains(c.script, "cat > "+binPath+".new"):
			order = append(order, "upload")
		case strings.Contains(c.script, "head -c 32 /dev/urandom"):
			order = append(order, "key")
		case strings.Contains(c.script, "cat > "+configPath+".new"):
			order = append(order, "config")
		case strings.Contains(c.script, "daemon-reload"):
			order = append(order, "unit")
		case strings.Contains(c.script, "systemctl enable berthy"):
			order = append(order, "enable")
		case strings.Contains(c.script, "mv -f "+binPath+".new"):
			order = append(order, "swap")
		case strings.Contains(c.script, "systemctl restart berthy"):
			order = append(order, "restart")
		}
	}
	if strings.Join(order, " ") != "user upload key config unit enable swap restart" {
		t.Fatalf("order = %v", order)
	}

	if c, _ := f.find("useradd"); !strings.Contains(c.script, "useradd --system --user-group --home-dir /var/lib/berthy --shell /usr/sbin/nologin berthy") ||
		!strings.Contains(c.script, "usermod -aG docker berthy") {
		t.Errorf("user script = %q", c.script)
	}
	// The binary goes up through stdin and is made executable.
	if c, _ := f.find("cat > " + binPath + ".new"); !bytes.Equal(c.stdin, mustRead(t, exe)) || !strings.Contains(c.script, "chmod 0755") || !strings.Contains(c.script, "umask 022") {
		t.Errorf("the binary upload is wrong: %q", c.script)
	}
	// The key is made on the server, owned by berthy, and nothing of it travels.
	if c, _ := f.find("head -c 32 /dev/urandom"); len(c.stdin) != 0 || !strings.Contains(c.script, "chown berthy:berthy") || !strings.Contains(c.script, "chmod 0600") ||
		!strings.Contains(c.script, "install -d -o berthy -g berthy -m 0700 "+shellQuote(stateDir)) {
		t.Errorf("the key script is wrong: %q", c.script)
	}
	// The configuration arrives on stdin, with the digest and not the token.
	c, _ := f.find("cat > " + configPath + ".new")
	file, err := agent.ParseFile(c.stdin)
	if err != nil {
		t.Fatal(err)
	}
	cfg := file.Configs()[0]
	if cfg.Environment != "p5" || cfg.Listen != listenIP+":7710" || cfg.Network != "p5net" || cfg.StateDir != stateDir || cfg.KeyFile != keyPath ||
		cfg.DockerSocket != dockerSocket || cfg.Protected || len(cfg.Tokens) != 1 {
		t.Errorf("config = %+v", cfg)
	}
	if tok := cfg.Tokens[0]; tok.Name != "admin" || tok.Scope != "admin" || tok.Digest != agent.Digest(s.Token) || tok.Agent {
		t.Errorf("token entry = %+v", tok)
	}
	if bytes.Contains(c.stdin, []byte(s.Token)) {
		t.Errorf("the configuration holds the token")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the configuration is not valid: %v", err)
	}
	if !strings.Contains(c.script, "chown berthy:berthy "+configPath+".new") || !strings.Contains(c.script, "chmod 0600 "+configPath+".new") {
		t.Errorf("config script = %q", c.script)
	}
	// The CLI configuration was written, and holds no token.
	data := mustRead(t, cli)
	if bytes.Contains(data, []byte(s.Token)) || !bytes.Contains(data, []byte(`"token_env": "BERTHY_SHOP_P5_TOKEN"`)) || !bytes.Contains(data, []byte(`"url": "http://`+listenIP+`:7710"`)) {
		t.Errorf("CLI config = %s", data)
	}
	if st, err := os.Stat(cli); err != nil || !modeIs(st, 0o600) {
		t.Errorf("CLI config mode: %v %v", st, err)
	}
	if d := stepOf(t, got, "verify").Detail; !strings.Contains(d, "3 of 3 checks ok") || !strings.Contains(d, "v0.0.1-test") {
		t.Errorf("verify detail = %q", d)
	}
}

// modeIs reports whether st has the permission bits want. Windows has no such bits, so it always does there.
func modeIs(st os.FileInfo, want fs.FileMode) bool {
	return runtime.GOOS == "windows" || st.Mode().Perm() == want
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSetUpServerChangesNothingAndMakesNoToken(t *testing.T) {
	exe := elfFile(t, emX86)
	f := setUpAgent(t, exe)
	cli := filepath.Join(t.TempDir(), "cli.json")
	writeCLIConfig(t, cli, `{"projects":{"shop":{"environments":{"p5":{"url":"http://`+listenIP+`:7710","token_env":"MY_P5_TOKEN"}}}}}`)
	before := mustRead(t, cli)
	asked := ""
	s := agentSession(t, f, exe, false, func(o *Options) {
		o.CLIConfig = cli
		o.Getenv = func(n string) string {
			asked = n
			return "bty.shop.p5." + strings.Repeat("c", 64)
		}
	})
	got, ok := runSteps(s)
	if !ok {
		t.Fatalf("failed: %+v", got)
	}
	if want := "connect=ok check=ok docker=ok swarm=ok network=ok user=ok binary=ok key=ok token=ok config=ok service=ok cli_config=ok cli_token=ok verify=ok"; statuses(got) != want {
		t.Fatalf("statuses = %s", statuses(got))
	}
	for _, sc := range f.scripts() {
		if agentChange(sc) {
			t.Errorf("a change was sent to a set-up server: %q", sc)
		}
	}
	if s.Token != "" || s.TokenLive {
		t.Errorf("a token was made")
	}
	if !bytes.Equal(before, mustRead(t, cli)) {
		t.Errorf("the CLI configuration changed")
	}
	if asked != "MY_P5_TOKEN" {
		t.Errorf("verify read $%s, want the token_env of the CLI configuration", asked)
	}
	for _, r := range got {
		if strings.Contains(r.Detail, "bty.") {
			t.Errorf("%s detail = %q", r.Step, r.Detail)
		}
	}
}

func writeCLIConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNewTokenAddsOneAdminTokenAndKeepsTheOthers(t *testing.T) {
	exe := elfFile(t, emX86)
	f := setUpAgent(t, exe)
	s := agentSession(t, f, exe, false, func(o *Options) { o.NewToken = true })
	got, ok := runSteps(s)
	if !ok {
		t.Fatalf("failed: %+v", got)
	}
	if want := "user=ok binary=ok key=ok token=changed config=changed service=changed"; !strings.Contains(statuses(got), want) {
		t.Fatalf("statuses = %s", statuses(got))
	}
	assertNoToken(t, f, got, s.Token)
	c, _ := f.find("cat > " + configPath + ".new")
	file, err := agent.ParseFile(c.stdin)
	if err != nil {
		t.Fatal(err)
	}
	cfg := file.Configs()[0]
	if len(cfg.Tokens) != 2 || cfg.Tokens[0].Digest != strings.Repeat("a", 64) || cfg.Tokens[0].Name != "admin" {
		t.Fatalf("tokens = %+v", cfg.Tokens)
	}
	if n := cfg.Tokens[1]; n.Name != "admin-20261116-123045" || n.Scope != "admin" || n.Digest != agent.Digest(s.Token) {
		t.Errorf("new token = %+v", n)
	}
	if !s.TokenLive {
		t.Errorf("the token is not live")
	}
	// The configuration is swapped in with the previous one kept, and the service restarts: the binary did not change.
	if c, ok := f.find("mv -f " + configPath + ".new"); !ok || strings.Contains(c.script, binPath+".new") || !strings.Contains(c.script, "cp -p "+configPath+" "+configPath+".prev") {
		t.Errorf("swap = %q", c.script)
	}
}

func TestDryRunOfFreshServerSendsOnlyReads(t *testing.T) {
	exe := elfFile(t, emX86)
	f := freshAgentServer(t)
	cli := filepath.Join(t.TempDir(), "cli.json")
	s := agentSession(t, f, exe, true, func(o *Options) { o.CLIConfig = cli })
	got, ok := runSteps(s)
	if !ok {
		t.Fatalf("failed: %+v", got)
	}
	want := "connect=ok check=ok docker=ok swarm=ok network=ok user=would_change binary=would_change key=would_change token=would_change config=would_change service=would_change cli_config=would_change cli_token=would_change verify=skipped"
	if statuses(got) != want {
		t.Fatalf("statuses = %s", statuses(got))
	}
	for i, w := range map[string]string{
		"user":       "would create the berthy user, add it to the docker group",
		"binary":     "would install this CLI (v0.0.1-test)",
		"token":      "would create an admin token",
		"config":     "would create /etc/berthy/agent.json (listen " + listenIP + ":7710, 1 environment)",
		"service":    "would install, enable and start berthy.service",
		"verify":     "would check the agent",
		"cli_config": "would write p5 to cli.json",
	} {
		if d := stepOf(t, got, i).Detail; d != w {
			t.Errorf("%s detail = %q, want %q", i, d, w)
		}
	}
	if d := stepOf(t, got, "key").Detail; !strings.Contains(d, "generate an encryption key") {
		t.Errorf("key detail = %q", d)
	}
	for _, sc := range f.scripts() {
		if sc != "id -u; id -un" && !strings.Contains(sc, "uname -s") && !strings.Contains(sc, "# hosted path probe") {
			t.Errorf("a dry run sent %q", sc)
		}
	}
	if s.Token != "" {
		t.Errorf("a dry run made a token")
	}
	if _, err := os.Stat(cli); err == nil {
		t.Errorf("a dry run wrote the CLI configuration")
	}
}

func TestDryRunOfSetUpServerOnlyReads(t *testing.T) {
	exe := elfFile(t, emX86)
	f := setUpAgent(t, exe)
	s := agentSession(t, f, exe, true, nil)
	got, ok := runSteps(s)
	if !ok || !strings.Contains(statuses(got), "user=ok binary=ok key=ok token=ok config=ok service=ok cli_config=would_change cli_token=skipped verify=skipped") {
		t.Fatalf("got %s ok=%v", statuses(got), ok)
	}
	for _, sc := range f.scripts() {
		if agentChange(sc) {
			t.Errorf("a dry run sent %q", sc)
		}
	}
}

func TestUserStep(t *testing.T) {
	exe := elfFile(t, emX86)
	tests := []struct {
		name   string
		mod    func(f *agentFake)
		status string
		script []string
		not    []string
	}{
		{"user missing", func(f *agentFake) { f.probe = without(f.probe, "user=", "user_groups=") }, StatusChanged, []string{"useradd --system", "usermod -aG docker berthy"}, nil},
		{"not in docker", func(f *agentFake) { f.probe = with(without(f.probe, "user_groups="), "user_groups=berthy") }, StatusChanged, []string{"usermod -aG docker berthy"}, []string{"useradd"}},
		{"in docker", nil, StatusOK, nil, []string{"useradd", "usermod"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := setUpAgent(t, exe)
			if tc.mod != nil {
				tc.mod(f)
			}
			s := agentSession(t, f, exe, false, nil)
			var got []StepResult
			Run(context.Background(), s, []Step{{"connect", connectStep}, {"check", checkStep}, {"user", userStep}}, func(r StepResult) { got = append(got, r) })
			if r := stepOf(t, got, "user"); r.Status != tc.status {
				t.Fatalf("user = %+v", r)
			}
			for _, m := range tc.script {
				if !f.ran(m) {
					t.Errorf("no script with %q", m)
				}
			}
			for _, m := range tc.not {
				if f.ran(m) {
					t.Errorf("a script with %q was sent", m)
				}
			}
		})
	}
}

// without removes the lines that start with any of the prefixes.
func without(lines []string, prefixes ...string) []string {
	var out []string
	for _, l := range lines {
		drop := false
		for _, p := range prefixes {
			if strings.HasPrefix(l, p) {
				drop = true
			}
		}
		if !drop {
			out = append(out, l)
		}
	}
	return out
}

func keyBytes() []byte { return bytes.Repeat([]byte{0x42}, 32) }

func writeKey(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "saved.key")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestKeyStep(t *testing.T) {
	exe := elfFile(t, emX86)
	key := keyBytes()
	noKey := func(f *agentFake) { f.probe = without(f.probe, "key_sha=", "key_stat=") }
	otherKey := func(f *agentFake) {
		f.probe = with(without(f.probe, "key_sha="), "key_sha="+sha256Hex([]byte("another key of 32 bytes.........")))
	}
	sameKey := func(f *agentFake) { f.probe = with(without(f.probe, "key_sha="), "key_sha="+sha256Hex(key)) }
	noKeyWithState := func(f *agentFake) { noKey(f); f.probe = with(f.probe, "state_db=yes") }
	noKeyNoState := func(f *agentFake) { noKey(f); f.probe = without(f.probe, "state_db=") }
	tests := []struct {
		name    string
		mod     func(f *agentFake)
		keyFile []byte
		status  string
		err     string
		script  string // a marker of the script that must have run
		uploads bool
	}{
		{"existing key kept", nil, nil, StatusOK, "", "", false},
		{"generated when there is none", noKeyNoState, nil, StatusChanged, "", "head -c 32 /dev/urandom", false},
		{"state without a key", noKeyWithState, nil, StatusFailed, stateDir + " has state but no key: pass --key-file with the saved key", "", false},
		{"key-file same as the server's", sameKey, key, StatusOK, "", "", false},
		{"key-file differs", otherKey, key, StatusFailed, "the server has a different encryption key; setup never replaces it", "", false},
		{"key-file uploaded", noKeyNoState, key, StatusChanged, "", "cat > \"$tmp\"", true},
		{"key-file with a restored state", noKeyWithState, key, StatusChanged, "", "cat > \"$tmp\"", true},
		{"key-file of the wrong size", sameKey, []byte("short"), StatusFailed, "--key-file must hold exactly 32 bytes", "", false},
		{"key on the server is not 32 bytes", func(f *agentFake) { f.probe = with(without(f.probe, "key_stat="), "key_stat=16 "+wantPrivate) }, nil, StatusFailed, keyPath + " is not 32 bytes; setup does not replace a key", "", false},
		{"owner of the key repaired", func(f *agentFake) { f.probe = with(without(f.probe, "key_stat="), "key_stat=32 root:root 644") }, nil, StatusChanged, "", "chmod 0600 " + shellQuote(keyPath), false},
		{"state owned by someone else", func(f *agentFake) { f.probe = with(f.probe, "state_foreign=yes") }, nil, StatusChanged, "", "find " + shellQuote(stateDir) + " -type f -user root -exec chown berthy:berthy", false},
		{"state directory missing", func(f *agentFake) { f.probe = without(f.probe, "state_dir_stat=") }, nil, StatusChanged, "", "install -d -o berthy -g berthy -m 0700 " + shellQuote(stateDir), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := setUpAgent(t, exe)
			if tc.mod != nil {
				tc.mod(f)
			}
			s := agentSession(t, f, exe, false, func(o *Options) {
				if tc.keyFile != nil {
					o.KeyFile = writeKey(t, tc.keyFile)
				}
			})
			var got []StepResult
			Run(context.Background(), s, []Step{{"connect", connectStep}, {"check", checkStep}, {"key", keyStep}}, func(r StepResult) { got = append(got, r) })
			r := stepOf(t, got, "key")
			if r.Status != tc.status || (tc.err != "" && r.Detail != tc.err) {
				t.Fatalf("key = %+v", r)
			}
			if tc.script != "" && !f.ran(tc.script) {
				t.Errorf("no script with %q:\n%v", tc.script, f.scripts())
			}
			c, uploaded := f.find("cat > \"$tmp\"")
			if uploaded != tc.uploads {
				t.Fatalf("upload = %v, want %v", uploaded, tc.uploads)
			}
			if uploaded && !bytes.Equal(c.stdin, key) {
				t.Errorf("the key did not arrive on stdin")
			}
			// No key byte is in any script, and nothing the key step prints repeats it.
			for _, c := range f.calls {
				if bytes.Contains([]byte(c.script), key) {
					t.Errorf("a script holds the key")
				}
			}
			if r.Status == StatusFailed {
				for _, sc := range f.scripts() {
					if strings.Contains(sc, "mktemp") || strings.Contains(sc, "mv -f") {
						t.Errorf("a failed key step changed the server: %q", sc)
					}
				}
			}
		})
	}
}

func TestKeyStepDryRun(t *testing.T) {
	exe := elfFile(t, emX86)
	f := setUpAgent(t, exe)
	f.probe = without(f.probe, "key_sha=", "key_stat=", "state_db=")
	s := agentSession(t, f, exe, true, func(o *Options) { o.KeyFile = writeKey(t, keyBytes()) })
	var got []StepResult
	Run(context.Background(), s, []Step{{"connect", connectStep}, {"check", checkStep}, {"key", keyStep}}, func(r StepResult) { got = append(got, r) })
	r := stepOf(t, got, "key")
	if r.Status != StatusWouldChange || r.Detail != "would install the key from --key-file" {
		t.Fatalf("key = %+v", r)
	}
	for _, c := range f.calls {
		if len(c.stdin) > 0 {
			t.Errorf("a dry run sent data on stdin")
		}
	}
}

func TestConfigMergeKeepsWhatSetupDoesNotOwn(t *testing.T) {
	exe := elfFile(t, emX86)
	f := setUpAgent(t, exe)
	f.config, _ = existingConfig(t, func(c *agent.Config) {
		c.Protected = true
		c.ApprovalTTL = "30m"
		c.ComposeCommand = []string{"/opt/docker-compose"}
		c.CloudflareAPIURL = "https://cf.example.test/client/v4"
		c.TelegramAPIURL = "https://tg.example.test"
		c.DockerSocket = "/run/custom.sock"
		c.Tokens = append(c.Tokens, agent.Token{Name: "ci", Digest: strings.Repeat("d", 64), Scope: "deploy"}, agent.Token{Name: "bot", Digest: strings.Repeat("e", 64), Scope: "read", Agent: true})
	})
	f.probe = with(without(f.probe, "network="), "network=overlay swarm true newnet")
	s := agentSession(t, f, exe, false, func(o *Options) { o.Network = "newnet"; o.Protected = false })
	got, ok := runSteps(s)
	if !ok {
		t.Fatalf("failed: %+v", got)
	}
	if st := stepOf(t, got, "config"); st.Status != StatusChanged || !strings.Contains(st.Detail, "updated /etc/berthy/agent.json") || !strings.Contains(st.Detail, "1 environment") {
		t.Fatalf("config = %+v", st)
	}
	c, _ := f.find("cat > " + configPath + ".new")
	file, err := agent.ParseFile(c.stdin)
	if err != nil {
		t.Fatal(err)
	}
	cfg := file.Configs()[0]
	oldFile, err := agent.ParseFile([]byte(f.config))
	if err != nil {
		t.Fatal(err)
	}
	old := oldFile.Configs()[0]
	want := old
	want.Network = "newnet"
	wb, _ := json.Marshal(want)
	gb, _ := json.Marshal(cfg)
	if !bytes.Equal(wb, gb) {
		t.Fatalf("config = %s\nwant     %s", gb, wb)
	}
	if !cfg.Protected {
		t.Errorf("protection was turned off")
	}
	if s.Token != "" {
		t.Errorf("a token was made without --new-token")
	}
	// The config was not swapped in with an old binary rollback of the binary: only the configuration changed.
	if sw, ok := f.find("mv -f " + configPath + ".new"); !ok || strings.Contains(sw.script, binPath) {
		t.Errorf("swap = %q", sw.script)
	}
}

func TestConfigProtectedIsSetFromTheManifestAndNeverCleared(t *testing.T) {
	exe := elfFile(t, emX86)
	f := setUpAgent(t, exe)
	s := agentSession(t, f, exe, false, func(o *Options) { o.Protected = true })
	got, ok := runSteps(s)
	if !ok || stepOf(t, got, "config").Status != StatusChanged {
		t.Fatalf("got %s ok=%v", statuses(got), ok)
	}
	c, _ := f.find("cat > " + configPath + ".new")
	if !bytes.Contains(c.stdin, []byte(`"protected": true`)) {
		t.Errorf("config = %s", c.stdin)
	}
}

func TestConfigIsWrittenOnlyOnChange(t *testing.T) {
	exe := elfFile(t, emX86)
	// The server's file is formatted differently but says the same: no write.
	f := setUpAgent(t, exe)
	var c agent.File
	if err := json.Unmarshal([]byte(f.config), &c); err != nil {
		t.Fatal(err)
	}
	compact, _ := json.Marshal(c)
	f.config = string(compact)
	s := agentSession(t, f, exe, false, nil)
	got, ok := runSteps(s)
	if !ok || stepOf(t, got, "config").Status != StatusOK {
		t.Fatalf("got %s ok=%v", statuses(got), ok)
	}
	if f.ran(configPath + ".new") {
		t.Errorf("the configuration was written")
	}
}

func TestConfigOwnerAndModeAreRepairedInPlace(t *testing.T) {
	exe := elfFile(t, emX86)
	f := setUpAgent(t, exe)
	f.probe = with(without(f.probe, "agent_config_stat="), "agent_config_stat=root:berthy 640")
	s := agentSession(t, f, exe, false, nil)
	got, ok := runSteps(s)
	if !ok || stepOf(t, got, "config").Status != StatusChanged || !f.ran("chmod 0600 "+configPath) {
		t.Fatalf("got %s ok=%v", statuses(got), ok)
	}
	if f.restarts != 0 {
		t.Errorf("the service restarted for a mode fix")
	}
}

func TestListenRules(t *testing.T) {
	exe := elfFile(t, emX86)
	t.Run("kept from the configuration when --listen is not given", func(t *testing.T) {
		f := setUpAgent(t, exe)
		s := agentSession(t, f, exe, false, func(o *Options) { o.ListenIP, o.ListenPort = nil, 0 })
		got, ok := runSteps(s)
		if !ok {
			t.Fatalf("failed: %+v", got)
		}
		if d := stepOf(t, got, "check").Detail; !strings.Contains(d, listenIP+" assigned") {
			t.Errorf("check = %q", d)
		}
		if s.Opts.listen() != listenIP+":7710" {
			t.Errorf("listen = %q", s.Opts.listen())
		}
	})
	t.Run("a different address is refused", func(t *testing.T) {
		f := setUpAgent(t, exe)
		f.probe = with(f.probe, "addr=172.31.250.11")
		s := agentSession(t, f, exe, false, func(o *Options) { o.ListenIP = net.ParseIP("172.31.250.11") })
		got, ok := runSteps(s)
		r := lastResult(got)
		if ok || r.Step != "check" || r.Detail != "the agent listens on "+listenIP+":7710; setup does not move it" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a different port is refused", func(t *testing.T) {
		f := setUpAgent(t, exe)
		s := agentSession(t, f, exe, false, func(o *Options) { o.ListenPort = 7711 })
		got, ok := runSteps(s)
		if ok || !strings.Contains(lastResult(got).Detail, "setup does not move it") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("needed on a server without a configuration", func(t *testing.T) {
		f := freshAgentServer(t)
		s := agentSession(t, f, exe, false, func(o *Options) { o.ListenIP, o.ListenPort = nil, 0 })
		got, ok := runSteps(s)
		r := lastResult(got)
		if ok || r.Step != "check" || !strings.Contains(r.Detail, "--listen ADDRESS:PORT is needed") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a public address is refused before anything changes", func(t *testing.T) {
		f := freshAgentServer(t)
		f.probe = with(f.probe, "addr=8.8.8.8")
		s := agentSession(t, f, exe, false, func(o *Options) { o.ListenIP = net.ParseIP("8.8.8.8") })
		got, ok := runSteps(s)
		r := lastResult(got)
		if ok || r.Step != "check" || !strings.Contains(r.Detail, "loopback, private, or VPN") {
			t.Fatalf("got %+v", got)
		}
		for _, sc := range f.scripts() {
			if agentChange(sc) {
				t.Errorf("a change was sent: %q", sc)
			}
		}
	})
}

func TestInvalidExistingConfigStopsWithoutPrintingIt(t *testing.T) {
	exe := elfFile(t, emX86)
	for name, content := range map[string]string{
		"not json":      "not json at all SECRETVALUE",
		"unknown field": `{"environments":[],"wat":"SECRETVALUE"}`,
		"invalid":       `{"listen":"` + listenIP + `:7710","environments":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := setUpAgent(t, exe)
			f.config = content
			got, ok := runSteps(agentSession(t, f, exe, false, nil))
			r := lastResult(got)
			if ok || r.Step != "check" || !strings.Contains(r.Detail, configPath) || strings.Contains(r.Detail, "SECRETVALUE") {
				t.Fatalf("got %+v", got)
			}
			for _, sc := range f.scripts() {
				if agentChange(sc) {
					t.Errorf("a change was sent: %q", sc)
				}
			}
		})
	}
}

func TestBinaryStep(t *testing.T) {
	x86 := elfFile(t, emX86)
	arm := elfFile(t, emArm)
	text := filepath.Join(t.TempDir(), "notelf")
	if err := os.WriteFile(text, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	release := func(o *Options) {
		o.Build = Build{OS: "windows", Arch: "amd64", Version: "v1.2.3", Release: true}
	}
	tests := []struct {
		name   string
		server func(*agentFake)
		opts   func(*Options)
		status string
		detail string
		script []string
	}{
		{"same hash is skipped", nil, nil, StatusOK, "this CLI (v0.0.1-test) is installed", nil},
		{"another binary is replaced", func(f *agentFake) { f.probe = with(without(f.probe, "bin_sha="), "bin_sha="+strings.Repeat("9", 64)) }, nil, StatusChanged, "uploaded this CLI (v0.0.1-test), sha256 ", []string{"cat > /usr/local/bin/berthy.new"}},
		{"--binary of the right arch", func(f *agentFake) { f.probe = without(f.probe, "bin_sha=") }, func(o *Options) { o.Binary = x86; o.Build = Build{OS: "windows", Arch: "amd64", Version: "dev"} }, StatusChanged, "uploaded --binary berthy, sha256 ", []string{"cat > /usr/local/bin/berthy.new"}},
		{"--binary of the wrong arch", nil, func(o *Options) { o.Binary = arm }, StatusFailed, "berthy is built for arm64, but the server is amd64", nil},
		{"--binary that is not an ELF", nil, func(o *Options) { o.Binary = text }, StatusFailed, "notelf is not a Linux executable", nil},
		{"--binary that is missing", nil, func(o *Options) { o.Binary = filepath.Join(t.TempDir(), "nope") }, StatusFailed, "cannot read --binary", nil},
		{"--binary wins over this CLI", func(f *agentFake) { f.probe = without(f.probe, "bin_sha=") }, func(o *Options) { o.Binary = x86 }, StatusChanged, "uploaded --binary berthy", nil},
		{"a Linux CLI of another arch downloads", func(f *agentFake) { f.probe = without(f.probe, "bin_sha=") }, func(o *Options) {
			o.Build = Build{OS: "linux", Arch: "arm64", Path: arm, Version: "v1.2.3", Release: true}
		}, StatusChanged, "downloaded v1.2.3 and verified its checksum", []string{"https://github.com/fredimachado/berthy/releases/download/v1.2.3/berthy-linux-amd64"}},
		{"a release the server already has", func(f *agentFake) { f.version = "berthy v1.2.3 linux/amd64\n" }, release, StatusOK, "v1.2.3 is installed", nil},
		{"another release is downloaded", func(f *agentFake) { f.version = "berthy v1.2.2 linux/amd64\n" }, release, StatusChanged, "downloaded v1.2.3 and verified its checksum", []string{"checksums.txt", "sha256sum -c", "checksum mismatch"}},
		{"a server that says no version is replaced", func(f *agentFake) { f.version = "" }, release, StatusChanged, "downloaded v1.2.3 and verified its checksum", nil},
		{"BERTHY_RELEASE_URL on a private address", func(f *agentFake) { f.probe = without(f.probe, "bin_sha=") }, func(o *Options) {
			release(o)
			o.ReleaseURL = "http://172.31.250.1:8000/rel/"
		}, StatusChanged, "downloaded v1.2.3 and verified its checksum", []string{"'http://172.31.250.1:8000/rel/v1.2.3/berthy-linux-amd64'", "'http://172.31.250.1:8000/rel/v1.2.3/checksums.txt'"}},
		{"BERTHY_RELEASE_URL on the internet over http", nil, func(o *Options) { release(o); o.ReleaseURL = "http://example.com/rel" }, StatusFailed, "BERTHY_RELEASE_URL must be an https URL, or an http URL on a loopback or private address", nil},
		{"a development build", nil, func(o *Options) { o.Build = Build{OS: "darwin", Arch: "arm64", Version: "dev"} }, StatusFailed, "this CLI is a development build for darwin/arm64: pass --binary with a Linux amd64 build of Berthy", nil},
		{"a development build for another Linux arch", nil, func(o *Options) { o.Build = Build{OS: "linux", Arch: "arm64", Path: arm, Version: "dev"} }, StatusFailed, "this CLI is a development build for linux/arm64: pass --binary with a Linux amd64 build of Berthy", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := setUpAgent(t, x86)
			if tc.server != nil {
				tc.server(f)
			}
			s := agentSession(t, f, x86, false, tc.opts)
			var got []StepResult
			Run(context.Background(), s, []Step{{"connect", connectStep}, {"check", checkStep}, {"binary", binaryStep}}, func(r StepResult) { got = append(got, r) })
			r := stepOf(t, got, "binary")
			if r.Status != tc.status || !strings.HasPrefix(r.Detail, tc.detail) {
				t.Fatalf("binary = %+v", r)
			}
			for _, m := range tc.script {
				if !f.ran(m) {
					t.Errorf("no script with %q", m)
				}
			}
			if tc.status == StatusOK && f.ran(binPath+".new") {
				t.Errorf("a binary was staged although the server has it")
			}
			if tc.status == StatusChanged != s.staged.Binary {
				t.Errorf("staged = %v for %s", s.staged.Binary, tc.status)
			}
		})
	}
}

func TestReleaseDownloadChecksTheChecksum(t *testing.T) {
	exe := elfFile(t, emX86)
	f := setUpAgent(t, exe)
	f.probe = without(f.probe, "bin_sha=")
	s := agentSession(t, f, exe, false, func(o *Options) {
		o.Build = Build{OS: "windows", Arch: "amd64", Version: "v1.2.3", Release: true}
	})
	runSteps(s)
	c, ok := f.find("checksums.txt")
	if !ok {
		t.Fatal("no download script")
	}
	for _, want := range []string{
		"curl -fsSL 'https://github.com/fredimachado/berthy/releases/download/v1.2.3/berthy-linux-amd64'",
		"curl -fsSL 'https://github.com/fredimachado/berthy/releases/download/v1.2.3/checksums.txt'",
		`grep -E '[ *]berthy-linux-amd64$' checksums.txt`, "sha256sum -c -", "checksum mismatch", `mktemp -d "$dir/.berthy-download.XXXXXX"`,
		"mv -f berthy-linux-amd64 /usr/local/bin/berthy.new",
	} {
		if !strings.Contains(c.script, want) {
			t.Errorf("the download script lacks %q:\n%s", want, c.script)
		}
	}
	if len(c.stdin) != 0 {
		t.Errorf("the download sent data")
	}
}

func TestReleaseDownloadUsesWgetWithoutCurl(t *testing.T) {
	script := downloadScript("https://x.test/v1", "berthy-linux-arm64", "wget")
	if !strings.Contains(script, "wget -qO \"$tmp/berthy-linux-arm64\" 'https://x.test/v1/berthy-linux-arm64'") || strings.Contains(script, "curl") {
		t.Errorf("script = %s", script)
	}
}

func TestUploadWithAnotherHashFails(t *testing.T) {
	exe := elfFile(t, emX86)
	f := setUpAgent(t, exe)
	f.probe = without(f.probe, "bin_sha=")
	f.corrupt = true
	got, ok := runSteps(agentSession(t, f, exe, false, nil))
	r := lastResult(got)
	if ok || r.Step != "binary" || !strings.Contains(r.Detail, "does not match") || !f.ran("rm -f "+binPath+".new") {
		t.Fatalf("got %+v", got)
	}
}

func TestReleaseBase(t *testing.T) {
	good := map[string]string{
		"":                               DefaultReleaseURL,
		"https://mirror.example.com/b/":  "https://mirror.example.com/b",
		"http://172.31.250.1:8000":       "http://172.31.250.1:8000",
		"http://127.0.0.1:9":             "http://127.0.0.1:9",
		"http://[::1]:9/x":               "http://[::1]:9/x",
		"http://localhost:8080":          "http://localhost:8080",
		"http://10.1.2.3/rel":            "http://10.1.2.3/rel",
		"http://192.168.0.5":             "http://192.168.0.5",
		"https://user.example.com:8443/": "https://user.example.com:8443",
	}
	for in, want := range good {
		got, err := ReleaseBase(in)
		if err != nil || got != want {
			t.Errorf("ReleaseBase(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"http://example.com", "http://8.8.8.8", "http://172.32.0.1", "ftp://10.0.0.1", "file:///etc", "https://u:p@example.com",
		"https://example.com?x=1", "https://example.com#f", "example.com", "http:///x", "http://0.0.0.0.example.com",
	} {
		if got, err := ReleaseBase(in); err == nil {
			t.Errorf("ReleaseBase(%q) = %q, want an error", in, got)
		} else if strings.Contains(err.Error(), in) {
			t.Errorf("the error repeats the URL: %v", err)
		}
	}
}

func TestUnitIsExactlyTheFileInTheRepository(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "systemd", "berthy.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(unitContent(), bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))) {
		t.Fatal("the embedded unit differs from deploy/systemd/berthy.service")
	}
	if len(systemd.Unit) == 0 || !bytes.Contains(unitContent(), []byte("ExecStart=/usr/local/bin/berthy agent --config /etc/berthy/agent.json")) {
		t.Fatal("the unit is not the agent's")
	}
}

func TestServiceStep(t *testing.T) {
	exe := elfFile(t, emX86)
	t.Run("nothing to do", func(t *testing.T) {
		f := setUpAgent(t, exe)
		s := agentSession(t, f, exe, false, nil)
		got, _ := runSteps(s)
		if r := stepOf(t, got, "service"); r.Status != StatusOK || f.restarts != 0 {
			t.Fatalf("service = %+v restarts=%d", r, f.restarts)
		}
	})
	t.Run("a changed unit is written and the service restarts", func(t *testing.T) {
		f := setUpAgent(t, exe)
		f.probe = with(without(f.probe, "unit_sha="), "unit_sha="+strings.Repeat("1", 64))
		got, ok := runSteps(agentSession(t, f, exe, false, nil))
		if !ok {
			t.Fatalf("failed: %+v", got)
		}
		r := stepOf(t, got, "service")
		if r.Status != StatusChanged || r.Detail != "restarted berthy.service (unit changed)" || f.restarts != 1 {
			t.Fatalf("service = %+v restarts=%d", r, f.restarts)
		}
		c, _ := f.find("daemon-reload")
		if !bytes.Equal(c.stdin, unitContent()) || !strings.Contains(c.script, "mv -f \"$tmp\" "+unitPath) || !strings.Contains(c.script, "chmod 0644") {
			t.Errorf("unit script = %q", c.script)
		}
		if !f.ran("systemctl enable berthy") {
			t.Errorf("not enabled")
		}
	})
	t.Run("a stopped service is started without swapping anything", func(t *testing.T) {
		f := setUpAgent(t, exe)
		f.probe = with(without(f.probe, "service_active="), "service_active=inactive")
		got, ok := runSteps(agentSession(t, f, exe, false, nil))
		r := stepOf(t, got, "service")
		if !ok || r.Status != StatusChanged || r.Detail != "started berthy.service" || f.ran("mv -f") {
			t.Fatalf("service = %+v ok=%v", r, ok)
		}
	})
	t.Run("a disabled service is enabled and not restarted", func(t *testing.T) {
		f := setUpAgent(t, exe)
		f.probe = with(without(f.probe, "service_enabled="), "service_enabled=disabled")
		got, ok := runSteps(agentSession(t, f, exe, false, nil))
		r := stepOf(t, got, "service")
		if !ok || r.Status != StatusChanged || r.Detail != "enabled berthy.service" || f.restarts != 0 || !f.ran("systemctl enable berthy") {
			t.Fatalf("service = %+v restarts=%d", r, f.restarts)
		}
	})
	t.Run("a new binary restarts it and keeps the previous one", func(t *testing.T) {
		f := setUpAgent(t, exe)
		f.prevBin = true
		f.probe = with(without(f.probe, "bin_sha="), "bin_sha="+strings.Repeat("9", 64))
		got, ok := runSteps(agentSession(t, f, exe, false, nil))
		r := stepOf(t, got, "service")
		if !ok || r.Detail != "restarted berthy.service (binary changed)" {
			t.Fatalf("service = %+v", r)
		}
		sw, _ := f.find("mv -f " + binPath + ".new")
		if !strings.Contains(sw.script, "cp -p "+binPath+" "+binPath+".prev") {
			t.Errorf("swap = %q", sw.script)
		}
	})
}

// activate drives Activate with the fake.
func activate(t *testing.T, f *agentFake, staged Staged) error {
	t.Helper()
	return Activate(context.Background(), f, listenIP+":7710", staged, 3*time.Second)
}

func TestActivateSucceeds(t *testing.T) {
	f := &agentFake{prevBin: true, prevCfg: true}
	if err := activate(t, f, Staged{Binary: true, Config: true}); err != nil {
		t.Fatal(err)
	}
	s := f.scripts()
	if len(s) != 2 {
		t.Fatalf("scripts = %q", s)
	}
	for _, want := range []string{
		"cp -p /usr/local/bin/berthy /usr/local/bin/berthy.prev", "mv -f /usr/local/bin/berthy.new /usr/local/bin/berthy",
		"cp -p /etc/berthy/agent.json /etc/berthy/agent.json.prev", "mv -f /etc/berthy/agent.json.new /etc/berthy/agent.json",
	} {
		if !strings.Contains(s[0], want) {
			t.Errorf("swap lacks %q:\n%s", want, s[0])
		}
	}
	for _, want := range []string{"systemctl restart berthy", "http://" + listenIP + ":7710/healthz", "curl", "wget", "-lt 3", "systemctl is-failed"} {
		if !strings.Contains(s[1], want) {
			t.Errorf("restart lacks %q:\n%s", want, s[1])
		}
	}
	// A service that crash-looped hit its start limit: the restart has to clear it first, or systemd refuses it as too quick.
	reset, restart := strings.Index(s[1], "systemctl reset-failed berthy"), strings.Index(s[1], "systemctl restart berthy")
	if reset < 0 || reset > restart {
		t.Errorf("reset-failed does not come before the restart:\n%s", s[1])
	}
	if f.ran("journalctl") || f.ran("systemctl stop") {
		t.Errorf("a rollback ran")
	}
}

func TestActivateWithNothingStagedOnlyRestarts(t *testing.T) {
	f := &agentFake{}
	if err := activate(t, f, Staged{}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || !strings.Contains(f.calls[0].script, "systemctl restart berthy") || f.ran("mv -f") {
		t.Fatalf("scripts = %q", f.scripts())
	}
}

func TestActivateRollsBackWhenTheAgentDoesNotAnswer(t *testing.T) {
	f := &agentFake{prevBin: true, prevCfg: true, waits: []int{1, 0}, journal: "key must be an owned regular 0600 file\nberthy.service: Failed with result 'exit-code'.\n"}
	err := activate(t, f, Staged{Binary: true, Config: true})
	var ae *ActivateError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v", err)
	}
	if !strings.HasPrefix(ae.Message, "the new agent did not come up; the previous one was put back: ") || !strings.Contains(ae.Message, "key must be an owned regular 0600 file") || !strings.Contains(ae.Message, " | ") {
		t.Errorf("message = %q", ae.Message)
	}
	// The log lines come without the timestamp and host: -o cat.
	if strings.Contains(ae.Message, "Nov 16") {
		t.Errorf("the message carries journal prefixes: %q", ae.Message)
	}
	var journalScript string
	for _, sc := range f.scripts() {
		if strings.HasPrefix(sc, "journalctl") {
			journalScript = sc
		}
	}
	if journalScript != "journalctl -u berthy -n 20 --no-pager -o cat 2>&1" {
		t.Errorf("journal command = %q", journalScript)
	}
	if ae.ConfigOutcome != ConfigPrevious {
		t.Errorf("the previous configuration is back, but outcome is %s", ae.ConfigOutcome)
	}
	var order []string
	for _, sc := range f.scripts() {
		switch {
		case strings.Contains(sc, "cp -p"):
			order = append(order, "swap")
		case strings.HasPrefix(sc, "journalctl -u berthy -n 20 --no-pager"):
			order = append(order, "journal")
		case strings.Contains(sc, ".prev") && strings.Contains(sc, "mv -f"):
			order = append(order, "back")
		case strings.Contains(sc, "systemctl restart"):
			order = append(order, "restart")
		default:
			order = append(order, "other")
		}
	}
	if strings.Join(order, " ") != "swap restart journal back restart" {
		t.Fatalf("order = %v", order)
	}
	back := f.calls[3].script
	if !strings.Contains(back, "mv -f /usr/local/bin/berthy.prev /usr/local/bin/berthy") || !strings.Contains(back, "mv -f /etc/berthy/agent.json.prev /etc/berthy/agent.json") {
		t.Errorf("rollback = %q", back)
	}
	if f.ran("systemctl stop") {
		t.Errorf("the service was stopped although the previous agent was put back")
	}
}

func TestActivateOnlyPutsBackWhatItStaged(t *testing.T) {
	f := &agentFake{prevBin: true, prevCfg: true, waits: []int{1, 0}}
	err := activate(t, f, Staged{Binary: true})
	if err == nil {
		t.Fatal("no error")
	}
	for _, c := range f.calls {
		if strings.Contains(c.script, "agent.json") {
			t.Errorf("the configuration was touched: %q", c.script)
		}
	}
}

func TestActivateRollbackThatDoesNotAnswerEither(t *testing.T) {
	f := &agentFake{prevBin: true, waits: []int{1, 1}}
	err := activate(t, f, Staged{Binary: true})
	if err == nil || !strings.Contains(err.Error(), "the previous one was put back, but it does not answer either") {
		t.Fatalf("err = %v", err)
	}
}

func TestActivateOnAFirstInstallLeavesTheAgentStopped(t *testing.T) {
	f := &agentFake{waits: []int{1}, journal: "invalid listen address\n"}
	err := activate(t, f, Staged{Binary: true, Config: true})
	var ae *ActivateError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v", err)
	}
	if ae.Message != "the agent did not come up and is left stopped: invalid listen address" || ae.ConfigOutcome != ConfigRetained {
		t.Errorf("error = %+v", ae)
	}
	if !f.ran("systemctl stop berthy") || f.restarts != 1 {
		t.Errorf("scripts = %q", f.scripts())
	}
}

func TestFailedFirstInstallStillPrintsTheTokenOfTheLiveConfiguration(t *testing.T) {
	exe := elfFile(t, emX86)
	f := freshAgentServer(t)
	f.waits = []int{1}
	f.journal = "boom\n"
	s := agentSession(t, f, exe, false, nil)
	got, ok := runSteps(s)
	r := lastResult(got)
	if ok || r.Step != "service" || !strings.Contains(r.Detail, "left stopped") || !strings.Contains(r.Detail, "boom") {
		t.Fatalf("got %+v", got)
	}
	if !s.TokenLive || s.Token == "" {
		t.Errorf("the token's configuration is live, but TokenLive = %v", s.TokenLive)
	}
	assertNoToken(t, f, got, s.Token)
}

func TestFailedUpgradeRestoresTheConfigurationSoTheTokenIsNotLive(t *testing.T) {
	exe := elfFile(t, emX86)
	f := setUpAgent(t, exe)
	f.prevCfg, f.prevBin = true, true
	f.waits = []int{1, 0}
	s := agentSession(t, f, exe, false, func(o *Options) { o.NewToken = true })
	got, ok := runSteps(s)
	r := lastResult(got)
	if ok || r.Step != "service" || !strings.Contains(r.Detail, "the previous one was put back") {
		t.Fatalf("got %+v", got)
	}
	if s.TokenLive {
		t.Errorf("the token is live although the configuration was put back")
	}
	if got := statuses(got); strings.Contains(got, "cli_config") || strings.Contains(got, "verify") {
		t.Errorf("steps ran after the failed service: %s", got)
	}
}

func TestCLIConfigStep(t *testing.T) {
	exe := elfFile(t, emX86)
	run := func(t *testing.T, path string, dry bool) StepResult {
		t.Helper()
		f := setUpAgent(t, exe)
		s := agentSession(t, f, exe, dry, func(o *Options) { o.CLIConfig = path })
		var got []StepResult
		Run(context.Background(), s, []Step{{"connect", connectStep}, {"check", checkStep}, {"cli_config", cliConfigStep}}, func(r StepResult) { got = append(got, r) })
		return stepOf(t, got, "cli_config")
	}
	t.Run("created private", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sub-cli.json")
		r := run(t, path, false)
		if r.Status != StatusChanged || strings.Contains(r.Detail, "bty.") {
			t.Fatalf("cli_config = %+v", r)
		}
		c, err := agentclient.ParseConfig(mustRead(t, path))
		if err != nil || c.Projects["shop"].Environments["p5"] != (agentclient.Environment{URL: "http://" + listenIP + ":7710", TokenEnv: "BERTHY_SHOP_P5_TOKEN"}) {
			t.Fatalf("config = %+v %v", c, err)
		}
		if st, _ := os.Stat(path); !modeIs(st, 0o600) {
			t.Errorf("mode = %v", st.Mode())
		}
		if run(t, path, false).Status != StatusOK {
			t.Errorf("a second run changed the file")
		}
	})
	t.Run("keeps the other environments and the chosen variable", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cli.json")
		writeCLIConfig(t, path, `{"projects":{"shop":{"environments":{"prod":{"url":"http://10.0.0.1:7710","token_env":"PROD_T"},"p5":{"url":"http://10.9.9.9:1","token_env":"MINE"}}}}}`)
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		r := run(t, path, false)
		if r.Status != StatusChanged {
			t.Fatalf("cli_config = %+v", r)
		}
		c, err := agentclient.ParseConfig(mustRead(t, path))
		if err != nil {
			t.Fatal(err)
		}
		if c.Projects["shop"].Environments["prod"] != (agentclient.Environment{URL: "http://10.0.0.1:7710", TokenEnv: "PROD_T"}) ||
			c.Projects["shop"].Environments["p5"] != (agentclient.Environment{URL: "http://" + listenIP + ":7710", TokenEnv: "MINE"}) {
			t.Fatalf("config = %+v", c)
		}
		if st, _ := os.Stat(path); !modeIs(st, 0o640) {
			t.Errorf("an existing file's mode changed: %v", st.Mode())
		}
	})
	t.Run("a file with unknown fields is refused and left alone", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cli.json")
		writeCLIConfig(t, path, `{"projects":{},"extra":1}`)
		r := run(t, path, false)
		if r.Status != StatusFailed || !strings.Contains(r.Detail, "not valid") || string(mustRead(t, path)) != `{"projects":{},"extra":1}` {
			t.Fatalf("cli_config = %+v", r)
		}
	})
	t.Run("dry run writes nothing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cli.json")
		if r := run(t, path, true); r.Status != StatusWouldChange {
			t.Fatalf("cli_config = %+v", r)
		}
		if _, err := os.Stat(path); err == nil {
			t.Errorf("the file was written")
		}
	})
	t.Run("without --config it writes the default file", func(t *testing.T) {
		r := run(t, "", false)
		dir, err := os.UserConfigDir()
		if err != nil {
			t.Fatal(err)
		}
		c, err := agentclient.ParseConfig(mustRead(t, filepath.Join(dir, "berthy", "config.json")))
		if r.Status != StatusChanged || err != nil || c.Projects["shop"].Environments["p5"] != (agentclient.Environment{URL: "http://" + listenIP + ":7710", TokenEnv: "BERTHY_SHOP_P5_TOKEN"}) {
			t.Fatalf("cli_config = %+v, %v", r, err)
		}
	})
	t.Run("without a default location it is skipped and says what to add", func(t *testing.T) {
		f := setUpAgent(t, exe)
		s := agentSession(t, f, exe, false, nil)
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("HOME", "")
		t.Setenv("AppData", "")
		var got []StepResult
		Run(context.Background(), s, []Step{{"connect", connectStep}, {"check", checkStep}, {"cli_config", cliConfigStep}}, func(r StepResult) { got = append(got, r) })
		r := stepOf(t, got, "cli_config")
		if want := `add {"url": "http://` + listenIP + `:7710", "token_env": "BERTHY_SHOP_P5_TOKEN"} as projects.shop.environments.p5 of the CLI configuration: there is no default location, so give --config or set BERTHY_CONFIG`; r.Status != StatusSkipped || r.Detail != want {
			t.Fatalf("cli_config = %+v", r)
		}
	})
}

func TestCLIConfigStepWritesTheEnvironmentUnderItsProject(t *testing.T) {
	exe := elfFile(t, emX86)
	path := filepath.Join(t.TempDir(), "cli.json")
	s := agentSession(t, setUpAgent(t, exe), exe, false, func(o *Options) { o.CLIConfig = path })
	var got []StepResult
	Run(context.Background(), s, []Step{{"connect", connectStep}, {"check", checkStep}, {"cli_config", cliConfigStep}}, func(r StepResult) { got = append(got, r) })
	if r := stepOf(t, got, "cli_config"); r.Status != StatusChanged {
		t.Fatalf("cli_config = %+v", r)
	}
	if want := `{"projects":{"shop":{"environments":{"p5":{"url":"http://` + listenIP + `:7710","token_env":"BERTHY_SHOP_P5_TOKEN"}}}}}`; compactJSON(t, mustRead(t, path)) != compactJSON(t, []byte(want)) {
		t.Fatalf("the file is %s, want %s", mustRead(t, path), want)
	}
}

func TestCLIConfigStepKeepsOtherProjects(t *testing.T) {
	exe := elfFile(t, emX86)
	path := filepath.Join(t.TempDir(), "cli.json")
	writeCLIConfig(t, path, `{"projects":{
  "sliceup":{"environments":{"p5":{"url":"http://10.1.1.1:7710","token_env":"SU_P5"},"prod":{"url":"http://10.1.1.1:7710","token_env":"SU_PROD"}}},
  "shop":{"environments":{"dev":{"url":"http://10.2.2.2:7710","token_env":"SHOP_DEV"}}}}}`)
	s := agentSession(t, setUpAgent(t, exe), exe, false, func(o *Options) { o.CLIConfig = path })
	var got []StepResult
	Run(context.Background(), s, []Step{{"connect", connectStep}, {"check", checkStep}, {"cli_config", cliConfigStep}}, func(r StepResult) { got = append(got, r) })
	if r := stepOf(t, got, "cli_config"); r.Status != StatusChanged {
		t.Fatalf("cli_config = %+v", r)
	}
	want := `{"projects":{` +
		`"shop":{"environments":{"dev":{"url":"http://10.2.2.2:7710","token_env":"SHOP_DEV"},"p5":{"url":"http://` + listenIP + `:7710","token_env":"BERTHY_SHOP_P5_TOKEN"}}},` +
		`"sliceup":{"environments":{"p5":{"url":"http://10.1.1.1:7710","token_env":"SU_P5"},"prod":{"url":"http://10.1.1.1:7710","token_env":"SU_PROD"}}}}}`
	if got := compactJSON(t, mustRead(t, path)); got != compactJSON(t, []byte(want)) {
		t.Fatalf("the file is %s, want %s", got, want)
	}
}

func TestCLIConfigStepRefusesTheLayoutOfAnEarlierRelease(t *testing.T) {
	exe := elfFile(t, emX86)
	path := filepath.Join(t.TempDir(), "cli.json")
	const old = `{"environments":{"p5":{"url":"http://10.9.9.9:1","token_env":"MINE"}}}`
	writeCLIConfig(t, path, old)
	s := agentSession(t, setUpAgent(t, exe), exe, false, func(o *Options) { o.CLIConfig = path })
	var got []StepResult
	Run(context.Background(), s, []Step{{"connect", connectStep}, {"check", checkStep}, {"cli_config", cliConfigStep}}, func(r StepResult) { got = append(got, r) })
	r := stepOf(t, got, "cli_config")
	want := "the CLI configuration " + path + " is in the layout of an earlier release: move it aside, or give --config with a new file, then run setup again"
	if r.Status != StatusFailed || r.Detail != want || string(mustRead(t, path)) != old {
		t.Fatalf("cli_config = %+v", r)
	}
}

func TestCLIConfigStepDistinguishesAReadFailureFromInvalidJSON(t *testing.T) {
	exe := elfFile(t, emX86)
	run := func(t *testing.T, path string) StepResult {
		t.Helper()
		s := agentSession(t, setUpAgent(t, exe), exe, false, func(o *Options) { o.CLIConfig = path })
		var got []StepResult
		Run(context.Background(), s, []Step{{"connect", connectStep}, {"check", checkStep}, {"cli_config", cliConfigStep}}, func(r StepResult) { got = append(got, r) })
		return stepOf(t, got, "cli_config")
	}
	dir := t.TempDir()
	r := run(t, dir)
	if want := "cannot read config " + dir; r.Status != StatusFailed || r.Detail != want {
		t.Fatalf("a directory as the file: %+v, want %q", r, want)
	}
	path := filepath.Join(t.TempDir(), "cli.json")
	writeCLIConfig(t, path, `{`)
	r = run(t, path)
	if want := "the CLI configuration is not valid: invalid config"; r.Status != StatusFailed || r.Detail != want {
		t.Fatalf("invalid JSON: %+v, want %q", r, want)
	}
}

// compactJSON is b without its indentation, with the keys in the order encoding/json writes them.
func compactJSON(t *testing.T, b []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestDefaultTokenEnv(t *testing.T) {
	for env, want := range map[string]string{"p5": "BERTHY_SHOP_P5_TOKEN", "my-env": "BERTHY_SHOP_MY_ENV_TOKEN", "prod": "BERTHY_SHOP_PROD_TOKEN"} {
		if got := DefaultTokenEnv("shop", env); got != want {
			t.Errorf("DefaultTokenEnv(%q) = %q, want %q", env, got, want)
		}
	}
}

func doctorBody(checks []api.Check) string {
	b, _ := json.Marshal(map[string]any{"checks": checks})
	return string(b)
}

func TestVerifyStep(t *testing.T) {
	exe := elfFile(t, emX86)
	token := "bty.shop.p5." + strings.Repeat("c", 64)
	withToken := func(o *Options) {
		o.CLIConfig = ""
		o.Getenv = func(n string) string {
			if n == "BERTHY_SHOP_P5_TOKEN" {
				return token
			}
			return ""
		}
	}
	runVerify := func(f *agentFake, mod func(*Options)) (StepResult, *Session) {
		s := agentSession(t, f, exe, false, mod)
		var got []StepResult
		Run(context.Background(), s, []Step{{"connect", connectStep}, {"check", checkStep}, {"cli_config", cliConfigStep}, {"verify", verifyStep}}, func(r StepResult) { got = append(got, r) })
		return stepOf(t, got, "verify"), s
	}
	t.Run("reachable and healthy", func(t *testing.T) {
		f := setUpAgent(t, exe)
		var asked [4]string
		r, _ := runVerify(f, func(o *Options) {
			withToken(o)
			o.Doctor = func(_ context.Context, base, project, env, tok string) ([]api.Check, error) {
				asked = [4]string{base, project, env, tok}
				return append(okChecks(), api.Check{Name: "disk", OK: false, Critical: false, Detail: "91% used"}), nil
			}
		})
		if r.Status != StatusOK || r.Detail != "agent v0.0.1-test, 3 of 4 checks ok (not ok: disk: 91% used)" {
			t.Fatalf("verify = %+v", r)
		}
		if asked != [4]string{"http://" + listenIP + ":7710", "shop", "p5", token} {
			t.Errorf("asked = %v", asked)
		}
		for _, c := range f.calls {
			if strings.Contains(c.script, "-H @-") {
				t.Errorf("the request went through the server")
			}
		}
	})
	t.Run("a critical check that is not ok fails the step", func(t *testing.T) {
		f := setUpAgent(t, exe)
		r, _ := runVerify(f, func(o *Options) {
			withToken(o)
			o.Doctor = func(context.Context, string, string, string, string) ([]api.Check, error) {
				return []api.Check{{Name: "agent", OK: true, Critical: true, Detail: "v1"}, {Name: "swarm", OK: false, Critical: true, Detail: "not a manager"}, {Name: "network", OK: false, Critical: true, Detail: "missing"}}, nil
			}
		})
		if r.Status != StatusFailed || r.Detail != "doctor found 2 critical problems: swarm: not a manager; network: missing" {
			t.Fatalf("verify = %+v", r)
		}
	})
	t.Run("a refused token fails the step", func(t *testing.T) {
		r, _ := runVerify(setUpAgent(t, exe), func(o *Options) {
			withToken(o)
			o.Doctor = func(context.Context, string, string, string, string) ([]api.Check, error) {
				return nil, errors.New("agent returned HTTP 401")
			}
		})
		if r.Status != StatusFailed || r.Detail != "agent returned HTTP 401" {
			t.Fatalf("verify = %+v", r)
		}
	})
	t.Run("unreachable from here: checked on the server with the token on stdin only", func(t *testing.T) {
		f := setUpAgent(t, exe)
		f.probe = with(f.probe, "curl=/usr/bin/curl")
		f.doctorBody = doctorBody(okChecks())
		r, _ := runVerify(f, func(o *Options) {
			withToken(o)
			o.Doctor = func(context.Context, string, string, string, string) ([]api.Check, error) { return nil, ErrUnreachable }
		})
		if r.Status != StatusOK || !strings.HasPrefix(r.Detail, "this machine cannot reach http://"+listenIP+":7710; checked on the server: agent v0.0.1-test, 3 of 3 checks ok") {
			t.Fatalf("verify = %+v", r)
		}
		c, ok := f.find("-H @-")
		if !ok {
			t.Fatal("no request on the server")
		}
		if string(c.stdin) != "Authorization: Bearer "+token+"\n" {
			t.Errorf("stdin = %q", c.stdin)
		}
		if strings.Contains(c.script, token) || !strings.Contains(c.script, "curl -fsS --max-time 30 -H @- 'http://"+listenIP+":7710/v1/projects/shop/environments/p5/doctor'") {
			t.Errorf("script = %q", c.script)
		}
		for _, other := range f.calls {
			if other.script != c.script && strings.Contains(other.script, token) {
				t.Errorf("another script holds the token: %q", other.script)
			}
		}
	})
	t.Run("a failing server-side request is reported without the token", func(t *testing.T) {
		f := setUpAgent(t, exe)
		f.probe = with(f.probe, "curl=/usr/bin/curl")
		f.doctorCode = 22
		r, _ := runVerify(f, func(o *Options) {
			withToken(o)
			o.Doctor = func(context.Context, string, string, string, string) ([]api.Check, error) { return nil, ErrUnreachable }
		})
		if r.Status != StatusFailed || strings.Contains(r.Detail, token) || !strings.Contains(r.Detail, "checking on the server failed") {
			t.Fatalf("verify = %+v", r)
		}
	})
	t.Run("without a token the agent is only asked whether it answers", func(t *testing.T) {
		f := setUpAgent(t, exe)
		r, _ := runVerify(f, func(o *Options) {
			o.Doctor = func(context.Context, string, string, string, string) ([]api.Check, error) {
				panic("no token, no doctor")
			}
		})
		if r.Status != StatusOK || r.Detail != "the agent answers; run berthy doctor with an admin token to check it fully" {
			t.Fatalf("verify = %+v", r)
		}
		if !f.ran("http://" + listenIP + ":7710/healthz") {
			t.Errorf("healthz was not asked")
		}
	})
	t.Run("without a token and without an answer it fails", func(t *testing.T) {
		f := setUpAgent(t, exe)
		f.healthCode = 1
		r, _ := runVerify(f, nil)
		if r.Status != StatusFailed || r.Detail != "the agent does not answer on http://"+listenIP+":7710" {
			t.Fatalf("verify = %+v", r)
		}
	})
	t.Run("the token this run made is used", func(t *testing.T) {
		exe := elfFile(t, emX86)
		f := freshAgentServer(t)
		var used string
		s := agentSession(t, f, exe, false, func(o *Options) {
			o.Doctor = func(_ context.Context, _, _, _, tok string) ([]api.Check, error) { used = tok; return okChecks(), nil }
		})
		if _, ok := runSteps(s); !ok || used != s.Token || used == "" {
			t.Fatalf("used %q, made %q", used, s.Token)
		}
	})
}

func TestHTTPDoctorTellsUnreachableFromRefused(t *testing.T) {
	// An address nothing listens on is unreachable, not an error answer.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	addr := l.Addr().String()
	l.Close()
	_, err = httpDoctor(context.Background(), "http://"+addr, "shop", "p5", "tok")
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v", err)
	}
}

func TestHostKeyFailureSaysWhatToDo(t *testing.T) {
	r := &hostKeyRunner{}
	_, err := Connect(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "Host key verification failed") ||
		!strings.Contains(err.Error(), "connect once with ssh ops@s1 to check and accept the server's host key") {
		t.Fatalf("err = %v", err)
	}
	// Other failures get no hint.
	_, err = Connect(context.Background(), &deadServer{})
	if err == nil || strings.Contains(err.Error(), "connect once") {
		t.Fatalf("err = %v", err)
	}
}

type hostKeyRunner struct{}

func (hostKeyRunner) Target() string { return "ops@s1" }
func (hostKeyRunner) Run(context.Context, string, io.Reader) (Result, error) {
	return Result{Stderr: []byte("No ED25519 host key is known for s1.\nHost key verification failed.\n"), Code: 255}, nil
}

func TestParseFactsOfTheAgent(t *testing.T) {
	f := ParseFacts(strings.Join([]string{
		"bin_sha=abc", "user=998", "user_groups=berthy docker", "unit_sha=def", "service_active=active", "service_enabled=enabled",
		"key_sha=123", "key_stat=32 berthy:berthy 600", "data_dir_stat=berthy:berthy 700", "state_dir_stat=berthy:berthy 700",
		"state_db=yes", "state_foreign=yes", "agent_config=yes", "agent_config_stat=berthy:berthy 600",
	}, "\n"))
	if f.BinSHA != "abc" || !f.UserExists || len(f.UserGroups) != 2 || f.UnitSHA != "def" || !f.ServiceActive || !f.ServiceEnabled ||
		f.DataDirStat != wantPrivateDir || !f.AgentConfig || f.AgentConfigStat != wantPrivate {
		t.Fatalf("facts = %+v", f)
	}
	empty := ParseFacts("user=\nuser_groups=\nservice_active=inactive\nservice_enabled=disabled\n")
	if empty.UserExists || empty.ServiceActive || empty.ServiceEnabled {
		t.Fatalf("empty = %+v", empty)
	}
	server := &agentFake{probe: []string{"key_sha=123", "key_stat=32 berthy:berthy 600", "state_dir_stat=berthy:berthy 700", "state_db=yes", "state_foreign=yes"}}
	paths, err := probePaths(context.Background(), server, stateDir, keyPath)
	if err != nil || !paths.KeyExists || paths.KeySHA != "123" || paths.KeyStat != "32 "+wantPrivate || paths.StateDirStat != wantPrivateDir || !paths.StateDB || !paths.StateForeign {
		t.Fatalf("path facts = %+v, %v", paths, err)
	}
}

func TestProbeOfTheAgentOnlyReads(t *testing.T) {
	f := newServer(nil)
	if _, err := Probe(context.Background(), f, "n"); err != nil {
		t.Fatal(err)
	}
	if _, err := probePaths(context.Background(), f, stateDir, keyPath); err != nil {
		t.Fatal(err)
	}
	script := f.scripts[0] + f.scripts[1]
	for _, bad := range []string{"mktemp", "chown", "chmod", "mv ", "rm ", "useradd", "cat /var/lib/berthy/key", "cat \"$k\"", "cat /etc/berthy"} {
		if strings.Contains(script, bad) {
			t.Errorf("the probe contains %q", bad)
		}
	}
	for _, want := range []string{"sha256sum", "id -nG berthy", "systemctl is-active berthy", "state.db", "key_sha"} {
		if !strings.Contains(script, want) {
			t.Errorf("the probe lacks %q", want)
		}
	}
	// The state may be restored before setup creates the user: files must count as foreign then, not be skipped because
	// find does not know the user's name.
	if strings.Contains(script, "! -user berthy") || !strings.Contains(script, `id -u berthy`) || !strings.Contains(script, "-mindepth 1") {
		t.Errorf("state ownership is probed by a user name find may not know:\n%s", script)
	}
	if strings.Contains(script, "%!") {
		t.Errorf("a format verb leaked into the probe:\n%s", script)
	}
}
