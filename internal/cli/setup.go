package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/manifest"
	"berthy/internal/scope"
	"berthy/internal/setup"
	"berthy/internal/version"
)

const setupUsage = "usage: berthy setup -e ENV (--host USER@SERVER | --local) [--project NAME] [--listen ADDRESS:PORT] [--network NAME] [--protected]\n" +
	"                    [--manifest-dir DIR] [--binary PATH] [--key-file PATH] [--new-token] [--show-token] [--config PATH]\n" +
	"                    [--no-install-docker] [--dry-run] [--json] [--verbose]"

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

var keyCopyNow = time.Now

// setupReport is the --json output of setup.
type setupReport struct {
	Project     string             `json:"project"`
	Environment string             `json:"environment"`
	Target      string             `json:"target"`
	DryRun      bool               `json:"dry_run"`
	Steps       []setup.StepResult `json:"steps"`
	OK          bool               `json:"ok"`
	// Token is the admin token this run made when the output rule requires it.
	Token       string   `json:"token,omitempty"`
	TokenStored bool     `json:"token_stored,omitempty"`
	NextSteps   []string `json:"next_steps,omitempty"`
}

// runSetup runs "setup": it connects to a server over ssh, or runs on it, and gets it ready for the agent. Exit 0 means every
// step is ok or changed (or would change, in a dry run), 1 that a step failed, 2 that the command line is wrong.
func runSetup(args []string, out, errout io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runSetupWithContext(ctx, args, out, errout)
}

