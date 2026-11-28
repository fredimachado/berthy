package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/keywrap"
	"berthy/internal/manifest"
	"berthy/internal/scope"
	"berthy/internal/setup"
	"berthy/internal/sshhost"
	"berthy/internal/version"
)

const setupUsage = "usage: berthy setup -e ENV [--host USER@SERVER | --server NAME | --local] [--project NAME] [--listen ADDRESS:PORT] [--network NAME] [--protected]\n" +
	"                    [--manifest-dir DIR] [--binary PATH] [--key-file PATH] [--new-token] [--show-token] [--config PATH]\n" +
	"                    [--no-install-docker] [--dry-run] [--json] [--verbose]\n" +
	"       berthy setup --remove -e ENV [--host USER@SERVER | --server NAME | --local] [--project NAME]\n" +
	"                    [--manifest-dir DIR] [--config PATH] [--purge] [--dry-run] [--json] [--verbose]"

// newSetupRunner makes the runner that reaches the server: ssh to host, or this server for local. Tests replace it.
var newSetupRunner = func(host string, local bool) (setup.Runner, error) {
	if local {
		return setup.NewLocal()
	}
	return setup.NewSSH(host)
}

// setupBuild describes this CLI for the steps that install the agent. Tests replace it.
var setupBuild = func() setup.Build {
	path, _ := os.Executable()
	return setup.Build{OS: runtime.GOOS, Arch: runtime.GOARCH, Path: path, Version: version.String(), Release: version.Release()}
}

// setupDoctor asks the new agent for its doctor from this machine. Nil sends an HTTP request; tests replace it.
var setupDoctor setup.DoctorFunc

var errNoKeyHome = errors.New("no home folder")

// KeyHome finds the home for key copies; only the executable enables it.
var KeyHome = func() (string, error) { return "", errNoKeyHome }
var setupNewWrapKey func() (*keywrap.Private, error)

var keyCopyNow = time.Now

// setupReport is the --json output of setup.
type setupReport struct {
	Project     string             `json:"project"`
	Environment string             `json:"environment"`
	Target      string             `json:"target"`
	DryRun      bool               `json:"dry_run"`
	Remove      bool               `json:"remove,omitempty"`
	Steps       []setup.StepResult `json:"steps"`
	OK          bool               `json:"ok"`
	// Token is the admin token this run made when the output rule requires it.
	Token       string   `json:"token,omitempty"`
	TokenStored bool     `json:"token_stored,omitempty"`
	KeyCopy     string   `json:"key_copy,omitempty"`
	NextSteps   []string `json:"next_steps,omitempty"`
}

// runSetup runs "setup": it connects to a server over ssh, or runs on it, and gets it ready for the agent. Exit 0 means every
// step is ok or changed (or would change, in a dry run), 1 that a step failed, 2 that the command line is wrong.
func runSetup(args []string, out, errout io.Writer) int {
	ctx, stop := commandContext()
	defer stop()
	return runSetupWithContext(ctx, args, out, errout)
}

