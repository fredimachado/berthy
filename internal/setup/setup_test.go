package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"

	"berthy/internal/sshhost"
)

// Markers of the scripts that change a server. The probe and the connect scripts contain none of them.
var changeMarkers = []string{"get.docker.com", "systemctl start docker", "systemctl enable docker", "swarm init", "network create"}

func isChange(script string) bool {
	for _, m := range changeMarkers {
		if strings.Contains(script, m) {
			return true
		}
	}
	return false
}

// fakeServer is a Runner that answers by the marker a script contains, and a server whose state the tests set.
type fakeServer struct {
	uid, user   string
	sudoOK      bool
	sudoSet     bool
	probe       []string // the probe's output lines, as the server would print them
	scripts     []string
	failOn      string // a marker whose script exits 1
	failMessage string
	installTail string
}

func (f *fakeServer) Target() string { return "ops@s1" }
func (f *fakeServer) SetSudo(v bool) { f.sudoSet = v }

func (f *fakeServer) Run(ctx context.Context, script string, stdin io.Reader) (Result, error) {
	f.scripts = append(f.scripts, script)
	switch {
	case script == "id -u; id -un":
		return Result{Stdout: []byte(f.uid + "\n" + f.user + "\n")}, nil
	case script == "sudo -n true":
		if f.sudoOK {
			return Result{}, nil
		}
		return Result{Stderr: []byte("sudo: a password is required\n"), Code: 1}, nil
	case strings.Contains(script, "echo \"os=$(uname"):
		return Result{Stdout: []byte(strings.Join(f.probe, "\n") + "\n")}, nil
	}
	if f.failOn != "" && strings.Contains(script, f.failOn) {
		return Result{Stdout: []byte(f.installTail), Stderr: []byte(f.failMessage), Code: 1}, nil
	}
	return Result{}, nil
}

func (f *fakeServer) changes() []string {
	var out []string
	for _, s := range f.scripts {
		if isChange(s) {
			out = append(out, s)
		}
	}
	return out
}

func (f *fakeServer) ranAny(markers ...string) bool {
	for _, s := range f.scripts {
		for _, m := range markers {
			if strings.Contains(s, m) {
				return true
			}
		}
	}
	return false
}

// dockerSteps are the steps that make the server ready for the agent: the ones before the agent's own.
func dockerSteps() []Step { return Steps()[:5] }

const listenIP = "172.31.250.10"

func base() []string {
	return []string{"os=Linux", "arch=x86_64", "systemd=yes", "addr=127.0.0.1", "addr=" + listenIP, "curl=/usr/bin/curl"}
}

func with(lines []string, more ...string) []string {
	return append(append([]string(nil), lines...), more...)
}

func dockerUp(version string) []string {
	return []string{"docker=/usr/bin/docker", "docker_client_version=" + version, "docker_active=active", "docker_enabled=enabled", "docker_version=" + version}
}

func setUpServer() []string {
	return with(base(), with(dockerUp("29.9.3"), "swarm=active true", "network=overlay swarm true p5net")...)
}

func newSession(f *fakeServer, dry bool) *Session {
	return &Session{R: f, DryRun: dry, Opts: Options{Project: "shop", Env: "p5", Network: "p5net", ListenIP: net.ParseIP(listenIP), ListenPort: 7710}}
}

func runAll(t *testing.T, f *fakeServer, dry bool) ([]StepResult, bool) {
	t.Helper()
	var got []StepResult
	ok := Run(context.Background(), newSession(f, dry), dockerSteps(), func(r StepResult) { got = append(got, r) })
	return got, ok
}

func statuses(rs []StepResult) string {
	var parts []string
	for _, r := range rs {
		parts = append(parts, r.Step+"="+r.Status)
	}
	return strings.Join(parts, " ")
}

func lastResult(rs []StepResult) StepResult { return rs[len(rs)-1] }

func newServer(probe []string) *fakeServer {
	return &fakeServer{uid: "1000", user: "ops", sudoOK: true, probe: probe}
}

