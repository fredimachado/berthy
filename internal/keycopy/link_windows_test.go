//go:build windows

package keycopy

import "os/exec"

func plantLink(link, target string) error {
	return exec.Command("cmd", "/c", "mklink", "/J", link, target).Run()
}