func runSetupWithContext(ctx context.Context, args []string, out, errout io.Writer) int {
	fset := flag.NewFlagSet("setup", flag.ContinueOnError)
	fset.SetOutput(errout)
	fset.Usage = func() { fmt.Fprintln(errout, setupUsage) }
	env := fset.String("e", "", "environment")
	remove := fset.Bool("remove", false, "remove a hosted environment after its apps have been destroyed")
	purge := fset.Bool("purge", false, "delete this removal's state instead of archiving it")
	project := fset.String("project", "", "project name")
	host := fset.String("host", "", "USER@SERVER to set up over ssh")
	server := fset.String("server", "", "NAME of the manifest server to set up")
	local := fset.Bool("local", false, "set up the server this command runs on")
	listen := fset.String("listen", "", "ADDRESS:PORT the agent listens on; needed on the first run unless the manifest gives the server's address")
	network := fset.String("network", "", "the shared network, when there is no manifest")
	protected := fset.Bool("protected", false, "mark the environment as protected, when there is no manifest")
	dir := fset.String("manifest-dir", manifest.DefaultDir(), "directory of the manifest")
	binary := fset.String("binary", "", "a Linux build of Berthy to install instead of the release that matches this CLI")
	keyFile := fset.String("key-file", "", "a saved encryption key to install, when rebuilding a server")
	newToken := fset.Bool("new-token", false, "add another admin token")
	showToken := fset.Bool("show-token", false, "print the admin token as well as saving it")
	cliConfig := fset.String("config", "", "the CLI configuration to add the environment to (by default the one the CLI finds)")
	noInstall := fset.Bool("no-install-docker", false, "stop when Docker is missing instead of installing it")
	dry := fset.Bool("dry-run", false, "check and report what would change, without changing anything")
	asJSON := fset.Bool("json", false, "report as JSON")
	verbose := fset.Bool("verbose", false, "print each command setup sends and its exit status on standard error; with --dry-run, the commands a real run would send")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	envGiven, keyFileGiven, hostGiven, serverGiven, purgeGiven := false, false, false, false, false
	var installOnly string
	fset.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "e":
			envGiven = true
		case "key-file":
			keyFileGiven = true
		case "host":
			hostGiven = true
		case "server":
			serverGiven = true
		case "purge":
			purgeGiven = true
		}
		switch f.Name {
		case "listen", "network", "protected", "binary", "key-file", "new-token", "show-token", "no-install-docker":
			if installOnly == "" {
				installOnly = f.Name
			}
		}
	})
	resolvedEnv, envFromVariable := agentclient.EnvironmentName(*env, envGiven)
	*env = resolvedEnv
	usage := func(format string, a ...any) int {
		printRefusal(errout, fmt.Sprintf(format, a...), !*asJSON)
		fmt.Fprintln(errout, setupUsage)
		return 2
	}
	chosen := 0
	if hostGiven {
		chosen++
	}
	if serverGiven {
		chosen++
	}
	if *local {
		chosen++
	}
	switch {
	case fset.NArg() != 0:
		return usage("setup takes no arguments")
	case purgeGiven && !*remove:
		return usage("--purge requires --remove")
	case *remove && installOnly != "":
		return usage("--%s cannot be used with --remove", installOnly)
	case *env == "":
		return usage("setup needs -e ENV: lowercase letters, digits and dashes")
	case !scope.ValidEnvironment(*env):
		if envFromVariable {
			return usage("BERTHY_ENV is not a valid environment")
		}
		return usage("setup needs -e ENV: lowercase letters, digits and dashes")
	case chosen > 1:
		return usage("use only one of --host, --server and --local")
	case hostGiven && !sshhost.Valid(*host):
		return usage("invalid --host %q: use USER@SERVER or SERVER", *host)
	}
	// The agent's address is needed on a first run only; the server's own configuration has it afterwards.
	var ip net.IP
	var port int
	if *listen != "" {
		var err error
		if ip, port, err = setup.ParseListen(*listen); err != nil {
			return usage("%v", err)
		}
	}
	from, err := setupSettings(*dir, *env, *project, *network, *protected)
	if err != nil {
		return usage("%v", err)
	}
	var defaultListen net.IP
	if !*local && !hostGiven {
		dest, err := setupDestination(from.manifest, *env, *server, serverGiven)
		if err != nil {
			return usage("%v", err)
		}
		*host = dest.SSH
		if dest.Address != "" {
			defaultListen = net.ParseIP(dest.Address)
		}
	}
	where := "on " + *host
	if *local {
		where = "on this server"
	}
	printBanner(errout, scope.ID{Project: from.project, Environment: *env}, where, envFromVariable, !*asJSON)
	var preflightErr error
	var steps []setup.Step
	opts := setup.Options{Local: *local, Env: *env, Project: from.project, CLIConfig: *cliConfig}
	if *remove {
		steps = setup.RemovalSteps(*purge)
	} else {
		steps = setup.Steps()
		preflightErr = checkSetupTokenEnv(from.project, *env, *cliConfig)
		if !*local {
			if h, err := KeyHome(); err == nil && filepath.IsAbs(h) {
				opts.KeyHome = h
			}
		}
		opts.NewWrapKey = setupNewWrapKey
		opts.Network, opts.Protected = from.network, from.protected
		opts.ListenIP, opts.ListenPort, opts.DefaultListenIP = ip, port, defaultListen
		opts.Binary, opts.KeyFile, opts.NewToken, opts.NoInstallDocker = *binary, *keyFile, *newToken, *noInstall
		opts.Build, opts.ReleaseURL, opts.Doctor = setupBuild(), os.Getenv("BERTHY_RELEASE_URL"), setupDoctor
		opts.Tokens = agentclient.Tokens(ctx)
	}

	report := setupReport{Project: from.project, Environment: *env, Target: *host, DryRun: *dry, Remove: *remove, Steps: []setup.StepResult{}}
	if *local {
		report.Target = "this server"
	}
	progress := newProgressLine(errout, !*asJSON && !*verbose && stderrTerminal(errout))
	words := !*verbose && stderrTerminal(out)
	opts.StepStarted = progress.Start
	show := func(r setup.StepResult) {
		report.Steps = append(report.Steps, r)
		if !*asJSON {
			progress.Finish(out, r, words)
		}
	}
	var session *setup.Session
	if preflightErr != nil {
		show(setup.StepResult{Step: "check", Status: setup.StatusFailed, Detail: terminalSafe(preflightErr.Error())})
	} else if runner, err := newSetupRunner(*host, *local); err != nil {
		show(setup.StepResult{Step: "connect", Status: setup.StatusFailed, Detail: terminalSafe(err.Error())})
	} else {
		report.Target = runner.Target()
		session = &setup.Session{R: runner, Opts: opts, DryRun: *dry}
		if *verbose {
			// The log prints to standard error as the run goes; the report reads the same log for each step's commands.
			logStyle := styleFor(errout, !*asJSON)
			session.Log = setup.NewStyledCommandLog(errout, setup.CommandLogStyle{
				Prompt: func(v string) string { return logStyle.paint(toneAccent, v) },
				Exit: func(code int, v string) string {
					if code == 0 {
						return logStyle.paint(toneOK, v)
					}
					return logStyle.paint(toneError, v)
				},
			})
			session.R = setup.Recording(runner, session.Log)
		}
		report.OK = setup.Run(ctx, session, steps, show)
	}
	progress.Close()
	// A failed run also shows an uncertain token because the server may have kept its configuration.
	if !*remove && session != nil && session.Token != "" && (session.TokenLive && (!session.TokenStored || *showToken) || !report.OK && session.TokenUncertain) {
		report.Token = session.Token
	}
	if !*remove && session != nil {
		report.TokenStored = session.TokenStored
		if report.OK && !*dry {
			report.KeyCopy = session.KeyCopyPath
		}
	}
	manifestDir := ""
	if !*remove && report.OK && !*dry {
		fset.Visit(func(f *flag.Flag) {
			if f.Name == "manifest-dir" {
				manifestDir = *dir
			}
		})
	}
	var secretStep secretsStep
	if !*remove && from.manifest != nil {
		secretStep.declared = len(from.manifest.Secrets) > 0
	}
	if !*asJSON {
		fmt.Fprintln(out, setupSummary(report, styleFor(out, true)))
		if report.Token != "" {
			fmt.Fprintf(out, "\nadmin token for %s (%s): %s\n", terminalSafe(from.project+"/"+*env), styleFor(out, true).paint(toneWarning, "shown once, store it now"), report.Token)
		}
		if !*remove && report.OK && !*dry && ctx.Err() == nil && from.manifest != nil && session.AdminToken() != "" {
			if read, ok := hiddenInput(); ok {
				id := scope.ID{Project: from.project, Environment: *env}
				base, _ := url.Parse(session.AgentURL())
				target := &agentclient.Target{Project: id.Project, Env: id.Environment, Base: base, Prefix: id.APIPath(), Token: session.AdminToken()}
				fmt.Fprintln(out, "\nsecrets:")
				unmet, result := guideSecrets(ctx, agentclient.NewHTTPClient(), target, from.manifest, *dir, *env, func() ([]byte, error) { return read(ctx) }, out, errout)
				secretStep.guided = result == guideDone
				for _, need := range unmet {
					secretStep.unmet = append(secretStep.unmet, need.Name)
				}
			}
		}
	}
	if !*remove && report.OK && !*dry {
		report.NextSteps = setupNextSteps(nextSteps{id: scope.ID{Project: from.project, Environment: *env}, keyCopy: report.KeyCopy, keyUnsettled: session.KeyUnsettled, keyFileGiven: keyFileGiven, envFromVariable: envFromVariable, newToken: *newToken, config: *cliConfig, local: *local, tokenEnv: session.TokenEnv, storeToken: report.Token != "" && !session.TokenStored, ingress: from.ingress, manifestDir: manifestDir, secrets: secretStep, inferProject: from.manifest != nil})
	}
	if *asJSON {
		// Keep characters in next steps readable.
		enc := json.NewEncoder(out)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintln(errout, "could not write the report")
			return 1
		}
	} else {
		if len(report.NextSteps) > 0 {
			fmt.Fprintln(out, "\n"+styleFor(out, true).paint(toneInfo, "next steps:"))
			for i, step := range report.NextSteps {
				fmt.Fprintf(out, "  %d. %s\n", i+1, terminalSafe(step))
			}
		}
	}
	if !report.OK {
		return 1
	}
	return 0
}

