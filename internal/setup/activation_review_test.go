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
	return d, tools
}
func shellTool(t *testing.T, tools, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(tools, name), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
}
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
			script := strings.ReplaceAll(strings.ReplaceAll(r.calls[0], binPath, bin), guardPath, guard)
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
	shellTool(t, tools, "date", `[ ! -f "$CLOCK" ] || cat "$CLOCK"; [ -f "$CLOCK" ] || echo 0`)
	shellTool(t, tools, "curl", `echo "$*" >> "$CALLS"; n=0; [ ! -f "$CLOCK" ] || n=$(cat "$CLOCK"); echo $((n+1)) > "$CLOCK"; exit 1`)
	shellTool(t, tools, "systemctl", "exit 1")
	shellTool(t, tools, "sleep", `n=$(cat "$CLOCK"); echo $((n+1)) > "$CLOCK"`)
	_ = runShell(t, waitScript("http://x", 3), "", tools, dir)
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], "--max-time 1") {
		t.Fatalf("probes do not respect elapsed time: %q", calls)
	}
}
func TestRestartDisarmsOnlyAfterHealthAnswers(t *testing.T) {
	for _, answer := range []bool{false, true} {
		t.Run(map[bool]string{false: "unhealthy", true: "healthy"}[answer], func(t *testing.T) {
			dir, tools := shellFixture(t)
			shellTool(t, tools, "date", "echo 0")
			shellTool(t, tools, "systemctl", `echo "$*" >> "$CALLS"; case "$1" in is-failed) exit 0;; esac; exit 0`)
			curl := "exit 1"
			if answer {
				curl = "exit 0"
			}
			shellTool(t, tools, "curl", curl)
			err := runShell(t, restartScript("x", 1), "", tools, dir)
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
	shellTool(t, tools, "systemctl", `case "$1" in stop) exit 42;; is-failed) exit 0;; esac; exit 0`)
	shellTool(t, tools, "curl", "exit 1")
	err := runShell(t, guardScript("/bin/x", "/etc/x", Staged{Binary: true}, "x", 1)+"\n", "", tools, dir)
	if err == nil {
		t.Fatal("guard hid failed stop")
	}
}

func TestFailedRecoveryKeepsTheGuardArmed(t *testing.T) {
	r := &guardRunner{}
	err := Activate(context.Background(), r, "x", Staged{Config: true}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "62 seconds") || r.calls[len(r.calls)-1] == disarmScript {
		t.Fatalf("unresolved recovery: %v; %q", err, r.calls)
	}
}
func TestRestartDoesNotConfirmWhenTimerStopFails(t *testing.T) {
	dir, tools := shellFixture(t)
	shellTool(t, tools, "date", "echo 0")
	shellTool(t, tools, "systemctl", `case "$1" in stop) exit 42;; esac; exit 0`)
	shellTool(t, tools, "curl", "exit 0")
	if err := runShell(t, restartScript("x", 1), "", tools, dir); err == nil {
		t.Fatal("restart confirmed after timer stop failed")
	}
}

func TestWaitDoesNotSleepAfterDeadline(t *testing.T) {
	dir, tools := shellFixture(t)
	shellTool(t, tools, "date", `[ ! -f "$CLOCK" ] || cat "$CLOCK"; [ -f "$CLOCK" ] || echo 0`)
	shellTool(t, tools, "curl", `echo 3 > "$CLOCK"; exit 1`)
	shellTool(t, tools, "systemctl", "exit 1")
	shellTool(t, tools, "sleep", `echo slept >> "$CALLS"`)
	_ = runShell(t, waitScript("http://x", 3), "", tools, dir)
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if strings.Contains(string(calls), "slept") {
		t.Fatal("slept after the deadline")
	}
}
