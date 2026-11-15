//go:build linux

package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// processScript is the shell a process-group test runs. It records its own pid and the pid of a sleeping grandchild in
// the file named by $0, then waits. Compose.Run appends its own arguments after the script, which the script ignores.
const processScript = `echo $$ > "$0"; sleep 30 & echo $! >> "$0"; wait`

func TestPrepareCommandSetsGroupAndParentDeathSignal(t *testing.T) {
	cmd := exec.Command("true")
	prepareCommand(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid || cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("process attributes %+v", cmd.SysProcAttr)
	}
}

// readPids waits until the file holds at least n pids, one per line, and returns them.
func readPids(t *testing.T, path string, n int) []int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(path)
		var pids []int
		for _, field := range strings.Fields(string(b)) {
			if v, err := strconv.Atoi(field); err == nil {
				pids = append(pids, v)
			}
		}
		if len(pids) >= n {
			return pids
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pids not written to %s", path)
	return nil
}

// goneOrZombie reports whether pid no longer runs. A zombie has exited, so it counts as gone.
func goneOrZombie(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	i := bytes.LastIndexByte(b, ')')
	return i >= 0 && i+2 < len(b) && b[i+2] == 'Z'
}

func waitGone(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !goneOrZombie(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) still running", what, pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// killIfRunning is a test cleanup: it stops a process a failing test left behind.
func killIfRunning(t *testing.T, pid int) {
	t.Cleanup(func() {
		if !goneOrZombie(pid) {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

func TestComposeCancelKillsTheWholeProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pids")
	c := Compose{Command: []string{"sh", "-c", processScript, pidFile}, Host: "unix:///run/docker.sock"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := c.Run(ctx, ComposeRun{Project: "shop-db", Dir: dir, Args: []string{"wait"}})
		done <- err
	}()
	pids := readPids(t, pidFile, 2)
	shell, grandchild := pids[0], pids[1]
	killIfRunning(t, grandchild)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled run reported success")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	waitGone(t, shell, "compose shell after cancel")
	waitGone(t, grandchild, "grandchild after cancel")
}

// TestParentDeathHelper is the agent that TestComposeChildDiesWithItsParent starts and kills. It runs a Compose command
// and waits for it. It does nothing unless the sentinel argument is present.
func TestParentDeathHelper(t *testing.T) {
	at := -1
	for i, a := range os.Args {
		if a == "parent-death-helper" {
			at = i
			break
		}
	}
	if at < 0 || at+1 >= len(os.Args) {
		return
	}
	pidFile := os.Args[at+1]
	c := Compose{Command: []string{"sh", "-c", processScript, pidFile}, Host: "unix:///run/docker.sock"}
	_, _, _ = c.Run(context.Background(), ComposeRun{Project: "shop-db", Dir: filepath.Dir(pidFile), Args: []string{"wait"}})
	os.Exit(0)
}

func TestComposeChildDiesWithItsParent(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pids")
	helper := exec.Command(os.Args[0], "-test.run=^TestParentDeathHelper$", "--", "parent-death-helper", pidFile)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	pids := readPids(t, pidFile, 2)
	shell, grandchild := pids[0], pids[1]
	killIfRunning(t, grandchild)
	// The agent dies without any chance to clean up, as after SIGKILL or a crash.
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	helper.Wait()
	waitGone(t, shell, "compose shell after its agent was killed")
}