func TestFreshServerInstallsDockerSwarmAndNetwork(t *testing.T) {
	f := newServer(base())
	// The probe is read again after the install and after the swarm init.
	g := &sequenceServer{fakeServer: f, outputs: [][]string{
		base(),
		with(base(), with(dockerUp("29.9.3"), "swarm=inactive false")...),
		with(base(), with(dockerUp("29.9.3"), "swarm=active true")...),
	}}
	var got []StepResult
	ok := Run(context.Background(), newSession(f, false).withRunner(g), dockerSteps(), func(r StepResult) { got = append(got, r) })
	if !ok {
		t.Fatalf("setup failed: %+v", got)
	}
	if want := "connect=ok check=ok docker=changed swarm=changed network=changed"; statuses(got) != want {
		t.Fatalf("statuses = %s, want %s", statuses(got), want)
	}
	if got[2].Detail != "installed 29.9.3 with the official script" {
		t.Fatalf("docker detail = %q", got[2].Detail)
	}
	var order []string
	for _, s := range g.scripts {
		switch {
		case strings.Contains(s, "get.docker.com"):
			order = append(order, "install")
		case strings.Contains(s, "swarm init"):
			order = append(order, "swarm")
		case strings.Contains(s, "network create"):
			order = append(order, "network")
		}
	}
	if !reflect.DeepEqual(order, []string{"install", "swarm", "network"}) {
		t.Fatalf("change order = %v", order)
	}
	install := g.changes()[0]
	for _, want := range []string{"curl -fsSL https://get.docker.com -o \"$tmp\"", "sh \"$tmp\"", "rm -f", "systemctl enable --now docker"} {
		if !strings.Contains(install, want) {
			t.Errorf("install script lacks %q:\n%s", want, install)
		}
	}
	swarm := g.changes()[1]
	if !strings.Contains(swarm, "--advertise-addr '"+listenIP+"'") || !strings.Contains(swarm, "--listen-addr '"+listenIP+":2377'") {
		t.Errorf("swarm init = %q", swarm)
	}
	if n := g.changes()[2]; !strings.Contains(n, "docker network create --driver overlay --attachable 'p5net'") {
		t.Errorf("network create = %q", n)
	}
	if !g.sudoSet {
		t.Errorf("sudo was not turned on for a login that is not root")
	}
}

// sequenceServer answers each probe with the next output of its list (the last one repeats).
type sequenceServer struct {
	*fakeServer
	outputs [][]string
	probes  int
}

func (s *sequenceServer) Run(ctx context.Context, script string, stdin io.Reader) (Result, error) {
	if strings.Contains(script, "echo \"os=$(uname") {
		s.scripts = append(s.scripts, script)
		i := s.probes
		if i >= len(s.outputs) {
			i = len(s.outputs) - 1
		}
		s.probes++
		return Result{Stdout: []byte(strings.Join(s.outputs[i], "\n") + "\n")}, nil
	}
	return s.fakeServer.Run(ctx, script, stdin)
}

func (s *Session) withRunner(r Runner) *Session { s.R = r; return s }

func TestSetUpServerChangesNothing(t *testing.T) {
	f := newServer(setUpServer())
	got, ok := runAll(t, f, false)
	if !ok {
		t.Fatalf("failed: %+v", got)
	}
	if want := "connect=ok check=ok docker=ok swarm=ok network=ok"; statuses(got) != want {
		t.Fatalf("statuses = %s", statuses(got))
	}
	if c := f.changes(); len(c) != 0 {
		t.Fatalf("change scripts sent to a set-up server: %v", c)
	}
	probes := 0
	for _, s := range f.scripts {
		if strings.Contains(s, "uname -s") {
			probes++
		}
	}
	if probes != 1 {
		t.Fatalf("the server was probed %d times, want 1", probes)
	}
	want := []string{
		"ops@s1 (sudo)", "linux amd64, systemd, 172.31.250.10 assigned", "Docker 29.9.3, running, enabled", "manager",
		"p5net (attachable overlay)",
	}
	for i, r := range got {
		if r.Detail != want[i] {
			t.Errorf("%s detail = %q, want %q", r.Step, r.Detail, want[i])
		}
	}
}

func TestRootLoginUsesNoSudo(t *testing.T) {
	f := &fakeServer{uid: "0", user: "root", probe: setUpServer()}
	got, ok := runAll(t, f, false)
	if !ok || got[0].Detail != "ops@s1" {
		t.Fatalf("ok=%v connect = %+v", ok, got[0])
	}
	if f.sudoSet || f.ranAny("sudo -n true") {
		t.Fatalf("root login asked for sudo")
	}
}

