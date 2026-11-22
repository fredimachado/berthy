// Package tui is "berthy ui": a full-screen terminal interface over the agent API. It reads what the CLI reads, with the CLI's
// configuration and token, and shows apps, logs, server meters and the changes AI agents have asked for.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/scope"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"
)

const usage = "usage: ui -e ENV [--project NAME] [--config CONFIG]"

// Run is "berthy ui -e ENV [--project NAME] [--config CONFIG]": it opens the interface on the terminal and returns when the
// person quits. It refuses to run when the standard input or output is not a terminal, and it reads the configuration first, so
// a missing file is named even then. No line it prints holds the token.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var env, config, project string
	var projectGiven bool
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-e", "--config", "--project":
			if i+1 == len(args) {
				fmt.Fprintf(stderr, "missing value for %s\n%s\n", args[i], usage)
				return 2
			}
			i++
			switch args[i-1] {
			case "-e":
				env = args[i]
			case "--project":
				project = args[i]
				projectGiven = true
			default:
				config = args[i]
			}
		default:
			fmt.Fprintf(stderr, "unknown argument %s\n%s\n", args[i], usage)
			return 2
		}
	}
	if !scope.ValidEnvironment(env) {
		fmt.Fprintln(stderr, "valid -e environment required")
		return 2
	}
	if projectGiven && !scope.ValidProject(project) {
		fmt.Fprintln(stderr, "valid --project required")
		return 2
	}
	path, err := agentclient.ConfigPath(config)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	target, err := agentclient.Resolve(path, agentclient.Selector{Project: project, Environment: env, ManifestDir: "."})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !isTerminal(stdin) || !isTerminal(stdout) {
		fmt.Fprintln(stderr, "berthy ui needs a terminal")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	model := NewModel(newClient(ctx, target), env, time.Now)
	program := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(stdin), tea.WithOutput(stdout))
	_, err = program.Run()
	model.shutdown()
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, tea.ErrProgramKilled) {
		fmt.Fprintln(stderr, "ui stopped:", target.Redact(err.Error()))
		return 1
	}
	return 0
}

// isTerminal reports whether a stream is a terminal.
func isTerminal(v any) bool {
	f, ok := v.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(f.Fd())
}
