package setup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"berthy/internal/agent"
	"berthy/internal/agentclient"
	"berthy/internal/credstore"
	"berthy/internal/keywrap"
)

// Statuses of a step.
const (
	StatusOK          = "ok"           // nothing to change
	StatusChanged     = "changed"      // the step changed the server
	StatusWouldChange = "would_change" // a dry run: the step would change the server
	StatusSkipped     = "skipped"      // the step did not run
	StatusFailed      = "failed"
)

// MinDockerMajor is the oldest Docker Engine setup accepts.
const MinDockerMajor = 25

// Build describes this CLI: what it can upload to the server and which release it matches.
type Build struct {
	OS, Arch string // GOOS and GOARCH
	Path     string // the running executable
	Version  string // the version it reports, such as v1.2.0 or "dev (8649d6e)"
	Release  bool   // a release build: the server can download the same version
}

// DefaultListenPort is the port a first install uses with DefaultListenIP when --listen is not given.
const DefaultListenPort = 7710

// Options are the choices of one run.
type Options struct {
	StepStarted func(name string)
	Local       bool                             // --local: this machine is the server and keeps no copy
	KeyHome     string                           // this machine's home, or empty when unavailable
	NewWrapKey  func() (*keywrap.Private, error) // nil uses keywrap.Generate
	Env         string
	Project     string // the project of the manifest the CLI read; empty when the directory has none
	Network     string // the shared overlay network
	ListenIP    net.IP // the address the agent listens on and Swarm advertises; nil to keep the one the server's agent has
	ListenPort  int
	// DefaultListenIP is the manifest's address for the server being set up, or nil. A first install listens on
	// DefaultListenIP:DefaultListenPort when --listen is not given.
	DefaultListenIP net.IP
	Protected       bool
	NoInstallDocker bool

	Binary     string // --binary: the Linux build of Berthy to install instead of this CLI or the release
	KeyFile    string // --key-file: the saved encryption key to install
	NewToken   bool   // --new-token: add another admin token
	CLIConfig  string // --config: the CLI configuration to write the environment into; empty means the one the CLI finds
	Build      Build
	ReleaseURL string // BERTHY_RELEASE_URL: where releases are downloaded from; empty means GitHub

	// HealthTimeout overrides the wait of one minute per hosted environment plus 30 seconds.
	HealthTimeout time.Duration
	// Now is the clock of token names. Nil means the system clock.
	Now func() time.Time
	// Getenv reads an environment variable. Nil means os.Getenv.
	Getenv func(string) string
	// Doctor asks the agent for its doctor from this machine. Nil means an HTTP request.
	Doctor DoctorFunc
	// Tokens is the credential store of this machine, already bounded by the run's context. Nil means none.
	Tokens credstore.Store
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now().UTC()
	}
	return time.Now().UTC()
}

func (o Options) getenv(name string) string {
	if o.Getenv != nil {
		return o.Getenv(name)
	}
	return os.Getenv(name)
}

func (o Options) healthTimeout(hostedCount int) time.Duration {
	if o.HealthTimeout > 0 {
		return o.HealthTimeout
	}
	return time.Duration(max(1, hostedCount))*time.Minute + 30*time.Second
}

// listen is the address the agent listens on, as ADDRESS:PORT, or empty when it is not known yet.
func (o Options) listen() string {
	if o.ListenIP == nil {
		return ""
	}
	return net.JoinHostPort(o.ListenIP.String(), strconv.Itoa(o.ListenPort))
}

// Session is the state of one run, shared by its steps.
type Session struct {
	binary         binaryPlan
	binaryErr      error
	suppliedKey    []byte
	suppliedKeyErr error
	KeyCopyPath    string // verified copy on this machine
	KeyUnsettled   bool   // the server refused a copy during an unfinished rotation
	R              Runner
	F              *Facts
	Opts           Options
	DryRun         bool

	// Login is who setup runs as, found by the connect step.
	Login Login

	// Token is the admin token this run made. It lives only here: the steps never put it in a detail, an error or a script.
	// TokenLive is true once the agent's configuration with its digest is in place on the server.
	Token     string
	TokenLive bool
	// TokenUncertain means activation could have installed this run's token, but did not confirm it.
	TokenUncertain bool
	// TokenStored is true once this run saved its token and the store confirmed it.
	TokenStored bool
	// TokenEnv is the environment variable the CLI configuration names for the token, once the cli_config step knows it.
	TokenEnv         string
	cliEntry         agentclient.Environment
	cliConfigKnown   bool
	credentialRead   bool
	credential       string
	credentialSource agentclient.TokenSource

	// Log records what the run sends, for --verbose. Nil when the run is not verbose.
	Log *CommandLog

	// dryToken is set in a dry run when the run would make an admin token: a real run then checks with that token.
	dryToken bool
	// freshDocker is set in a dry run when Docker is missing: later steps then assume the Docker the install would leave.
	freshDocker bool
	// dockerStopped is set in a dry run when Docker is installed but stopped: the Swarm and the network cannot be read until
	// it runs.
	dockerStopped bool

	existing         *agent.File // the server's agent configuration, when there is one
	existingRaw      []byte
	configRead       bool
	newToken         *agent.Token
	hostedCount      int    // environments in the desired configuration
	staged           Staged // what this run staged for the service step
	removal          *removal
	removalCandidate []byte // nil when the removal's candidate is already live
	removalNetwork   *removalNetwork
	unitChanged      bool
	serverVer        *string // the version the agent binary on the server reports; "" when it says none
	// listenFromManifest is set when resolveListen took DefaultListenIP because there was no --listen and no agent config.
	listenFromManifest bool
}