func TestBareHostNamesTheUser(t *testing.T) {
	f := &bareHost{fakeServer: newServer(setUpServer())}
	var got []StepResult
	Run(context.Background(), newSession(f.fakeServer, false).withRunner(f), Steps()[:1], func(r StepResult) { got = append(got, r) })
	if got[0].Detail != "ops@s1 (sudo)" {
		t.Fatalf("detail = %q", got[0].Detail)
	}
}

type bareHost struct{ *fakeServer }

func (b *bareHost) Target() string { return "s1" }

func TestSudoRefusedFailsConnect(t *testing.T) {
	f := newServer(setUpServer())
	f.sudoOK = false
	got, ok := runAll(t, f, false)
	if ok || lastResult(got).Step != "connect" || lastResult(got).Status != StatusFailed {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
	if want := "the account must be root or able to run sudo without a password"; lastResult(got).Detail != want {
		t.Fatalf("detail = %q", lastResult(got).Detail)
	}
	if len(got) != 1 {
		t.Fatalf("steps ran after a failed connect: %+v", got)
	}
}

type deadServer struct{ fakeServer }

func (d *deadServer) Run(ctx context.Context, script string, stdin io.Reader) (Result, error) {
	return Result{Stderr: []byte("ssh: connect to host s1 port 22: Connection refused\n"), Code: 255}, nil
}

func TestConnectFailureNamesTheCause(t *testing.T) {
	var got []StepResult
	Run(context.Background(), newSession(&fakeServer{}, false).withRunner(&deadServer{}), dockerSteps(), func(r StepResult) { got = append(got, r) })
	if len(got) != 1 || got[0].Status != StatusFailed || !strings.Contains(got[0].Detail, "Connection refused") {
		t.Fatalf("got %+v", got)
	}
}

func TestRunnerErrorFailsTheStep(t *testing.T) {
	r := &erroring{err: errors.New("boom")}
	var got []StepResult
	Run(context.Background(), newSession(&fakeServer{}, false).withRunner(r), dockerSteps(), func(x StepResult) { got = append(got, x) })
	if len(got) != 1 || got[0].Step != "connect" || got[0].Status != StatusFailed || !strings.Contains(got[0].Detail, "boom") {
		t.Fatalf("got %+v", got)
	}
}

type erroring struct{ err error }

func (e *erroring) Target() string { return "s1" }
func (e *erroring) Run(context.Context, string, io.Reader) (Result, error) {
	return Result{}, e.err
}

func TestStepFailures(t *testing.T) {
	tests := []struct {
		name   string
		probe  []string
		opts   func(*Options)
		step   string
		detail string
	}{
		{"docker 24", with(base(), with(dockerUp("24.0.7"), "swarm=inactive false")...), nil, "docker", "Docker 24.0.7 is older than the minimum 25"},
		{"docker 24 stopped, client version", with(base(), "docker=/usr/bin/docker", "docker_client_version=24.0.7", "docker_active=inactive", "docker_enabled=enabled"), nil, "docker", "Docker 24.0.7 is older than the minimum 25"},
		{"no docker version", with(base(), "docker=/usr/bin/docker", "docker_active=active", "docker_enabled=enabled"), nil, "docker", "could not read the version of Docker"},
		{"worker", with(base(), with(dockerUp("29.9.3"), "swarm=active false")...), nil, "swarm", "this server is a worker in another Swarm"},
		{"locked swarm", with(base(), with(dockerUp("29.9.3"), "swarm=locked false")...), nil, "swarm", "the Swarm state is locked"},
		{"pending swarm", with(base(), with(dockerUp("29.9.3"), "swarm=pending false")...), nil, "swarm", "the Swarm state is pending"},
		{"bridge network", with(base(), with(dockerUp("29.9.3"), "swarm=active true", "network=bridge local false p5net")...), nil, "network", "network p5net exists but is not an attachable overlay"},
		{"overlay not attachable", with(base(), with(dockerUp("29.9.3"), "swarm=active true", "network=overlay swarm false p5net")...), nil, "network", "network p5net exists but is not an attachable overlay"},
		{"network error", with(base(), with(dockerUp("29.9.3"), "swarm=active true", "network_error=Error response from daemon: multiple matches")...), nil, "network", "could not inspect network p5net: Error response from daemon: multiple matches"},
		{"listen address not assigned", setUpServer(), func(o *Options) { o.ListenIP = net.ParseIP("172.31.250.99") }, "check", "172.31.250.99 is not assigned to this server: --listen needs one of its addresses"},
		{"not linux", with(setUpServer()[1:], "os=Darwin"), nil, "check", "this server runs Darwin, not Linux"},
		{"no systemd", append([]string{"os=Linux", "arch=x86_64", "systemd=", "addr=" + listenIP}, dockerUp("29.9.3")...), nil, "check", "systemd is not running on this server"},
		{"arm32", append([]string{"os=Linux", "arch=armv7l", "systemd=yes", "addr=" + listenIP}, dockerUp("29.9.3")...), nil, "check", "unsupported CPU architecture armv7l: Berthy supports x86_64 and aarch64"},
		{"no curl or wget", []string{"os=Linux", "arch=x86_64", "systemd=yes", "addr=" + listenIP}, nil, "docker", "Docker is not installed, and neither curl nor wget is available to download its install script"},
		{"no-install-docker", base(), func(o *Options) { o.NoInstallDocker = true }, "docker", "Docker is not installed (--no-install-docker)"},
	}
	for _, tc := range tests {
		for _, dry := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s dry=%v", tc.name, dry), func(t *testing.T) {
				f := newServer(tc.probe)
				s := newSession(f, dry)
				if tc.opts != nil {
					tc.opts(&s.Opts)
				}
				var got []StepResult
				ok := Run(context.Background(), s, dockerSteps(), func(r StepResult) { got = append(got, r) })
				last := lastResult(got)
				if ok || last.Step != tc.step || last.Status != StatusFailed || last.Detail != tc.detail {
					t.Fatalf("got %+v ok=%v, want %s failing with %q", got, ok, tc.step, tc.detail)
				}
				if c := f.changes(); len(c) != 0 {
					t.Fatalf("changes sent: %v", c)
				}
			})
		}
	}
}