func checkSetupTokenEnv(project, env, config string) error {
	path, err := agentclient.ConfigPath(config)
	if err != nil {
		return nil
	}
	c, err := setup.ReadCLIConfig(path)
	if err != nil {
		return err
	}
	tokenEnv := c.Projects[project].Environments[env].TokenEnv
	if tokenEnv == "" {
		tokenEnv = setup.DefaultTokenEnv(project, env)
	}
	_, err = c.Put(project, env, agentclient.Environment{URL: "http://127.0.0.1:1", TokenEnv: tokenEnv})
	return err
}

// setupFrom is what setup takes from the manifest, or from the flags when the directory has none.
type setupFrom struct {
	manifest  *manifest.Manifest
	project   string // empty when the directory has no manifest
	network   string
	protected bool
	ingress   bool // the manifest declares managed ingress
}

// setupSettings finds the network and protected setting: from the merged manifest of the environment when the directory has
// one, from the flags otherwise. A flag that disagrees with the manifest is an error.
func setupSettings(dir, env, project, network string, protected bool) (setupFrom, error) {
	if project != "" && !scope.ValidProject(project) {
		return setupFrom{}, fmt.Errorf("invalid --project %q", project)
	}
	m, err := manifest.Load(dir, env)
	switch {
	case err == nil:
		if project != "" && project != m.Project {
			return setupFrom{}, fmt.Errorf("--project %s differs from the project %s in the manifest", project, m.Project)
		}
		name := m.NetworkName(env)
		if network != "" && network != name {
			return setupFrom{}, fmt.Errorf("--network %s differs from the network %s in the manifest of %s", network, name, env)
		}
		return setupFrom{manifest: m, project: m.Project, network: name, protected: m.Protected || protected, ingress: m.Ingress != nil}, nil
	case errors.Is(err, fs.ErrNotExist):
		if project == "" {
			return setupFrom{}, fmt.Errorf("there is no berthy.yml in %s: pass --project NAME", dir)
		}
		if err := (scope.ID{Project: project, Environment: env}).CheckLength(); err != nil {
			return setupFrom{}, err
		}
		if network == "" {
			network = (&manifest.Manifest{Project: project}).NetworkName(env)
		}
		if !manifest.ValidNetwork(network) {
			return setupFrom{}, fmt.Errorf("invalid --network %q: lowercase letters, digits, - and _", network)
		}
		return setupFrom{project: project, network: network, protected: protected}, nil
	}
	return setupFrom{}, fmt.Errorf("manifest of %s: %v", env, err)
}

