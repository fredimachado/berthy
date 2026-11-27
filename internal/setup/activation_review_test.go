package setup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func shellFixture(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires sh")
	}
	d := t.TempDir()
	tools := filepath.Join(d, "tools")
	if err := os.Mkdir(tools, 0700); err != nil {
		t.Fatal(err)
	}
	shellTool(t, tools, "flock", "exit 0")
	shellTool(t, tools, "setpriv", `shift 3; exec "$@"`)
	shellTool(t, tools, "sleep", `n=$(date +%s); echo $((n+1)) > "$CLOCK"`)
	return d, tools
}
func shellTool(t *testing.T, tools, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(tools, name), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
}

func shellClock(t *testing.T, tools string) {
	t.Helper()
	shellTool(t, tools, "date", `if [ -f "$CLOCK" ]; then cat "$CLOCK"; else echo 0; fi`)
}

const expireProbe = `n=$(date +%s); echo $((n+1)) > "$CLOCK"; exit 1`

func runShell(t *testing.T, script, input, tools, dir string) error {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = append(os.Environ(), "PATH="+tools+":"+os.Getenv("PATH"), "CALLS="+filepath.Join(dir, "calls"), "CLOCK="+filepath.Join(dir, "clock"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("shell: %v: %s", err, output)
	}
	return err
}
func TestSwapRejectsTruncatedGuard(t *testing.T) {
	for _, truncated := range []bool{true, false} {
		t.Run(map[bool]string{true: "truncated", false: "complete"}[truncated], func(t *testing.T) {
			dir, tools := shellFixture(t)
			bin := filepath.Join(dir, "bin")
			guard := filepath.Join(dir, "guard")
			os.WriteFile(bin, []byte("old"), 0600)
			os.WriteFile(bin+".new", []byte("new"), 0600)
			shellTool(t, tools, "systemctl", "exit 0")
			shellTool(t, tools, "systemd-run", "echo armed >> \"$CALLS\"")
			r := &guardRunner{stage: "up"}
			_ = Activate(context.Background(), r, "x", Staged{Binary: true}, time.Second)
			script := strings.NewReplacer(binPath, bin, guardPath, guard, guardState, filepath.Join(dir, "state"),
				guardLock, filepath.Join(dir, "lock"), guardReason, filepath.Join(dir, "reason")).Replace(r.calls[0])
			input := r.input
			if truncated {
				input = input[:len(input)/2]
			}
			err := runShell(t, script, input, tools, dir)
			if (err != nil) != truncated {
				t.Errorf("swap error = %v", err)
			}
			b, _ := os.ReadFile(bin)
			want := "new"
			if truncated {
				want = "old"
			}
			if string(b) != want {
				t.Errorf("binary = %q, want %q", b, want)
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
			if strings.Contains(string(calls), "armed") == truncated {
				t.Errorf("arm calls = %q", calls)
			}
		})
	}
}
func TestWaitScriptUsesElapsedDeadline(t *testing.T) {
	dir, tools := shellFixture(t)
	shellClock(t, tools)
	shellTool(t, tools, "probe", `echo "$*" >> "$CALLS"; `+expireProbe)
	shellTool(t, tools, "systemctl", "echo active")
	shellTool(t, tools, "sleep", `n=$(cat "$CLOCK"); echo $((n+1)) > "$CLOCK"`)
	if err := runShell(t, waitScriptOnSuccess(filepath.Join(tools, "probe"), "x", 3, "exit 0"), "", tools, dir); err == nil {
		t.Fatal("an active but silent agent passed the wait")
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "--timeout 3s") || !strings.Contains(lines[1], "--timeout 1s") {
		t.Fatalf("probes do not respect elapsed time: %q", calls)
	}
}
func TestRestartDisarmsOnlyAfterHealthAnswers(t *testing.T) {
	for _, answer := range []bool{false, true} {
		t.Run(map[bool]string{false: "unhealthy", true: "healthy"}[answer], func(t *testing.T) {
			dir, tools := shellFixture(t)
			shellClock(t, tools)
			shellTool(t, tools, "systemctl", `echo "$*" >> "$CALLS"; case "$1" in show) echo active ;; esac`)
			probe := expireProbe
			if answer {
				probe = "exit 0"
			}
			shellTool(t, tools, "probe", probe)
			os.WriteFile(filepath.Join(dir, "state"), []byte("pending\n"), 0600)
			script := strings.NewReplacer(guardState, filepath.Join(dir, "state"), guardLock, filepath.Join(dir, "lock"),
				guardProbePath, filepath.Join(tools, "probe")).Replace(restartScript("x", 1))
			err := runShell(t, script, "", tools, dir)
			if (err == nil) != answer {
				t.Fatalf("restart error = %v", err)
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
			if strings.Contains(string(calls), "stop berthy-activation-guard.timer") != answer {
				t.Fatalf("timer stop = %q", calls)
			}
		})
	}
}
func TestGuardReportsFirstInstallStopFailure(t *testing.T) {
	dir, tools := shellFixture(t)
	shellTool(t, tools, "date", "echo 0")
	shellTool(t, tools, "systemctl", `case "$1" in stop) exit 42;; show) echo failed;; esac`)
	os.WriteFile(filepath.Join(dir, "state"), []byte("pending\n"), 0600)
	script := strings.NewReplacer(guardState, filepath.Join(dir, "state"), guardLock, filepath.Join(dir, "lock")).Replace(guardScript("/bin/x", "/etc/x", Staged{Binary: true}, "x", 1))
	err := runShell(t, script, "", tools, dir)
	state, _ := os.ReadFile(filepath.Join(dir, "state"))
	if err == nil || string(state) != "stop-failed\n" {
		t.Fatalf("guard hid failed stop: error=%v, state=%q", err, state)
	}
}

func TestWaitDoesNotSleepAfterDeadline(t *testing.T) {
	dir, tools := shellFixture(t)
	shellClock(t, tools)
	shellTool(t, tools, "probe", `echo probed >> "$CALLS"; echo 3 > "$CLOCK"; exit 1`)
	shellTool(t, tools, "systemctl", "echo active")
	shellTool(t, tools, "sleep", `echo slept >> "$CALLS"`)
	err := runShell(t, waitScriptOnSuccess(filepath.Join(tools, "probe"), "x", 3, "exit 0"), "", tools, dir)
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if err == nil || string(calls) != "probed\n" {
		t.Fatalf("wait must fail without sleeping after the probe runs out of time: error=%v, calls=%q", err, calls)
	}
}
