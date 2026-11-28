package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"runtime"
	"strconv"

	"berthy/internal/agent"
	"berthy/internal/api"
	"berthy/internal/setup"

	"go.yaml.in/yaml/v3"
)

func readClusterAlerts(path string) (*agent.ClusterAlerts, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read cluster alerts file: %w", err)
	}
	defer f.Close()
	const max = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || len(raw) > max {
		return nil, errors.New("cluster alerts file is unreadable or larger than 1 MiB")
	}
	decode := yaml.NewDecoder(bytes.NewReader(raw))
	decode.KnownFields(true)
	var alerts *agent.ClusterAlerts
	var extra any
	if err := decode.Decode(&alerts); err != nil || alerts == nil || decode.Decode(&extra) != io.EOF {
		return nil, errors.New("cluster alerts file must contain one YAML mapping with only reminder, telegram and webhook settings")
	}
	if err := alerts.Validate(); err != nil {
		return nil, fmt.Errorf("invalid cluster alerts: %w", err)
	}
	return alerts, nil
}

func (f setupFlags) clusterSetupCommand(project string) string {
	command := "berthy setup -e " + shellWord(f.env) + " --project " + shellWord(project)
	switch {
	case f.local:
		command += " --local"
	case f.server != "":
		command += " --server " + shellWord(f.server)
	default:
		command += " --host " + shellWord(f.host)
	}
	if f.dirGiven {
		command += " --manifest-dir " + shellWord(f.dir)
	}
	return command
}

func runSetupClusterSecret(ctx context.Context, f setupFlags, from setupFrom, stdin io.Reader, out, errout io.Writer) int {
	report := setupReport{Project: from.project, Environment: f.env, Target: f.host, DryRun: f.dry, clusterSecret: true}
	if f.local {
		report.Target = "this server"
	}
	progress := newProgressLine(errout, !f.asJSON && !f.verbose && stderrTerminal(errout))
	show := func(r setup.StepResult) {
		report.Steps = append(report.Steps, r)
		if !f.asJSON {
			progress.Finish(out, r, !f.verbose && stderrTerminal(out))
		}
	}
	runner, err := newSetupRunner(f.host, f.local)
	if err != nil {
		show(setup.StepResult{Step: "connect", Status: setup.StatusFailed, Detail: terminalSafe(err.Error())})
	} else {
		report.Target = runner.Target()
		s := &setup.Session{R: runner, DryRun: f.dry, Opts: setup.Options{StepStarted: progress.Start}}
		if f.verbose {
			s.Log = setupCommandLog(errout, f.asJSON)
			s.R = setup.Recording(runner, s.Log)
		}
		step := setup.Step{Name: "cluster_secret", Run: func(ctx context.Context, s *setup.Session) (string, string, error) {
			if f.dry {
				err := s.PreviewClusterSecret(ctx, f.clusterSecret)
				return setup.StatusWouldChange, "would stop berthy.service, store " + f.clusterSecret + ", then start and verify the service", err
			}
			value, err := clusterSecretInput(ctx, stdin, f.clusterSecret, errout)
			if err != nil {
				return "", "", err
			}
			defer clear(value)
			err = setup.SetClusterSecret(ctx, s.R, f.clusterSecret, value)
			return setup.StatusChanged, "stored " + f.clusterSecret + " and verified berthy.service", err
		}}
		report.OK = setup.Run(ctx, s, []setup.Step{step}, show)
	}
	progress.Close()
	return writeSetupReport(ctx, f, from, report, nil, out, errout)
}

func clusterSecretInput(ctx context.Context, stdin io.Reader, name string, errout io.Writer) ([]byte, error) {
	if stdin == os.Stdin {
		if read, interactive := hiddenInput(); interactive {
			fmt.Fprintf(errout, "Value of %s (hidden): ", name)
			data, err := read(ctx)
			fmt.Fprintln(errout)
			return normalizeClusterSecret(data, err)
		}
		stop := exitOnPipeSignal(ctx)
		defer stop()
	}
	value, err := readClusterSecretValue(stdin)
	if ctx.Err() != nil {
		clear(value)
		return nil, errors.New("cluster credential input interrupted")
	}
	return value, err
}

func readClusterSecretValue(stdin io.Reader) ([]byte, error) {
	return normalizeClusterSecret(io.ReadAll(io.LimitReader(stdin, api.MaxSecretValue+3)))
}

func normalizeClusterSecret(raw []byte, err error) ([]byte, error) {
	if err != nil {
		clear(raw)
		return nil, errors.New("could not read the cluster credential")
	}
	line, single := oneLine(raw)
	if !single || len(line) == 0 || len(line) > api.MaxSecretValue || bytes.IndexByte(line, 0) >= 0 {
		clear(raw)
		return nil, errors.New("the cluster credential must be a nonempty single line of at most 64 KiB")
	}
	return line, nil
}

func runSetupClusterSecretWriter(args []string, stdin io.Reader, errout io.Writer) int {
	if len(args) != 1 || api.CheckSecretName(args[0]) != nil {
		fmt.Fprintln(errout, "setup-cluster-secret: expected one valid secret name")
		return 2
	}
	if runtime.GOOS != "linux" {
		fmt.Fprintln(errout, "setup-cluster-secret runs on the server")
		return 1
	}
	u, err := user.Lookup("berthy")
	if err != nil || u.Uid != strconv.Itoa(os.Geteuid()) {
		fmt.Fprintln(errout, "setup-cluster-secret: must run as the agent user")
		return 1
	}
	f, err := agent.LoadFile("/etc/berthy/agent.json")
	if err != nil {
		fmt.Fprintln(errout, "setup-cluster-secret: cannot load the installed agent configuration")
		return 1
	}
	if err := storeSetupClusterSecret(f.Cluster, args[0], stdin); err != nil {
		fmt.Fprintln(errout, "setup-cluster-secret:", err)
		return 1
	}
	return 0
}

func storeSetupClusterSecret(cluster agent.ClusterConfig, name string, stdin io.Reader) error {
	const maxFrame = api.MaxSecretValue + len(setup.ClusterSecretEnd)
	raw, err := io.ReadAll(io.LimitReader(stdin, int64(maxFrame)+1))
	defer clear(raw)
	value, complete := bytes.CutSuffix(raw, []byte(setup.ClusterSecretEnd))
	if err != nil || !complete || len(raw) > maxFrame {
		return errors.New("invalid credential input")
	}
	value, err = normalizeClusterSecret(value, nil)
	if err != nil {
		return errors.New("invalid credential input")
	}
	if err := agent.WriteClusterSecret(context.Background(), cluster, name, value); err != nil {
		return errors.New("cannot store the cluster credential")
	}
	return nil
}