func TestDockerStoppedAndDisabledIsStartedAndEnabled(t *testing.T) {
	stopped := with(base(), "docker=/usr/bin/docker", "docker_client_version=29.9.3", "docker_active=inactive", "docker_enabled=disabled")
	f := newServer(stopped)
	g := &sequenceServer{fakeServer: f, outputs: [][]string{stopped, with(base(), with(dockerUp("29.9.3"), "swarm=active true", "network=overlay swarm true p5net")...)}}
	var got []StepResult
	ok := Run(context.Background(), newSession(f, false).withRunner(g), dockerSteps(), func(r StepResult) { got = append(got, r) })
	if !ok {
		t.Fatalf("failed: %+v", got)
	}
	if got[2].Status != StatusChanged || got[2].Detail != "started and enabled Docker 29.9.3" {
		t.Fatalf("docker = %+v", got[2])
	}
	c := g.changes()
	if len(c) != 2 || c[0] != "systemctl start docker" || c[1] != "systemctl enable docker" {
		t.Fatalf("changes = %q", c)
	}
	if got[3].Status != StatusOK || got[4].Status != StatusOK {
		t.Fatalf("swarm and network should be read after the start: %+v", got)
	}
}

func TestDockerEnabledOnly(t *testing.T) {
	probe := with(base(), "docker=/usr/bin/docker", "docker_client_version=29.9.3", "docker_active=active", "docker_enabled=disabled", "docker_version=29.9.3", "swarm=active true", "network=overlay swarm true p5net")
	f := newServer(probe)
	got, ok := runAll(t, f, false)
	if !ok || got[2].Detail != "enabled Docker 29.9.3" {
		t.Fatalf("got %+v", got)
	}
	if c := f.changes(); len(c) != 1 || c[0] != "systemctl enable docker" {
		t.Fatalf("changes = %q", c)
	}
}

func TestDockerThatDoesNotStartFails(t *testing.T) {
	stopped := with(base(), "docker=/usr/bin/docker", "docker_client_version=29.9.3", "docker_active=inactive", "docker_enabled=enabled")
	f := newServer(stopped)
	got, ok := runAll(t, f, false)
	if ok || lastResult(got).Step != "docker" || lastResult(got).Detail != "Docker did not start" {
		t.Fatalf("got %+v", got)
	}
}

