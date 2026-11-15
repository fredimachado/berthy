//go:build linux

package engine

import (
	"os/exec"
	"syscall"
)

// prepareCommand puts cmd in its own process group and asks the kernel to SIGKILL it when the agent dies, so a Compose
// command cannot outlive the agent that started it.
func prepareCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

// killCommand SIGKILLs the process group of cmd, so every process cmd started dies with it.
func killCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}
