package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type guardRunner struct {
	calls    []string
	input    string
	stage    string
	restarts int
}

func (*guardRunner) Target() string { return "fixture" }
func (r *guardRunner) Run(_ context.Context, script string, stdin io.Reader) (Result, error) {
	r.calls = append(r.calls, script)
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		r.input = string(b)
	}
	switch {
	case strings.Contains(script, ".new "):
		return Result{Stdout: []byte("prev_bin=yes\nprev_cfg=yes\n")}, nil
	case strings.Contains(script, "systemctl restart berthy"):
		r.restarts++
		if r.stage == "lost" {
			return Result{}, errors.New("connection lost")
		}
		if r.stage == "up" {
			return Result{}, nil
		}
		return Result{Code: 1}, nil
	case strings.Contains(script, "activation-guard.timer") && r.stage == "up":
		return Result{}, errors.New("disarm failed")
	}
	return Result{}, nil
}
func TestSwapArmsTheGuardBeforeRenamingAnything(t *testing.T) {
	for _, staged := range []Staged{{Binary: true}, {Config: true}, {Binary: true, Config: true}} {
		r := &guardRunner{stage: "up"}
		_ = Activate(context.Background(), r, "127.0.0.1:7710", staged, time.Second)
		s := r.calls[0]
		arm := strings.Index(s, "systemd-run --quiet --unit=berthy-activation-guard --on-active=62s")
		if arm < 0 {
			t.Errorf("guard not armed: %s", s)
			continue
		}
		for _, path := range []string{binPath, configPath} {
			if strings.Contains(s, "mv -f "+path+".new") && !(strings.Index(s, "cp -p "+path) < arm && arm < strings.Index(s, "mv -f "+path+".new")) {
				t.Error("guard is not between copies and renames")
			}
			if strings.Contains(s, "mv -f "+path+".prev "+path) {
				t.Error("guard leaked into command")
			}
		}
		if strings.Contains(s, "systemctl restart berthy") || !strings.Contains(r.input, "systemctl restart berthy") {
			t.Error("guard was not sent on stdin")
		}
	}
}
func TestSwapRemovesAStalePrevWhenThereIsNoLiveFile(t *testing.T) {
	r := &guardRunner{stage: "up"}
	_ = Activate(context.Background(), r, "x", Staged{Binary: true, Config: true}, time.Second)
	for _, p := range []string{binPath, configPath} {
		if !strings.Contains(r.calls[0], "else rm -f "+p+".prev; fi") {
			t.Errorf("stale prev not removed for %s", p)
		}
	}
}
func TestActivateDisarmsTheGuardWhenTheNewAgentAnswers(t *testing.T) {
	r := &guardRunner{stage: "up"}
	if err := Activate(context.Background(), r, "x", Staged{Config: true}, time.Second); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 3 || !strings.Contains(r.calls[len(r.calls)-1], "systemctl stop berthy-activation-guard.timer") {
		t.Fatalf("no disarm: %q", r.calls)
	}
}
func TestActivateDisarmsTheGuardAfterPuttingThePreviousAgentBack(t *testing.T) {
	r := &guardRunner{}
	_ = Activate(context.Background(), r, "x", Staged{Config: true}, time.Second)
	if !strings.Contains(r.calls[len(r.calls)-1], "systemctl stop berthy-activation-guard.timer") {
		t.Fatalf("no disarm: %q", r.calls)
	}
}
func TestLostConnectionAfterTheSwapLeavesTheGuardArmed(t *testing.T) {
	r := &guardRunner{stage: "lost"}
	err := Activate(context.Background(), r, "x", Staged{Config: true}, time.Second)
	var ae *ActivateError
	if !errors.As(err, &ae) || ae.ConfigOutcome != ConfigUnconfirmed || !strings.Contains(err.Error(), "62 seconds") || len(r.calls) != 2 || !strings.Contains(fmt.Sprint(errors.Unwrap(err)), "connection lost") {
		t.Fatalf("lost connection: %v, calls %d", err, len(r.calls))
	}
}
func TestDryRunListsTheGuard(t *testing.T) {
	scripts := activateScripts(Staged{Config: true}, "x", time.Second)
	joined := strings.Join(scripts, "\n")
	if !strings.Contains(joined, "systemd-run") || !strings.Contains(joined, "systemctl stop berthy-activation-guard.timer") {
		t.Fatal("dry run omits guard")
	}
}