func TestInstallFailureShowsTheLastLines(t *testing.T) {
	f := newServer(base())
	f.failOn = "get.docker.com"
	var tail []string
	for i := 1; i <= 30; i++ {
		tail = append(tail, fmt.Sprintf("line %d", i))
	}
	f.installTail = strings.Join(tail, "\n")
	f.failMessage = "E: unable to locate package\n"
	got, ok := runAll(t, f, false)
	last := lastResult(got)
	if ok || last.Step != "docker" || last.Status != StatusFailed {
		t.Fatalf("got %+v", got)
	}
	if strings.Contains(last.Detail, "line 11 ") || !strings.Contains(last.Detail, "line 12 |") || !strings.Contains(last.Detail, "line 30") || !strings.Contains(last.Detail, "unable to locate package") {
		t.Fatalf("detail = %q", last.Detail)
	}
}

func TestInstallUsesWgetWithoutCurl(t *testing.T) {
	probe := []string{"os=Linux", "arch=aarch64", "systemd=yes", "addr=" + listenIP, "wget=/usr/bin/wget"}
	f := newServer(probe)
	g := &sequenceServer{fakeServer: f, outputs: [][]string{probe, with(base(), with(dockerUp("29.9.3"), "swarm=active true", "network=overlay swarm true p5net")...)}}
	got, _ := func() ([]StepResult, bool) {
		var got []StepResult
		ok := Run(context.Background(), newSession(f, false).withRunner(g), dockerSteps(), func(r StepResult) { got = append(got, r) })
		return got, ok
	}()
	if got[1].Detail != "linux arm64, systemd, 172.31.250.10 assigned" {
		t.Fatalf("check = %+v", got[1])
	}
	if c := g.changes(); len(c) == 0 || !strings.Contains(c[0], `wget -qO "$tmp" https://get.docker.com`) || strings.Contains(c[0], "curl") {
		t.Fatalf("changes = %q", c)
	}
}

func TestDryRunOfFreshServerSendsNoChange(t *testing.T) {
	f := newServer(base())
	got, ok := runAll(t, f, true)
	if !ok {
		t.Fatalf("failed: %+v", got)
	}
	if want := "connect=ok check=ok docker=would_change swarm=would_change network=would_change"; statuses(got) != want {
		t.Fatalf("statuses = %s", statuses(got))
	}
	for i, want := range map[int]string{
		2: "would install Docker with the official script",
		3: "would turn on Swarm advertising 172.31.250.10",
		4: "would create network p5net",
	} {
		if got[i].Detail != want {
			t.Errorf("%s detail = %q, want %q", got[i].Step, got[i].Detail, want)
		}
	}
	assertOnlyReads(t, f)
}

func assertOnlyReads(t *testing.T, f *fakeServer) {
	t.Helper()
	for _, s := range f.scripts {
		switch {
		case s == "id -u; id -un", s == "sudo -n true", strings.Contains(s, "uname -s"):
		default:
			t.Errorf("a dry run sent %q", s)
		}
		if isChange(s) || strings.Contains(s, "mktemp") || strings.Contains(s, "touch ") || strings.Contains(s, " > ") {
			t.Errorf("a dry run sent a script that may write: %q", s)
		}
	}
}

func TestDryRunOfStoppedDockerDoesNotStartIt(t *testing.T) {
	stopped := with(base(), "docker=/usr/bin/docker", "docker_client_version=29.9.3", "docker_active=inactive", "docker_enabled=disabled")
	f := newServer(stopped)
	got, ok := runAll(t, f, true)
	if !ok {
		t.Fatalf("failed: %+v", got)
	}
	if want := "connect=ok check=ok docker=would_change swarm=skipped network=skipped"; statuses(got) != want {
		t.Fatalf("statuses = %s", statuses(got))
	}
	if got[2].Detail != "would start and enable Docker" {
		t.Fatalf("docker = %q", got[2].Detail)
	}
	assertOnlyReads(t, f)
}

func TestDryRunOfPartlySetUpServer(t *testing.T) {
	f := newServer(with(base(), with(dockerUp("29.9.3"), "swarm=inactive false")...))
	got, ok := runAll(t, f, true)
	if !ok || statuses(got) != "connect=ok check=ok docker=ok swarm=would_change network=would_change" {
		t.Fatalf("got %s ok=%v", statuses(got), ok)
	}
	assertOnlyReads(t, f)
}

func TestDryRunOfSetUpServerIsAllOK(t *testing.T) {
	f := newServer(setUpServer())
	got, _ := runAll(t, f, true)
	if statuses(got) != "connect=ok check=ok docker=ok swarm=ok network=ok" {
		t.Fatalf("got %s", statuses(got))
	}
	assertOnlyReads(t, f)
}

