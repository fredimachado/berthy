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

const testStatePath = "/run/berthy-activation-guard.state"
const testLockPath = "/run/berthy-activation-guard.lock"

func stateShell(t *testing.T, script, word, mode string, args ...string) (int, string, string, string) {
	t.Helper()
	dir, tools := shellFixture(t)
	state := filepath.Join(dir, "state")
	lock := filepath.Join(dir, "lock")
	calls := filepath.Join(dir, "calls")
	if word != "" || mode == "empty-state" {
		if err := os.WriteFile(state, []byte(word+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "unreadable-state" {
		shellTool(t, tools, "cat", "exit 1")
	}
	shellTool(t, tools, "date", "echo 0")
	ctl := `echo "$*" >> "$CALLS"; case "$1" in is-failed) exit 0;; esac; exit 0`
	if mode == "timer-fails" {
		ctl = `echo "$*" >> "$CALLS"; case "$1" in stop) exit 42;; esac; exit 0`
	}
	if mode == "stop-fails" {
		ctl = `echo "$*" >> "$CALLS"; case "$*" in 'stop berthy') exit 42;; is-failed*) exit 0;; esac; exit 0`
	}
	shellTool(t, tools, "systemctl", ctl)
	curl := "exit 0"
	if mode == "down" || mode == "stop-fails" || mode == "move-fails" {
		curl = "exit 1"
	}
	if mode == "restore" {
		curl = `if [ -f "$CALLS.probed" ]; then exit 0; fi; touch "$CALLS.probed"; exit 1`
	}
	shellTool(t, tools, "curl", curl)
	if mode == "lock-timeout" {
		shellTool(t, tools, "flock", `echo "flock $*" >> "$CALLS"; exit 1`)
	}
	if mode == "no-actions" {
		shellTool(t, tools, "mv", `echo "mv $*" >> "$CALLS"; exec /bin/mv "$@"`)
	}
	if mode == "move-fails" {
		shellTool(t, tools, "mv", `case "$2" in *.prev) exit 42;; esac; exec /bin/mv "$@"`)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.WriteFile(bin, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin+".prev", []byte("previous"), 0600); err != nil {
		t.Fatal(err)
	}
	script = strings.NewReplacer(testStatePath, state, testLockPath, lock, "/nonexistent/bin", bin, "/nonexistent/cfg", filepath.Join(dir, "cfg")).Replace(script)
	cmd := exec.Command("sh", append([]string{"-c", script, "guard"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+tools+":"+os.Getenv("PATH"), "CALLS="+calls)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ex *exec.ExitError
		if !errors.As(err, &ex) {
			t.Fatal(err)
		}
		code = ex.ExitCode()
	}
	if (mode == "restore" || mode == "down") && len(args) > 0 && args[0] == "yes" {
		got, err := os.ReadFile(bin)
		if err != nil || string(got) != "previous" {
			t.Errorf("previous binary not restored: %q, %v", got, err)
		}
	}
	result, _ := os.ReadFile(state)
	log, _ := os.ReadFile(calls)
	return code, string(out), strings.TrimSpace(string(result)), string(log)
}
func TestLockedRestartConfirmsDespiteTimerStopFailure(t *testing.T) {
	code, out, state, _ := stateShell(t, restartScript("x", 1), "pending", "timer-fails")
	if code != 0 || state != "confirmed" {
		t.Fatalf("code=%d output=%q state=%q", code, out, state)
	}
}
func TestLockedRestartReadsCompletedState(t *testing.T) {
	for _, word := range []string{"confirmed", "kept", "restored", "restored-down", "stopped", "stop-failed", "restore-failed"} {
		t.Run(word, func(t *testing.T) {
			code, out, state, calls := stateShell(t, restartScript("x", 1), word, "no-actions")
			if code != 3 || !strings.Contains(out, "guard="+word) || state != word || strings.Contains(calls, "restart berthy") || strings.Contains(calls, "stop berthy") || strings.Contains(calls, "mv ") {
				t.Fatalf("code=%d output=%q calls=%q", code, out, calls)
			}
		})
	}
}
func TestLockedRestartGivesUpWhenLockIsHeld(t *testing.T) {
	code, out, _, calls := stateShell(t, restartScript("x", 1), "pending", "lock-timeout")
	if code != 3 || !strings.Contains(out, "guard=running") || !strings.Contains(calls, "flock -w 1 9") || strings.Contains(calls, "restart berthy") {
		t.Fatalf("code=%d output=%q calls=%q", code, out, calls)
	}
}
func TestGuardLeavesConfirmedActivationAlone(t *testing.T) {
	code, out, state, calls := stateShell(t, guardScript("/nonexistent/bin", "/nonexistent/cfg", Staged{Binary: true}, "x", 1), "confirmed", "no-actions", "yes", "no")
	if code != 0 || state != "confirmed" || strings.Contains(calls, "restart berthy") || strings.Contains(calls, "stop berthy\n") || strings.Contains(calls, "mv ") {
		t.Fatalf("code=%d output=%q state=%q calls=%q", code, out, state, calls)
	}
}
func TestGuardDoesNotActOnFinalState(t *testing.T) {
	for _, word := range []string{"kept", "restored", "restored-down", "stopped", "stop-failed", "restore-failed"} {
		t.Run(word, func(t *testing.T) {
			code, out, state, calls := stateShell(t, guardScript("/nonexistent/bin", "/nonexistent/cfg", Staged{Binary: true}, "x", 1), word, "no-actions", "yes", "no")
			if code != 0 || !strings.Contains(out, "guard="+word) || state != word || strings.Contains(calls, "restart berthy") || strings.Contains(calls, "stop berthy") || strings.Contains(calls, "mv ") {
				t.Fatalf("code=%d out=%q state=%q calls=%q", code, out, state, calls)
			}
		})
	}
}
func TestGuardRecordsItsDecision(t *testing.T) {
	for _, tc := range []struct{ name, mode, bin, cfg, want string }{{"kept", "", "no", "no", "kept"}, {"restored", "restore", "yes", "no", "restored"}, {"restored-down", "down", "yes", "no", "restored-down"}, {"stopped", "down", "no", "no", "stopped"}, {"stop-failed", "stop-fails", "no", "no", "stop-failed"}, {"restore-failed", "move-fails", "yes", "no", "restore-failed"}} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, state, _ := stateShell(t, guardScript("/nonexistent/bin", "/nonexistent/cfg", Staged{Binary: true}, "x", 1), "pending", tc.mode, tc.bin, tc.cfg)
			failed := tc.want == "restored-down" || tc.want == "stop-failed" || tc.want == "restore-failed"
			if state != tc.want || (code != 0) != failed {
				t.Fatalf("code=%d output=%q state=%q want=%q", code, out, state, tc.want)
			}
		})
	}
}
func TestWgetProbeIsBoundedAsAWhole(t *testing.T) {
	dir, tools := shellFixture(t)
	shellTool(t, tools, "date", "echo 0")
	shellTool(t, tools, "systemctl", "exit 0")
	shellTool(t, tools, "wget", `echo "wget $*" >> "$CALLS"; exit 1`)
	shellTool(t, tools, "timeout", `echo "timeout $*" >> "$CALLS"; shift; exec "$@"`)
	cmd := exec.Command("sh", "-c", waitScript("http://x", 3))
	cmd.Env = append(os.Environ(), "PATH="+tools, "CALLS="+filepath.Join(dir, "calls"))
	_ = cmd.Run()
	data, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if !strings.Contains(string(data), "timeout 3 wget") || !strings.Contains(string(data), "--tries=1") {
		t.Fatalf("wget not bounded: %q", data)
	}
}

type stateResultRunner struct {
	word       string
	journal    string
	journalErr error
	calls      []string
	failOwner  bool
}

func (*stateResultRunner) Target() string { return "fixture" }
func (r *stateResultRunner) Run(_ context.Context, s string, _ io.Reader) (Result, error) {
	r.calls = append(r.calls, s)
	if strings.HasPrefix(s, "journalctl") {
		return Result{Stdout: []byte(r.journal)}, r.journalErr
	}
	if strings.Contains(s, ".new ") {
		return Result{Stdout: []byte("prev_bin=yes\nprev_cfg=yes\n")}, nil
	}
	if strings.Contains(s, "systemctl restart berthy") {
		return Result{Code: 3, Stdout: []byte("guard=" + r.word + "\n")}, nil
	}
	return Result{}, nil
}
func TestActivateMapsGuardStates(t *testing.T) {
	for _, tc := range []struct {
		word    string
		ok      bool
		outcome ConfigOutcome
		text    string
	}{{"confirmed", true, "", ""}, {"kept", true, "", ""}, {"restored", false, ConfigPrevious, "previous one was put back"}, {"restored-down", false, ConfigPrevious, "does not answer either"}, {"stopped", false, ConfigRetained, "left stopped"}, {"stop-failed", false, ConfigRetained, "the agent did not come up and could not be stopped"}, {"restore-failed", false, ConfigRetained, "the new agent did not come up and the previous one could not be put back"}, {"running", false, ConfigUnconfirmed, "not known"}, {"recovering", false, ConfigUnconfirmed, "not known"}, {"bogus", false, ConfigUnconfirmed, "not known"}} {
		t.Run(tc.word, func(t *testing.T) {
			r := &stateResultRunner{word: tc.word}
			err := Activate(context.Background(), r, "x", Staged{Binary: true, Config: true}, time.Second)
			if tc.ok {
				if err != nil {
					t.Fatalf("expected success: %v", err)
				}
			} else {
				var ae *ActivateError
				if !errors.As(err, &ae) || ae.ConfigOutcome != tc.outcome || !strings.Contains(err.Error(), tc.text) {
					t.Fatalf("error=%v outcome expected=%s", err, tc.outcome)
				}
			}
			wantCalls := 2
			if tc.word == "restored" || tc.word == "restored-down" || tc.word == "stopped" || tc.word == "stop-failed" || tc.word == "restore-failed" {
				wantCalls = 3
			}
			if len(r.calls) != wantCalls {
				t.Fatalf("scripts after guard decision: %d", len(r.calls))
			}
		})
	}
}

type ownerResultRunner struct {
	word      string
	fail      bool
	emptyCode int
	previous  bool
	calls     []string
}

func (*ownerResultRunner) Target() string { return "fixture" }
func (r *ownerResultRunner) Run(_ context.Context, s string, _ io.Reader) (Result, error) {
	r.calls = append(r.calls, s)
	if strings.Contains(s, ".new ") {
		if r.previous {
			return Result{Stdout: []byte("prev_bin=yes\nprev_cfg=yes\n")}, nil
		}
		return Result{}, nil
	}
	if strings.Contains(s, "systemctl restart berthy") {
		return Result{Code: 1}, nil
	}
	if strings.HasPrefix(s, "journalctl") {
		return Result{Stdout: []byte("journal detail\n")}, nil
	}
	if strings.Contains(s, "/run/berthy-activation-guard.sh") && !strings.Contains(s, ".new ") {
		if r.fail {
			return Result{}, errors.New("lost owner")
		}
		if r.word == "" {
			return Result{Code: r.emptyCode}, nil
		}
		return Result{Stdout: []byte("guard=" + r.word + "\n")}, nil
	}
	return Result{}, nil
}
func TestActivateRunsGuardOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name, word     string
		previous, fail bool
		outcome        ConfigOutcome
		message        string
	}{{"restore", "restored", true, false, ConfigPrevious, "previous one was put back"}, {"stop", "stopped", false, false, ConfigRetained, "left stopped"}, {"lost owner", "", true, true, ConfigUnconfirmed, "server finishes"}} {
		t.Run(tc.name, func(t *testing.T) {
			r := &ownerResultRunner{word: tc.word, fail: tc.fail, previous: tc.previous}
			err := Activate(context.Background(), r, "x", Staged{Binary: true, Config: true}, time.Second)
			var ae *ActivateError
			if !errors.As(err, &ae) || ae.ConfigOutcome != tc.outcome || !strings.Contains(err.Error(), tc.message) || tc.fail && strings.Contains(err.Error(), "seconds after the swap") {
				t.Fatalf("error=%v expected=%s", err, tc.outcome)
			}
			wantScript := "/bin/sh " + guardPath + " no no now"
			if tc.previous {
				wantScript = "/bin/sh " + guardPath + " yes yes now"
			}
			if len(r.calls) != 4 || r.calls[3] != wantScript {
				t.Fatalf("expected synchronous guard: %q", r.calls)
			}
			if tc.previous && !strings.Contains(r.calls[3], "yes yes") {
				t.Fatalf("missing previous flags: %q", r.calls[3])
			}
		})
	}
}