// setupDestination picks the server entry from the manifest's servers: the one named by --server, or the only one.
// m is nil when the directory has no manifest.
func setupDestination(m *manifest.Manifest, env, name string, named bool) (manifest.Server, error) {
	var servers map[string]manifest.Server
	if m != nil {
		servers = m.Servers
	}
	names := slices.Sorted(maps.Keys(servers))
	joined := strings.Join(names, ", ")
	if named {
		if len(servers) == 0 {
			return manifest.Server{}, fmt.Errorf("the manifest of %s lists no servers", env)
		}
		server, ok := servers[name]
		if !ok {
			return manifest.Server{}, fmt.Errorf("the manifest of %s has no server %q: use one of %s", env, name, joined)
		}
		return server, nil
	}
	switch len(servers) {
	case 0:
		return manifest.Server{}, fmt.Errorf("setup needs --host USER@SERVER, a server in the manifest, or --local")
	case 1:
		return servers[names[0]], nil
	}
	return manifest.Server{}, fmt.Errorf("the manifest of %s lists servers %s: pass --server NAME", env, joined)
}

// secretsStep describes the secrets still needing attention after setup.
type secretsStep struct {
	guided   bool
	unmet    []string
	declared bool
}

// nextSteps is what setup needs to describe what follows a successful run.
type nextSteps struct {
	secrets         secretsStep
	id              scope.ID
	keyCopy         string
	keyUnsettled    bool
	keyFileGiven    bool
	envFromVariable bool
	newToken        bool
	config          string
	manifestDir     string
	local           bool
	tokenEnv        string
	storeToken      bool
	ingress         bool
	inferProject    bool // setup loaded a manifest, so the follow-up commands read the project from it
}

