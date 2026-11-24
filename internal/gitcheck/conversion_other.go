//go:build !windows

package gitcheck

import (
	"context"
	"os/exec"
)

func runConversion(_ context.Context, cmd *exec.Cmd) error { return cmd.Run() }