// StepResult is what a step reported.
type StepResult struct {
	Step   string `json:"step"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	// Commands are the scripts the step sent, in order, each exactly as it was sent; in a dry run they are also the scripts a real
	// run would send. The verbose log prints each one without its final newline. Nil, so that the report has no commands key,
	// unless the run is verbose; a verbose step with no scripts has an empty list.
	Commands *[]string `json:"commands,omitempty"`
}

// Step checks the facts, says what it would change, and changes it unless it is a dry run. It returns the status and a
// detail for the output; an error is a failed step.
type Step struct {
	Name string
	Run  func(ctx context.Context, s *Session) (status, detail string, err error)
}

// Steps is every step of setup, in order.
func Steps() []Step {
	return []Step{
		{"connect", connectStep},
		{"check", checkStep},
		{"docker", dockerStep},
		{"swarm", swarmStep},
		{"network", networkStep},
		{"user", userStep},
		{"binary", binaryStep},
		{"key", keyStep},
		{"token", tokenStep},
		{"config", configStep},
		{"service", serviceStep},
		{"cli_config", cliConfigStep},
		{"cli_token", tokenStoreStep},
		{"verify", verifyStep},
		{"cli_key", keyCopyStep},
	}
}

// Run runs steps in order and reports each result as it happens. It stops at the first step that fails and returns false.
func Run(ctx context.Context, s *Session, steps []Step, report func(StepResult)) bool {
	defer func() { clear(s.suppliedKey); s.suppliedKey = nil }()
	for _, step := range steps {
		s.Log.begin(step.Name)
		if s.Opts.StepStarted != nil {
			s.Opts.StepStarted(step.Name)
		}
		status, detail, err := step.Run(ctx, s)
		if err != nil {
			status, detail = StatusFailed, clean(err.Error())
		}
		report(StepResult{Step: step.Name, Status: status, Detail: detail, Commands: s.Log.end()})
		if err != nil {
			return false
		}
	}
	return true
}

// facts probes the server once and keeps the answer.
func (s *Session) facts(ctx context.Context) (*Facts, error) {
	if s.F == nil {
		f, err := Probe(ctx, s.R, s.Opts.Network)
		if err != nil {
			return nil, err
		}
		s.F = f
	}
	return s.F, nil
}

// reprobe reads the facts again after a change.
func (s *Session) reprobe(ctx context.Context) error {
	s.F = nil
	_, err := s.facts(ctx)
	return err
}

// change runs a script that changes the server. what names it in the error when it fails.
func (s *Session) change(ctx context.Context, what, script string) (Result, error) {
	res, err := s.R.Run(ctx, script, nil)
	if err != nil {
		return res, fmt.Errorf("%s: %w", what, err)
	}
	if res.Code != 0 {
		return res, fmt.Errorf("%s: %s", what, failureText(res))
	}
	return res, nil
}

func connectStep(ctx context.Context, s *Session) (string, string, error) {
	login, err := Connect(ctx, s.R)
	if err != nil {
		return "", "", err
	}
	s.Login = login
	target := s.R.Target()
	if !strings.Contains(target, "@") && target != "this server" {
		target = login.User + "@" + target
	}
	if login.Sudo {
		target += " (sudo)"
	}
	return StatusOK, target, nil
}

// goArch is the architecture name Berthy's releases use for uname -m's.
func goArch(uname string) string {
	switch uname {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	}
	return ""
}

func checkStep(ctx context.Context, s *Session) (string, string, error) {
	f, err := s.facts(ctx)
	if err != nil {
		return "", "", err
	}
	if f.OS != "Linux" {
		return "", "", fmt.Errorf("this server runs %s, not Linux", orUnknown(clean(f.OS)))
	}
	if !f.Systemd {
		return "", "", errors.New("systemd is not running on this server")
	}
	arch := goArch(f.Arch)
	if arch == "" {
		return "", "", fmt.Errorf("unsupported CPU architecture %s: Berthy supports x86_64 and aarch64", orUnknown(clean(f.Arch)))
	}
	if err := s.resolveListen(ctx, f); err != nil {
		return "", "", err
	}
	if _, err := s.desiredFile(f); err != nil {
		return "", "", err
	}
	assigned := false
	for _, a := range f.Addrs {
		if ip := net.ParseIP(a); ip != nil && ip.Equal(s.Opts.ListenIP) {
			assigned = true
		}
	}
	if !assigned {
		if s.listenFromManifest {
			return "", "", fmt.Errorf("%s is not assigned to this server: the manifest's address for it must be one of its addresses", s.Opts.ListenIP)
		}
		return "", "", fmt.Errorf("%s is not assigned to this server: --listen needs one of its addresses", s.Opts.ListenIP)
	}
	s.binary, s.binaryErr = s.prepareBinary(ctx, f, arch)
	if s.binaryErr != nil && !s.DryRun {
		return "", "", s.binaryErr
	}
	if s.Opts.KeyFile != "" {
		s.suppliedKey, s.suppliedKeyErr = readKeyFile(s.Opts.KeyFile)
		if s.suppliedKeyErr != nil && !s.DryRun {
			return "", "", s.suppliedKeyErr
		}
	}
	return StatusOK, fmt.Sprintf("linux %s, systemd, %s assigned", arch, s.Opts.ListenIP), nil
}

func orUnknown(s string) string {
	if s == "" {
		return "an unknown system"
	}
	return s
}

// dockerMajor is the major number of a Docker version such as "29.9.3" or "24.0.7-ce".
func dockerMajor(v string) (int, bool) {
	major, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(major)
	return n, err == nil && n >= 0
}

// installScript downloads Docker's official install script to a temporary file, runs it and removes the file. fetch is the
// download command. Everything the installer prints goes to the script's standard output.
func installScript(fetch string) string {
	return "set -e\n" +
		"export DEBIAN_FRONTEND=noninteractive\n" +
		"tmp=$(mktemp)\n" +
		"trap 'rm -f \"$tmp\"' EXIT\n" +
		fetch + "\n" +
		"sh \"$tmp\" 2>&1\n" +
		"systemctl enable --now docker 2>&1\n"
}

const (
	curlFetch = `curl -fsSL https://get.docker.com -o "$tmp"`
	wgetFetch = `wget -qO "$tmp" https://get.docker.com`
)

