package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"

	"berthy/internal/manifest"
	"berthy/internal/setup"
)

const setupUsage = "usage: berthy setup -e ENV (--host USER@SERVER | --local) --listen ADDRESS:PORT [--network NAME] [--protected]\n" +
	"                    [--manifest-dir DIR] [--no-install-docker] [--dry-run] [--json]"

// newSetupRunner makes the runner that reaches the server: ssh to host, or this server for local. Tests replace it.
var newSetupRunner = func(host string, local bool) (setup.Runner, error) {
	if local {
		return setup.NewLocal()
	}
	return setup.NewSSH(host)
}

// setupReport is the --json output of setup.
type setupReport struct {
	Environment string             `json:"environment"`
	Target      string             `json:"target"`
	DryRun      bool               `json:"dry_run"`
	Steps       []setup.StepResult `json:"steps"`
	OK          bool               `json:"ok"`
}

// runSetup runs "setup": it connects to a server over ssh, or runs on it, and gets it ready for the agent. Exit 0 means every
// step is ok or changed (or would change, in a dry run), 1 that a step failed, 2 that the command line is wrong.
func runSetup(args []string, out, errout io.Writer) int {
	fset := flag.NewFlagSet("setup", flag.ContinueOnError)
	fset.SetOutput(errout)
	fset.Usage = func() { fmt.Fprintln(errout, setupUsage) }
	env := fset.String("e", "", "environment")
	host := fset.String("host", "", "USER@SERVER to set up over ssh")
	local := fset.Bool("local", false, "set up the server this command runs on")
	listen := fset.String("listen", "", "ADDRESS:PORT the agent listens on")
	network := fset.String("network", "", "the shared network, when there is no manifest")
	protected := fset.Bool("protected", false, "mark the environment as protected, when there is no manifest")
	dir := fset.String("manifest-dir", ".", "directory of the manifest")
	noInstall := fset.Bool("no-install-docker", false, "stop when Docker is missing instead of installing it")
	dry := fset.Bool("dry-run", false, "check and report what would change, without changing anything")
	asJSON := fset.Bool("json", false, "report as JSON")
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
	case *env == "" || !envPattern.MatchString(*env):
		return usage("setup needs -e ENV: lowercase letters, digits and dashes")
	case *host != "" && *local, *host == "" && !*local:
		return usage("use exactly one of --host and --local")
	case *host != "" && !setup.ValidHost(*host):
		return usage("invalid --host %q: use USER@SERVER or SERVER", *host)
	case *listen == "":
		return usage("setup needs --listen ADDRESS:PORT, the private address of the server")
	}
	ip, port, err := setup.ParseListen(*listen)
	if err != nil {
		return usage("%v", err)
	}
	name, isProtected, err := setupSettings(*dir, *env, *network, *protected)
	if err != nil {
		return usage("%v", err)
	}
	opts := setup.Options{Env: *env, Network: name, ListenIP: ip, ListenPort: port, Protected: isProtected, NoInstallDocker: *noInstall}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	report := setupReport{Environment: *env, Target: *host, DryRun: *dry, Steps: []setup.StepResult{}}
	if *local {
		report.Target = "this server"
	}
	show := func(r setup.StepResult) {
		report.Steps = append(report.Steps, r)
		if !*asJSON {
			printSetupStep(out, r)
		}
	}
	runner, err := newSetupRunner(*host, *local)
	if err != nil {
		show(setup.StepResult{Step: "connect", Status: setup.StatusFailed, Detail: terminalSafe(err.Error())})
	} else {
		report.Target = runner.Target()
		report.OK = setup.Run(ctx, &setup.Session{R: runner, Opts: opts, DryRun: *dry}, setup.Steps(), show)
	}
	if *asJSON {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintln(errout, "could not write the report")
			return 1
		}
		fmt.Fprintln(out, string(data))
	} else {
		fmt.Fprintln(out, setupSummary(report))
	}
	if !report.OK {
		return 1
	}
	return 0
}

// setupSettings finds the network and protected setting: from the merged manifest of the environment when the directory has
// one, from the flags otherwise. A flag that disagrees with the manifest is an error.
func setupSettings(dir, env, network string, protected bool) (string, bool, error) {
	m, err := manifest.Load(dir, env)
	switch {
	case err == nil:
		if network != "" && network != m.Network {
			return "", false, fmt.Errorf("--network %s differs from the network %s in the manifest of %s", network, m.Network, env)
		}
		return m.Network, m.Protected || protected, nil
	case errors.Is(err, fs.ErrNotExist):
		if network == "" {
			return "", false, fmt.Errorf("there is no berthy.yml in %s: pass --network NAME", dir)
		}
		if !manifest.ValidNetwork(network) {
			return "", false, fmt.Errorf("invalid --network %q: lowercase letters, digits, - and _", network)
		}
		return network, protected, nil
	}
	return "", false, fmt.Errorf("manifest of %s: %v", env, err)
}

var setupGlyphs = map[string]string{
	setup.StatusOK:          "✓",
	setup.StatusChanged:     "✓",
	setup.StatusWouldChange: "·",
	setup.StatusSkipped:     "–",
	setup.StatusFailed:      "✖",
}

func printSetupStep(out io.Writer, r setup.StepResult) {
	fmt.Fprintf(out, "%s %-7s  %s\n", setupGlyphs[r.Status], terminalSafe(r.Step), terminalSafe(r.Detail))
}

// setupSummary is the last line of the text output.
func setupSummary(r setupReport) string {
	prefix := fmt.Sprintf("setup of %s on %s: ", terminalSafe(r.Environment), terminalSafe(r.Target))
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