// setupNextSteps describes what follows a successful setup.
func setupNextSteps(n nextSteps) []string {
	env := n.id.Environment
	config := ""
	if !n.inferProject {
		config = " --project " + shellWord(n.id.Project)
	}
	if n.config != "" {
		config += " --config " + shellWord(n.config)
	}
	if n.manifestDir != "" {
		config += " --manifest-dir " + shellWord(n.manifestDir)
	}
	retry := "run the same berthy setup command again, from this same directory and with the same environment"
	if n.envFromVariable {
		retry += " (BERTHY_ENV=" + env + ")"
	}
	if n.newToken {
		retry += "; leave out --new-token, because this run already added that admin token"
	}
	var first string
	switch {
	case n.keyCopy != "":
		first = "keep the copy of the encryption key that setup saved in " + n.keyCopy + ", and store another in a password manager or vault: without the key, the secrets of a lost server are gone"
	case n.keyUnsettled:
		first = "a key rotation on the server did not finish, so setup copied no key. On the server, find and fix what kept the agent from putting its key files in place (for example a leftover <key>.prev.tmp it cannot remove, a full disk, or wrong ownership). Do not restart the agent before that: it finishes the rotation when it starts, and it cannot start while the cause remains. Then restart it and " + retry
		if n.keyFileGiven {
			first += "; leave out --key-file, because the saved key may be older than the key the agent puts in place"
		}
	case n.local:
		first = "setup kept no copy of the encryption key of " + n.id.Project + "/" + env + ", because it ran on the server itself. To save one, run berthy setup for that same project and environment over SSH from your own machine, with --project " + n.id.Project + " and the same build that is installed on this server: the same released version of the CLI, or the same custom Linux binary given with --binary. First resolve any interrupted key rotation on this server. If you are not sure about the build or the rotation, check before you copy"
		if n.inferProject {
			first += "; run it from the directory of the " + n.id.Project + " manifest"
		} else {
			first += "; give the same --network and --protected settings as this run"
		}
	default:
		first = "setup saved no copy of the encryption key (cli_key above says why): fix that, then " + retry
	}
	steps := []string{first}
	if n.storeToken {
		steps = append(steps, fmt.Sprintf("store the admin token above, and set it in $%s when you run berthy", n.tokenEnv))
	}
	if !n.secrets.guided {
		if n.secrets.declared {
			steps = append(steps, fmt.Sprintf("generate the secrets the manifest declares: berthy secret generate -e %s%s", env, config))
		}
		steps = append(steps, fmt.Sprintf("set the secrets the manifest names: berthy secret set NAME -e %s%s", env, config))
	} else if len(n.secrets.unmet) > 0 {
		steps = append(steps, fmt.Sprintf("set the secrets still missing (%s): berthy secret set -e %s%s", strings.Join(n.secrets.unmet, ", "), env, config))
	}
	if n.ingress {
		steps = append(steps, fmt.Sprintf("apply the managed ingress: berthy ingress apply -e %s%s", env, config))
	}
	return append(steps, fmt.Sprintf("deploy every app in depends_on order, with --version APP=REF for each image app: berthy deploy -e %s%s --version APP=REF (or one app: berthy deploy APP -e %s%s --version REF)", env, config, env, config))
}

