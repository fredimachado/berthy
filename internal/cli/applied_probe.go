package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"path"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"berthy/internal/scope"
	"berthy/internal/state"
)

func runHasAppliedApps(args []string, out, errout io.Writer) int {
	if len(args) != 3 || !scope.ValidProject(args[0]) || !scope.ValidEnvironment(args[1]) ||
		!path.IsAbs(args[2]) || path.Clean(args[2]) != args[2] || strings.ContainsRune(args[2], 0) {
		fmt.Fprintln(errout, "has-applied-apps: expected a project, an environment and an absolute clean state path")
		return 2
	}
	id := scope.ID{Project: args[0], Environment: args[1]}
	if err := id.CheckLength(); err != nil {
		fmt.Fprintln(errout, "has-applied-apps:", err)
		return 2
	}
	if runtime.GOOS != "linux" {
		fmt.Fprintln(errout, "has-applied-apps runs on the server")
		return 1
	}
	u, err := user.Lookup("berthy")
	if err != nil || u.Uid != strconv.Itoa(os.Geteuid()) {
		fmt.Fprintln(errout, "has-applied-apps: must run as the agent user")
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return printAppliedApps(ctx, args[2], id, out, errout)
}

func printAppliedApps(ctx context.Context, stateDir string, id scope.ID, out, errout io.Writer) int {
	applied, err := state.HasAppliedApps(ctx, stateDir, id)
	if err != nil {
		fmt.Fprintln(errout, "has-applied-apps: could not read applied apps")
		return 1
	}
	answer := "empty\n"
	if applied {
		answer = "applied\n"
	}
	if _, err := io.WriteString(out, answer); err != nil {
		fmt.Fprintln(errout, "has-applied-apps: cannot write answer")
		return 1
	}
	return 0
}
