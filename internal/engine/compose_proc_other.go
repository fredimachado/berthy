//go:build !linux

package engine

import (
	"os"
	"os/exec"
)

// prepareCommand changes nothing off Linux. The agent runs on Linux only.
func prepareCommand(*exec.Cmd) {}

// killCommand kills the process of cmd. Off Linux there is no process group to kill.
func killCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil && err != os.ErrProcessDone {
		return err
	}
	return nil
}