func runSetupWithContext(ctx context.Context, args []string, out, errout io.Writer) int {
	fset := flag.NewFlagSet("setup", flag.ContinueOnError)
	fset.SetOutput(errout)
	fset.Usage = func() { fmt.Fprintln(errout, setupUsage) }
	env := fset.String("e", "", "environment")
	project := fset.String("project", "", "project name")
	host := fset.String("host", "", "USER@SERVER to set up over ssh")
	local := fset.Bool("local", false, "set up the server this command runs on")
	listen := fset.String("listen", "", "ADDRESS:PORT the agent listens on; needed on the first run")
	network := fset.String("network", "", "the shared network, when there is no manifest")
	protected := fset.Bool("protected", false, "mark the environment as protected, when there is no manifest")
	dir := fset.String("manifest-dir", ".", "directory of the manifest")
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
	usage := func(format string, a ...any) int {
		fmt.Fprintf(errout, format+"\n", a...)
		fmt.Fprintln(errout, setupUsage)
		return 2
	}
	switch {
	case fset.NArg() != 0:
		return usage("setup takes no arguments")
	case *env == "" || !scope.ValidEnvironment(*env):
		return usage("setup needs -e ENV: lowercase letters, digits and dashes")
	case *host != "" && *local, *host == "" && !*local:
		return usage("use exactly one of --host and --local")
	case *host != "" && !setup.ValidHost(*host):
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
	preflightErr := checkSetupTokenEnv(from.project, *env, *cliConfig)
	opts := setup.Options{
		Env: *env, Project: from.project, Network: from.network, ListenIP: ip, ListenPort: port, Protected: from.protected, NoInstallDocker: *noInstall,
		Binary: *binary, KeyFile: *keyFile, NewToken: *newToken, CLIConfig: *cliConfig, Build: setupBuild(),
		ReleaseURL: os.Getenv("BERTHY_RELEASE_URL"), Doctor: setupDoctor,
	}

	opts.Tokens = agentclient.Tokens(ctx)
	report := setupReport{Project: from.project, Environment: *env, Target: *host, DryRun: *dry, Steps: []setup.StepResult{}}
	if *local {
		report.Target = "this server"
	}
	show := func(r setup.StepResult) {
		report.Steps = append(report.Steps, r)
		if !*asJSON {
			printSetupStep(out, r)
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
			session.Log = setup.NewCommandLog(errout)
			session.R = setup.Recording(runner, session.Log)
		}
		report.OK = setup.Run(ctx, session, setup.Steps(), show)
	}
	// A live token must be shown when it was not confirmed saved, even if a later step failed.
	if session != nil && session.Token != "" && session.TokenLive && (!session.TokenStored || *showToken) {
		report.Token = session.Token
	}
	if session != nil {
		report.TokenStored = session.TokenStored
	}
	if report.OK && !*dry {
		manifestDir := ""
		fset.Visit(func(f *flag.Flag) {
			if f.Name == "manifest-dir" {
				manifestDir = *dir
			}
		})
		report.NextSteps = setupNextSteps(scope.ID{Project: from.project, Environment: *env}, session.KeyFile(), *cliConfig, *local, report.Target, session.TokenEnv, report.Token != "" && !session.TokenStored, from.ingress, manifestDir)
	}
	if *asJSON {
		// No HTML escaping: the next steps hold shell redirections that should stay readable.
		enc := json.NewEncoder(out)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintln(errout, "could not write the report")
			return 1
		}
	} else {
		fmt.Fprintln(out, setupSummary(report))
		if report.Token != "" {
			fmt.Fprintf(out, "\nadmin token for %s (shown once, store it now): %s\n", terminalSafe(from.project+"/"+*env), report.Token)
		}
		if len(report.NextSteps) > 0 {
			fmt.Fprintln(out, "\nnext steps:")
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
		return setupFrom{project: m.Project, network: name, protected: m.Protected || protected, ingress: m.Ingress != nil}, nil
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

// setupNextSteps is what the person does after a successful setup. It prints the command that copies the key, never the key.
// cfg is the --config path given to setup, quoted for the shell when it needs it. The commands name it only when it was given:
// without it they find the CLI configuration themselves, the one the cli_config step wrote or the one BERTHY_CONFIG names.
func setupNextSteps(id scope.ID, keyFile, cfg string, local bool, target, tokenEnv string, madeToken, ingress bool, manifestDir string) []string {
	env := id.Environment
	read := "sudo cat " + shellWord(keyFile)
	if !local {
		read = "ssh " + shellWord(target) + " " + shellWord(read)
	}
	config := ""
	if cfg != "" {
		config = " --config " + shellWord(cfg)
	}
	if manifestDir != "" {
		config += " --manifest-dir " + shellWord(manifestDir)
	}
	config = " --project " + shellWord(id.Project) + config
	steps := []string{
		"run the command from a private folder, such as your home folder (cd ~), and keep the file private",
		"in a POSIX shell (macOS, Linux, or Git Bash/WSL on Windows): " + keyCopyCommand(read, id),
	}
	if madeToken {
		steps = append(steps, fmt.Sprintf("store the admin token above, and set it in $%s when you run berthy", tokenEnv))
	}
	steps = append(steps, fmt.Sprintf("set the secrets the manifest names: berthy secret set NAME -e %s%s", env, config))
	if ingress {
		steps = append(steps, fmt.Sprintf("apply the managed ingress: berthy ingress apply -e %s%s", env, config))
	}
	return append(steps, fmt.Sprintf("deploy every app in depends_on order, with --version APP=REF for each image app: berthy deploy -e %s%s --version APP=REF (or one app: berthy deploy APP -e %s%s --version REF)", env, config, env, config))
}

// keyCopyCommand is the shell command that copies the key off the server: read prints the key, and its output goes to a file
// named for the environment. Setup and `key rotate` both print it, and neither prints the key.
func keyFileName(id scope.ID) string { return id.Slug() + ".key" }

func keyCopyCommand(read string, id scope.ID) string {
	name := keyFileName(id) + "." + keyCopyNow().UTC().Format("20060102T150405Z")
	return "(umask 077; set -C; " + read + " > " + shellWord(name) + ")"
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

func printSetupStep(out io.Writer, r setup.StepResult) {
	fmt.Fprintf(out, "%s %-10s  %s\n", setupGlyphs[r.Status], terminalSafe(r.Step), terminalSafe(r.Detail))
}

// setupSummary is the last line of the text output.
func setupSummary(r setupReport) string {
	prefix := fmt.Sprintf("setup of %s on %s: ", terminalSafe(r.Project+"/"+r.Environment), terminalSafe(r.Target))
	if !r.OK {
		failed := "connect"
		for _, s := range r.Steps {
			if s.Status == setup.StatusFailed {
				failed = s.Step
			}
		}
		return prefix + "failed at " + terminalSafe(failed)
	}
	if !r.DryRun {
		return prefix + "done"
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
