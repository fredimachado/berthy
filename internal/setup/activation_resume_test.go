package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type resumeFixture struct{ dir, tools, state, lock, bin, cfg, calls string }

func newResumeFixture(t *testing.T, state string) resumeFixture {
	t.Helper()
	d, tools := shellFixture(t)
	f := resumeFixture{dir: d, tools: tools, state: filepath.Join(d, "state"), lock: filepath.Join(d, "lock"), bin: filepath.Join(d, "bin"), cfg: filepath.Join(d, "cfg"), calls: filepath.Join(d, "calls")}
	for path, body := range map[string]string{f.state: state + "\n", f.bin: "new-bin", f.bin + ".prev": "old-bin", f.cfg: "new-cfg", f.cfg + ".prev": "old-cfg"} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	shellClock(t, tools)
	shellTool(t, tools, "systemctl", `word=$(/bin/cat "$STATE"); echo "systemctl $* state=$word" >> "$CALLS"; case "$1" in show) echo active;; esac`)
	shellTool(t, tools, "berthy.setup-probe", `[ "$(/bin/cat "$BIN")" = old-bin ] || { `+expireProbe+`; }`)
	shellTool(t, tools, "mv", `word=$(/bin/cat "$STATE"); echo "mv $* state=$word" >> "$CALLS"; exec /bin/mv "$@"`)
	return f
}
func (f resumeFixture) run(t *testing.T, script string, args ...string) (int, string, string, string) {
	t.Helper()
	script = strings.NewReplacer(guardState, f.state, guardLock, f.lock, guardProbePath, filepath.Join(f.tools, "berthy.setup-probe")).Replace(script)
	cmd := exec.Command("sh", append([]string{"-c", script, "guard"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+f.tools+":"+os.Getenv("PATH"), "STATE="+f.state, "CALLS="+f.calls, "BIN="+f.bin, "CLOCK="+filepath.Join(f.dir, "clock"))
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ex *exec.ExitError
		if !errors.As(err, &ex) {
			t.Fatal(err)
		}
		code = ex.ExitCode()
	}
	state, _ := os.ReadFile(f.state)
	calls, _ := os.ReadFile(f.calls)
	return code, string(out), strings.TrimSpace(string(state)), string(calls)
}
func TestGuardStopsTimerAfterRecordingResult(t *testing.T) {
	f := newResumeFixture(t, "pending")
	code, out, state, calls := f.run(t, guardScript(f.bin, f.cfg, Staged{Binary: true, Config: true}, "x", 1), "yes", "yes", "now")
	if code != 0 || state != "restored" || !strings.Contains(calls, "systemctl stop berthy-activation-guard.timer state=restored") {
		t.Fatalf("code=%d output=%q state=%q calls=%q", code, out, state, calls)
	}
	for _, line := range strings.Split(calls, "\n") {
		if strings.HasPrefix(line, "systemctl stop berthy-activation-guard.timer ") && !strings.HasSuffix(line, "state=restored") {
			t.Fatalf("timer stopped before final result: %s", line)
		}
	}
}
func TestGuardResumesInterruptedRecovery(t *testing.T) {
	f := newResumeFixture(t, "recovering")
	os.WriteFile(f.bin, []byte("old-bin"), 0600)
	os.Remove(f.bin + ".prev")
	code, out, state, calls := f.run(t, guardScript(f.bin, f.cfg, Staged{Binary: true, Config: true}, "x", 1), "yes", "yes")
	cfg, _ := os.ReadFile(f.cfg)
	restart := strings.Index(calls, "systemctl restart berthy")
	move := strings.Index(calls, "mv -f "+f.cfg+".prev "+f.cfg)
	if code != 0 || state != "restored" || string(cfg) != "old-cfg" || move < 0 || !strings.Contains(calls, "mv -f "+f.cfg+".prev "+f.cfg+" state=recovering") || restart < move || strings.Count(calls, "systemctl restart berthy") != 1 {
		t.Fatalf("code=%d output=%q state=%q cfg=%q calls=%q", code, out, state, cfg, calls)
	}
}
func TestGuardNowRunsRecoveryBeforeRestart(t *testing.T) {
	for _, previous := range []bool{true, false} {
		t.Run(map[bool]string{true: "restore", false: "stop"}[previous], func(t *testing.T) {
			f := newResumeFixture(t, "pending")
			arg := "no"
			if previous {
				arg = "yes"
			}
			code, out, state, calls := f.run(t, guardScript(f.bin, f.cfg, Staged{Binary: true, Config: true}, "x", 1), arg, "no", "now")
			if previous {
				move := strings.Index(calls, "mv -f "+f.bin+".prev "+f.bin)
				restart := strings.Index(calls, "systemctl restart berthy")
				if code != 0 || state != "restored" || move < 0 || !strings.Contains(calls, "mv -f "+f.bin+".prev "+f.bin+" state=recovering") || restart < move || strings.Count(calls, "systemctl restart berthy") != 1 {
					t.Fatalf("code=%d out=%q state=%q calls=%q", code, out, state, calls)
				}
			} else if code != 0 || state != "stopped" || !strings.Contains(calls, "systemctl stop berthy state=recovering") || strings.Contains(calls, "systemctl restart berthy") {
				t.Fatalf("code=%d out=%q state=%q calls=%q", code, out, state, calls)
			}
		})
	}
}
func TestGuardNowLeavesFinalStateAlone(t *testing.T) {
	f := newResumeFixture(t, "restored")
	code, out, state, calls := f.run(t, guardScript(f.bin, f.cfg, Staged{Binary: true}, "x", 1), "yes", "no", "now")
	if code != 0 || state != "restored" || strings.Contains(calls, "systemctl") || strings.Contains(calls, "mv ") {
		t.Fatalf("code=%d output=%q state=%q calls=%q", code, out, state, calls)
	}
}
func TestConfirmedWriteFailureHasDistinctExit(t *testing.T) {
	f := newResumeFixture(t, "pending")
	shellTool(t, f.tools, "mktemp", "exit 1")
	shellTool(t, f.tools, "berthy.setup-probe", "exit 0")
	code, out, state, calls := f.run(t, restartScript("x", 1))
	if code != 4 || state != "pending" || strings.Contains(calls, "systemctl stop berthy-activation-guard.timer") {
		t.Fatalf("code=%d output=%q state=%q calls=%q", code, out, state, calls)
	}
}

type recordFailureRunner struct {
	calls    []string
	previous bool
}

func (*recordFailureRunner) Target() string { return "fixture" }
func (r *recordFailureRunner) Run(_ context.Context, s string, _ io.Reader) (Result, error) {
	r.calls = append(r.calls, s)
	if strings.Contains(s, ".new ") {
		if r.previous {
			return Result{Stdout: []byte("prev_cfg=yes\n")}, nil
		}
		return Result{}, nil
	}
	if strings.Contains(s, "systemctl restart berthy") {
		return Result{Code: 4}, nil
	}
	return Result{}, nil
}
func TestActivateReportsConfirmedWriteFailure(t *testing.T) {
	for _, tc := range []struct {
		previous bool
		outcome  ConfigOutcome
	}{{true, ConfigUnconfirmed}, {false, ConfigRetained}} {
		t.Run(fmt.Sprint(tc.previous), func(t *testing.T) {
			r := &recordFailureRunner{previous: tc.previous}
			err := Activate(context.Background(), r, "x", Staged{Config: true}, time.Second)
			var ae *ActivateError
			if !errors.As(err, &ae) || ae.ConfigOutcome != tc.outcome || !strings.Contains(err.Error(), "agent answered") || !strings.Contains(err.Error(), "could not record") || !strings.Contains(err.Error(), "timer") || len(r.calls) != 2 {
				t.Fatalf("err=%v calls=%q", err, r.calls)
			}
		})
	}
}
func TestOwnerErrorReportsUncertainRecovery(t *testing.T) {
	r := &ownerResultRunner{fail: true, previous: true}
	err := Activate(context.Background(), r, "x", Staged{Config: true, Binary: true}, time.Second)
	var ae *ActivateError
	if !errors.As(err, &ae) || ae.ConfigOutcome != ConfigUnconfirmed || !strings.Contains(err.Error(), "timer decides otherwise") {
		t.Fatalf("err=%v", err)
	}
}
func TestPriorRecoveryResultsIncludeJournal(t *testing.T) {
	for _, word := range []string{"restored", "restored-down", "stopped"} {
		t.Run(word, func(t *testing.T) {
			r := &stateResultRunner{word: word, journal: "last agent log\n"}
			err := Activate(context.Background(), r, "x", Staged{Config: true, Binary: true}, time.Second)
			if err == nil || !strings.Contains(err.Error(), "last agent log") {
				t.Fatalf("missing journal: %v", err)
			}
			if len(r.calls) != 3 || r.calls[2] != "journalctl -u berthy -n 20 --no-pager -o cat 2>&1" {
				t.Fatalf("calls=%q", r.calls)
			}
		})
	}
}

func TestPriorRecoveryResultSurvivesJournalError(t *testing.T) {
	for _, tc := range []struct {
		word, message string
		outcome       ConfigOutcome
	}{
		{"restored", "previous one was put back", ConfigPrevious},
		{"restored-down", "does not answer either", ConfigPrevious},
		{"stopped", "left stopped", ConfigRetained},
		{"stop-failed", "could not be stopped", ConfigRetained},
		{"restore-failed", "could not be put back", ConfigRetained},
	} {
		t.Run(tc.word, func(t *testing.T) {
			r := &stateResultRunner{word: tc.word, journalErr: errors.New("journal unavailable")}
			err := Activate(context.Background(), r, "x", Staged{Config: true, Binary: true}, time.Second)
			var ae *ActivateError
			if !errors.As(err, &ae) || ae.ConfigOutcome != tc.outcome || !strings.Contains(err.Error(), tc.message) || len(r.calls) != 3 {
				t.Fatalf("error=%v calls=%q", err, r.calls)
			}
		})
	}
}