func dockerStep(ctx context.Context, s *Session) (string, string, error) {
	f, err := s.facts(ctx)
	if err != nil {
		return "", "", err
	}
	if !f.Docker {
		return installDocker(ctx, s, f)
	}
	version := f.DockerVersion
	if version == "" {
		version = f.DockerClientVersion
	}
	major, ok := dockerMajor(version)
	if !ok {
		return "", "", errors.New("could not read the version of Docker")
	}
	if major < MinDockerMajor {
		return "", "", fmt.Errorf("Docker %s is older than the minimum %d", clean(version), MinDockerMajor)
	}
	var todo []string
	if !f.DockerActive {
		todo = append(todo, "start")
	}
	if !f.DockerEnabled {
		todo = append(todo, "enable")
	}
	if len(todo) == 0 {
		return StatusOK, fmt.Sprintf("Docker %s, running, enabled", clean(version)), nil
	}
	if !f.DockerActive {
		s.dockerStopped = true
	}
	scripts := make([]string, len(todo))
	for i, verb := range todo {
		scripts[i] = "systemctl " + verb + " docker"
	}
	if s.DryRun {
		for _, sc := range scripts {
			s.Log.Would(sc)
		}
		return StatusWouldChange, "would " + strings.Join(todo, " and ") + " Docker", nil
	}
	for i, verb := range todo {
		if _, err := s.change(ctx, verb+" Docker", scripts[i]); err != nil {
			return "", "", err
		}
	}
	if err := s.reprobe(ctx); err != nil {
		return "", "", err
	}
	if !s.F.DockerActive {
		return "", "", errors.New("Docker did not start")
	}
	if major, ok := dockerMajor(s.F.DockerVersion); !ok || major < MinDockerMajor {
		return "", "", fmt.Errorf("Docker %s is older than the minimum %d", clean(s.F.DockerVersion), MinDockerMajor)
	}
	done := make([]string, len(todo))
	for i, verb := range todo {
		done[i] = pastTense[verb]
	}
	return StatusChanged, fmt.Sprintf("%s Docker %s", strings.Join(done, " and "), clean(s.F.DockerVersion)), nil
}

var pastTense = map[string]string{"start": "started", "enable": "enabled"}