func TestGuardResponseDistinguishesNoWord(t *testing.T) {
	for _, tc := range []struct {
		name, word string
		previous   bool
		code       int
		outcome    ConfigOutcome
		message    string
	}{
		{"no word with previous", "", true, 255, ConfigUnconfirmed, "setup may have started recovery; the server finishes it if it started, and the timer decides otherwise"},
		{"no word on first install", "", false, 1, ConfigRetained, "setup may have started recovery; the server finishes it if it started, and the timer decides otherwise"},
		{"empty successful response", "", true, 0, ConfigUnconfirmed, "setup may have started recovery; the server finishes it if it started, and the timer decides otherwise"},
		{"explicit running with previous", "running", true, 0, ConfigUnconfirmed, "the server guard took over and its result is not known yet"},
		{"explicit running on first install", "running", false, 0, ConfigRetained, "the server guard took over and its result is not known yet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &ownerResultRunner{word: tc.word, previous: tc.previous, emptyCode: tc.code}
			err := Activate(context.Background(), r, "x", Staged{Binary: true, Config: true}, time.Second)
			var ae *ActivateError
			if !errors.As(err, &ae) || ae.ConfigOutcome != tc.outcome || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error=%v; want %s (%s)", err, tc.message, tc.outcome)
			}
			if tc.word == "running" && strings.Contains(err.Error(), "timer decides otherwise") {
				t.Fatal("running was treated as no response")
			}
		})
	}
}
func TestGuardFailureReportsKnownResultAndJournal(t *testing.T) {
	for _, tc := range []struct {
		word     string
		previous bool
		outcome  ConfigOutcome
		message  string
	}{
		{"stop-failed", false, ConfigRetained, "the agent did not come up and could not be stopped"},
		{"restore-failed", true, ConfigRetained, "the new agent did not come up and the previous one could not be put back"},
		{"restore-failed", false, ConfigRetained, "the new agent did not come up and the previous one could not be put back"},
	} {
		t.Run(tc.word+fmt.Sprint(tc.previous), func(t *testing.T) {
			r := &ownerResultRunner{word: tc.word, previous: tc.previous}
			err := Activate(context.Background(), r, "x", Staged{Binary: true, Config: true}, time.Second)
			var ae *ActivateError
			if !errors.As(err, &ae) || ae.ConfigOutcome != tc.outcome || !strings.Contains(err.Error(), tc.message) || !strings.Contains(err.Error(), "journal detail") {
				t.Fatalf("error=%v; want %s (%s) with journal", err, tc.message, tc.outcome)
			}
		})
	}
}