func TestActivateDisarmsTheGuardAfterStoppingAFirstInstall(t *testing.T) {
	r := &firstInstallGuardRunner{}
	_ = Activate(context.Background(), r, "x", Staged{Config: true}, time.Second)
	if len(r.calls) == 0 || !strings.Contains(r.calls[len(r.calls)-1], "systemctl stop berthy-activation-guard.timer") {
		t.Fatalf("no disarm: %q", r.calls)
	}
}

type firstInstallGuardRunner struct{ calls []string }

func (*firstInstallGuardRunner) Target() string { return "fixture" }
func (r *firstInstallGuardRunner) Run(_ context.Context, s string, _ io.Reader) (Result, error) {
	r.calls = append(r.calls, s)
	if strings.Contains(s, "systemctl restart berthy") {
		return Result{Code: 1}, nil
	}
	return Result{}, nil
}

func TestTheGuardPutsThePreviousAgentBackWhenTheNewOneDoesNotAnswer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires sh")
	}
	for _, tc := range []struct {
		name, prev      string
		answer, missing bool
	}{{"answers", "yes", true, false}, {"rollback", "yes", false, false}, {"first install", "no", false, false}, {"prev gone", "yes", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			cfg := filepath.Join(dir, "cfg")
			tools := filepath.Join(dir, "tools")
			os.Mkdir(tools, 0700)
			for _, p := range []string{bin, cfg} {
				os.WriteFile(p, []byte("new"), 0600)
				if !tc.missing {
					os.WriteFile(p+".prev", []byte("old"), 0600)
				}
			}
			os.WriteFile(filepath.Join(tools, "systemctl"), []byte("#!/bin/sh\necho \"$*\" >> \"$CALLS\"\ncase \"$1\" in is-failed) exit 0;; esac\nexit 0\n"), 0700)
			curl := "#!/bin/sh\nif [ -f \"$CALLS.probed\" ]; then exit 0; fi\ntouch \"$CALLS.probed\"\nexit 1\n"
			if tc.answer {
				curl = "#!/bin/sh\nexit 0\n"
			}
			os.WriteFile(filepath.Join(tools, "curl"), []byte(curl), 0700)
			r := &guardRunner{stage: "up"}
			_ = Activate(context.Background(), r, "x", Staged{Binary: true, Config: true}, time.Second)
			script := strings.ReplaceAll(strings.ReplaceAll(r.input, binPath, bin), configPath, cfg)
			if script == "" {
				t.Fatal("guard script not sent")
			}
			cmd := exec.Command("sh", "-c", script, "guard", tc.prev, tc.prev)
			cmd.Env = append(os.Environ(), "PATH="+tools+":"+os.Getenv("PATH"), "CALLS="+filepath.Join(dir, "calls"))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("guard failed: %v %s", err, out)
			}
			want := "new"
			if tc.name == "rollback" {
				want = "old"
			}
			for _, path := range []string{bin, cfg} {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != want {
					t.Fatalf("%s = %s: %v", path, got, err)
				}
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
			if tc.name == "first install" && !strings.Contains(string(calls), "stop berthy") {
				t.Fatal("service not stopped")
			}
			if (tc.name == "rollback" || tc.name == "prev gone") && strings.Count(string(calls), "restart berthy") != 2 {
				t.Fatal("second restart missing")
			}
		})
	}
}

func TestGuardMessageOnUnresolvedActivation(t *testing.T) {
	for _, stage := range []string{"swap 255", "swap cancelled", "restart error", "journal error", "stop 255", "rollback 255", "rollback error", "after rollback"} {
		t.Run(stage, func(t *testing.T) {
			r := &outcomeRunner{stage: stage}
			err := Activate(context.Background(), r, "x", Staged{Config: true}, time.Second)
			if err == nil || !strings.Contains(err.Error(), "62 seconds") {
				t.Fatalf("guard message missing: %v", err)
			}
			if strings.Contains(err.Error(), "may have armed") != strings.HasPrefix(stage, "swap") {
				t.Fatalf("wrong certainty: %v", err)
			}
		})
	}
	for _, stage := range []string{""} {
		r := &outcomeRunner{stage: stage}
		err := Activate(context.Background(), r, "x", Staged{Config: true}, time.Second)
		if err == nil || strings.Contains(err.Error(), "62 seconds") {
			t.Fatalf("completed rollback: %v", err)
		}
	}
}