// keyFileName names the manual rotation copy for the environment.
func keyFileName(id scope.ID) string { return id.Slug() + ".key" }

// keyCopyCommand is the manual rotation command; read prints the key into a new file in the server's home folder.
func keyCopyCommand(read string, id scope.ID) string {
	name := keyFileName(id) + "." + keyCopyNow().UTC().Format("20060102T150405Z")
	return "(umask 077; set -C; " + read + " > ~/" + shellWord(name) + ")"
}

// shellWord is s as one word of a POSIX shell command: unchanged when it has only letters, digits and _ . / ~ : -, and in
// single quotes otherwise, with each single quote written as '\”.
func shellWord(s string) string {
	plain := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_./~:-", r)) {
			plain = false
			break
		}
	}
	if plain {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellDoubleQuote is s as one double-quoted word for a command the person pastes into cmd.exe, PowerShell or a POSIX
// shell. It escapes ", $ and backtick.
func shellDoubleQuote(s string) string {
	return `"` + strings.NewReplacer(`"`, `\"`, `$`, `\$`, "`", "\\`").Replace(s) + `"`
}

var setupGlyphs = map[string]string{
	setup.StatusOK:          "✓",
	setup.StatusChanged:     "✓",
	setup.StatusWouldChange: "·",
	setup.StatusSkipped:     "–",
	setup.StatusFailed:      "✖",
}

// setupSummary is the last line of the text output.
func setupSummary(r setupReport, s cliStyle) string {
	action := "setup"
	if r.Remove {
		action = "removal"
	}
	prefix := fmt.Sprintf("%s of %s on %s: ", action, terminalSafe(r.Project+"/"+r.Environment), terminalSafe(r.Target))
	if !r.OK {
		failed := "connect"
		for _, s := range r.Steps {
			if s.Status == setup.StatusFailed {
				failed = s.Step
			}
		}
		return prefix + s.paint(toneError, "failed at") + " " + terminalSafe(failed)
	}
	if !r.DryRun {
		return prefix + s.paint(toneOK, "done")
	}
	n := 0
	for _, s := range r.Steps {
		if s.Status == setup.StatusWouldChange {
			n++
		}
	}
	switch n {
	case 0:
		return prefix + "dry run, nothing would change"
	case 1:
		return prefix + "dry run, 1 step would change"
	}
	return fmt.Sprintf("%sdry run, %d steps would change", prefix, n)
}