func TestRunStopsAtTheFirstFailure(t *testing.T) {
	ran := 0
	steps := []Step{
		{"a", func(context.Context, *Session) (string, string, error) { ran++; return StatusOK, "", nil }},
		{"b", func(context.Context, *Session) (string, string, error) {
			ran++
			return "", "", errors.New("no\x1b[31m")
		}},
		{"c", func(context.Context, *Session) (string, string, error) { ran++; return StatusOK, "", nil }},
	}
	var got []StepResult
	if Run(context.Background(), &Session{}, steps, func(r StepResult) { got = append(got, r) }) || ran != 2 {
		t.Fatalf("ran %d steps", ran)
	}
	if got[1].Status != StatusFailed || strings.ContainsRune(got[1].Detail, 0x1b) {
		t.Fatalf("got %+v", got)
	}
}

func TestParseFacts(t *testing.T) {
	f := ParseFacts("os = Linux \r\narch=x86_64\nsystemd=yes\naddr=10.0.0.1\naddr=fd00::1\naddr=\ncurl=/usr/bin/curl\n" +
		"junk line\nunknown=1\ndocker=/usr/bin/docker\ndocker_version= 29.9.3 \ndocker_active=active\ndocker_enabled=enabled\n" +
		"swarm=active   true\nnetwork=overlay  swarm   true\nagent_env=p5\n=novalue\n")
	want := &Facts{OS: "Linux", Arch: "x86_64", Systemd: true, Addrs: []string{"10.0.0.1", "fd00::1"}, Curl: true, Docker: true,
		DockerVersion: "29.9.3", DockerActive: true, DockerEnabled: true, SwarmState: "active", SwarmManager: true,
		NetworkFound: true, NetworkDriver: "overlay", NetworkScope: "swarm", NetworkAttachable: true}
	if !reflect.DeepEqual(f, want) {
		t.Fatalf("got %+v\nwant %+v", f, want)
	}
}

func TestParseFactsOfMissingKeys(t *testing.T) {
	for _, in := range []string{"", "\n\n", "swarm=\nnetwork=\ndocker_active=\n", "network=overlay\nswarm=active"} {
		f := ParseFacts(in)
		if f.NetworkFound || f.Docker || f.Systemd || f.DockerActive || len(f.Addrs) != 0 {
			t.Errorf("ParseFacts(%q) = %+v", in, f)
		}
	}
	if f := ParseFacts("swarm=active"); f.SwarmState != "active" || f.SwarmManager {
		t.Errorf("one-word swarm = %+v", f)
	}
}

func TestProbeScript(t *testing.T) {
	f := newServer(nil)
	if _, err := Probe(context.Background(), f, "it's;$(x)"); err != nil {
		t.Fatal(err)
	}
	script := f.scripts[0]
	if !strings.Contains(script, `'it'\''s;$(x)'`) {
		t.Errorf("the network name is not quoted in:\n%s", script)
	}
	for _, bad := range []string{"swarm init", "network create", "mktemp", "systemctl start", "systemctl enable", "get.docker.com", "cat /etc/berthy"} {
		if strings.Contains(script, bad) {
			t.Errorf("the probe contains %q", bad)
		}
	}
	// Docker is asked only after systemd says it runs.
	if strings.Index(script, "docker version") < strings.Index(script, `[ "$active" = active ]`) {
		t.Errorf("docker is asked before its service is known to run")
	}
}

func TestProbeFailure(t *testing.T) {
	if _, err := Probe(context.Background(), &deadServer{}, "n"); err == nil || !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("err = %v", err)
	}
	if _, err := Probe(context.Background(), &erroring{err: errors.New("boom")}, "n"); err == nil {
		t.Fatal("no error")
	}
}