func TestPriorGuardFailureIncludesJournal(t *testing.T) {
	for _, tc := range []struct{ word, message string }{
		{"stop-failed", "the agent did not come up and could not be stopped"},
		{"restore-failed", "the new agent did not come up and the previous one could not be put back"},
	} {
		t.Run(tc.word, func(t *testing.T) {
			r := &stateResultRunner{word: tc.word, journal: "last agent log\n"}
			err := Activate(context.Background(), r, "x", Staged{Binary: true, Config: true}, time.Second)
			if err == nil || !strings.Contains(err.Error(), tc.message) || !strings.Contains(err.Error(), "last agent log") {
				t.Fatalf("known guard failure lacks journal: %v", err)
			}
			if len(r.calls) != 3 || r.calls[2] != "journalctl -u berthy -n 20 --no-pager -o cat 2>&1" {
				t.Fatalf("journal call: %q", r.calls)
			}
		})
	}
}

func TestRestartLeavesRecoveringForGuard(t *testing.T) {
	code, out, state, calls := stateShell(t, restartScript("x", 1), "recovering", "no-actions")
	if code != 1 || state != "recovering" || strings.Contains(calls, "restart berthy") || strings.Contains(calls, "stop berthy") || strings.Contains(calls, "mv ") {
		t.Fatalf("code=%d out=%q state=%q calls=%q", code, out, state, calls)
	}
}
func TestGuardRejectsInvalidState(t *testing.T) {
	for _, tc := range []struct{ name, word, mode, want string }{
		{"missing", "", "no-actions", "guard=running"},
		{"empty", "", "empty-state", "guard=running"},
		{"unreadable", "pending", "unreadable-state", "guard=running"},
		{"unknown", "unexpected", "no-actions", "guard=unexpected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, _, calls := stateShell(t, guardScript("/nonexistent/bin", "/nonexistent/cfg", Staged{Binary: true}, "x", 1), tc.word, tc.mode, "yes", "no")
			if code == 0 || !strings.Contains(out, tc.want) || strings.Contains(calls, "restart berthy") || strings.Contains(calls, "stop berthy") || strings.Contains(calls, "mv ") {
				t.Fatalf("code=%d out=%q calls=%q", code, out, calls)
			}
		})
	}
}