func installDocker(ctx context.Context, s *Session, f *Facts) (string, string, error) {
	if s.Opts.NoInstallDocker {
		return "", "", errors.New("Docker is not installed (--no-install-docker)")
	}
	var fetch string
	switch {
	case f.Curl:
		fetch = curlFetch
	case f.Wget:
		fetch = wgetFetch
	default:
		return "", "", errors.New("Docker is not installed, and neither curl nor wget is available to download its install script")
	}
	script := installScript(fetch)
	if s.DryRun {
		s.freshDocker = true
		s.Log.Would(script)
		return StatusWouldChange, "would install Docker with the official script", nil
	}
	res, err := s.R.Run(ctx, script, nil)
	if err != nil {
		return "", "", fmt.Errorf("installing Docker: %w", err)
	}
	if res.Code != 0 {
		return "", "", fmt.Errorf("installing Docker failed (exit code %d): %s", res.Code, lastLines(res, 20))
	}
	if err := s.reprobe(ctx); err != nil {
		return "", "", err
	}
	nf := s.F
	if !nf.Docker || !nf.DockerActive {
		return "", "", errors.New("Docker was installed but is not running")
	}
	if major, ok := dockerMajor(nf.DockerVersion); !ok || major < MinDockerMajor {
		return "", "", fmt.Errorf("Docker %s is older than the minimum %d", clean(nf.DockerVersion), MinDockerMajor)
	}
	return StatusChanged, fmt.Sprintf("installed %s with the official script", clean(nf.DockerVersion)), nil
}

// lastLines is the last n non-empty lines of what a script printed, joined for one line of output.
func lastLines(res Result, n int) string {
	lines := nonEmptyLines(append(append([]byte(nil), res.Stdout...), append([]byte("\n"), res.Stderr...)...))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i := range lines {
		lines[i] = clean(lines[i])
	}
	return strings.Join(lines, " | ")
}

// swarmInitScript turns on Swarm, advertising ip. The management port listens on the private address only, not on every address
// of the server.
func swarmInitScript(ip string) string {
	listen := net.JoinHostPort(ip, "2377")
	return "docker swarm init --advertise-addr " + shellQuote(ip) + " --listen-addr " + shellQuote(listen) + " 2>&1"
}

func swarmStep(ctx context.Context, s *Session) (string, string, error) {
	ip := s.Opts.ListenIP.String()
	advertise := fmt.Sprintf("would turn on Swarm advertising %s", ip)
	script := swarmInitScript(ip)
	if s.freshDocker {
		s.Log.Would(script)
		return StatusWouldChange, advertise, nil
	}
	if s.dockerStopped && s.DryRun {
		return StatusSkipped, "Docker is not running yet: the Swarm is checked after it starts", nil
	}
	f, err := s.facts(ctx)
	if err != nil {
		return "", "", err
	}
	switch f.SwarmState {
	case "active":
		if f.SwarmManager {
			return StatusOK, "manager", nil
		}
		return "", "", errors.New("this server is a worker in another Swarm")
	case "inactive":
		if s.DryRun {
			s.Log.Would(script)
			return StatusWouldChange, advertise, nil
		}
		if _, err := s.change(ctx, "turning on Swarm", script); err != nil {
			return "", "", err
		}
		if err := s.reprobe(ctx); err != nil {
			return "", "", err
		}
		if s.F.SwarmState != "active" || !s.F.SwarmManager {
			return "", "", errors.New("Swarm did not turn on as a manager")
		}
		return StatusChanged, "turned on Swarm advertising " + ip, nil
	case "":
		return "", "", errors.New("could not read the Swarm state")
	}
	return "", "", fmt.Errorf("the Swarm state is %s", clean(f.SwarmState))
}

// networkCreateScript creates the shared network: an overlay network that containers of the apps can attach to.
func networkCreateScript(name string) string {
	return "docker network create --driver overlay --attachable " + shellQuote(name) + " 2>&1"
}

func networkStep(ctx context.Context, s *Session) (string, string, error) {
	name := s.Opts.Network
	create := "would create network " + name
	script := networkCreateScript(name)
	if s.freshDocker {
		s.Log.Would(script)
		return StatusWouldChange, create, nil
	}
	if s.dockerStopped && s.DryRun {
		return StatusSkipped, "Docker is not running yet: the network is checked after it starts", nil
	}
	f, err := s.facts(ctx)
	if err != nil {
		return "", "", err
	}
	if f.NetworkError != "" {
		return "", "", fmt.Errorf("could not inspect network %s: %s", name, clean(f.NetworkError))
	}
	if f.NetworkFound {
		if f.NetworkDriver == "overlay" && f.NetworkScope == "swarm" && f.NetworkAttachable {
			return StatusOK, name + " (attachable overlay)", nil
		}
		return "", "", fmt.Errorf("network %s exists but is not an attachable overlay", name)
	}
	if s.DryRun {
		s.Log.Would(script)
		return StatusWouldChange, create, nil
	}
	if _, err := s.change(ctx, "creating network "+name, script); err != nil {
		return "", "", err
	}
	return StatusChanged, "created network " + name, nil
}