func TestShellQuote(t *testing.T) {
	tests := map[string]string{
		"":             `''`,
		"plain":        `'plain'`,
		"it's":         `'it'\''s'`,
		"$HOME":        `'$HOME'`,
		"`id`":         "'`id`'",
		"a\nb":         "'a\nb'",
		`back\slash`:   `'back\slash'`,
		"'; rm -rf /;": `''\''; rm -rf /;'`,
		`"dq"`:         `'"dq"'`,
	}
	for in, want := range tests {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRemoteCommandAndSSHArgs(t *testing.T) {
	if got := remoteCommand(false, "echo 'hi'"); got != `sh -c 'echo '\''hi'\'''` {
		t.Errorf("remoteCommand = %q", got)
	}
	if got := remoteCommand(true, "id"); got != `sudo -n sh -c 'id'` {
		t.Errorf("remoteCommand = %q", got)
	}
	args := sshArgs("ops@s1", true, "id -u")
	want := []string{"-o", "BatchMode=yes", "-T", "--", "ops@s1", `sudo -n sh -c 'id -u'`}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("sshArgs = %q", args)
	}
	// The destination is always after "--", and the script never becomes an argument of its own.
	args = sshArgs("s1", false, "x; --version")
	if len(args) != 6 || args[3] != "--" || args[4] != "s1" {
		t.Errorf("sshArgs = %q", args)
	}
}

func TestLocalArgs(t *testing.T) {
	name, args := localArgs(false, "id")
	if name != "sh" || !reflect.DeepEqual(args, []string{"-c", "id"}) {
		t.Errorf("root: %s %q", name, args)
	}
	name, args = localArgs(true, "id")
	if name != "sudo" || !reflect.DeepEqual(args, []string{"-n", "sh", "-c", "id"}) {
		t.Errorf("sudo: %s %q", name, args)
	}
	if _, err := newLocal("windows"); err == nil {
		t.Error("--local accepted on windows")
	}
	if _, err := newLocal("linux"); err != nil {
		t.Error(err)
	}
}

func TestValidHost(t *testing.T) {
	good := []string{"s1", "ops@s1", "deploy_user@10.0.0.5", "root@db-1.example.com", "a.b", "_svc@host"}
	bad := []string{"", "-oProxyCommand=x", "-s1", "ops@-s1", "ops@", "@s1", "Ops@s1", "ops@s1;id", "ops@s1 id", "ops@@s1", "ops@s1\n", "ops@[::1]", "-ops@s1", "ops@s1:22", "a b"}
	for _, h := range good {
		if !sshhost.Valid(h) {
			t.Errorf("sshhost.Valid(%q) = false", h)
		}
	}
	for _, h := range bad {
		if sshhost.Valid(h) {
			t.Errorf("sshhost.Valid(%q) = true", h)
		}
	}
	if _, err := NewSSH("-oProxyCommand=x"); err == nil {
		t.Error("NewSSH accepted an option")
	}
	if (&SSH{Dest: "-x"}).Target() != "-x" {
		t.Error("target")
	}
	if _, err := (&SSH{Dest: "-x", Program: "ssh"}).Run(context.Background(), "id", nil); err == nil {
		t.Error("Run accepted an option as the destination")
	}
}

func TestParseListen(t *testing.T) {
	good := map[string]string{"172.31.250.10:7710": "172.31.250.10", "10.0.0.1:1": "10.0.0.1", "[fd00::1]:7710": "fd00::1", "127.0.0.1:65535": "127.0.0.1"}
	for in, want := range good {
		ip, port, err := ParseListen(in)
		if err != nil || ip.String() != want || port < 1 {
			t.Errorf("ParseListen(%q) = %v %d %v", in, ip, port, err)
		}
	}
	for _, in := range []string{"", "172.31.250.10", ":7710", "0.0.0.0:7710", "[::]:7710", "host:7710", "10.0.0.1:0", "10.0.0.1:65536", "10.0.0.1:x", "10.0.0.1:+80", "10.0.0.1:", "fd00::1:7710", "10.0.0.1:77:10"} {
		if _, _, err := ParseListen(in); err == nil {
			t.Errorf("ParseListen(%q) succeeded", in)
		}
	}
}

func TestTailBufferKeepsTheEnd(t *testing.T) {
	b := &tailBuffer{max: 5}
	b.Write([]byte("abc"))
	b.Write([]byte("defgh"))
	if string(b.Bytes()) != "defgh" {
		t.Fatalf("got %q", b.Bytes())
	}
}

func TestCleanReplacesControlCharacters(t *testing.T) {
	if got := clean("a\x1b[31mb\nc\u0085"); strings.ContainsAny(got, "\x1b\n\u0085") {
		t.Fatalf("clean = %q", got)
	}
}

func TestSetupRejectsNetworkIDInsteadOfName(t *testing.T) {
	f := newServer(with(base(), with(dockerUp("29.9.3"), "swarm=active true", "network=overlay swarm true peer-overlay")...))
	results, ok := runAll(t, f, false)
	if ok || lastResult(results).Step != "network" {
		t.Fatalf("accepted network ID: %+v", results)
	}
}
