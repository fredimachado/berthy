package setup

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"berthy/internal/scope"
)

const (
	testStartupPath = "/run/berthy-startup/result"
	oldInvocation   = "11111111111111111111111111111111"
	newInvocation   = "22222222222222222222222222222222"
	testTarget      = "shop/lab"
	testReasonPath  = "/run/berthy-activation-guard.reason"
)

func stagedAddition() Staged {
	return Staged{Config: true, Added: &scope.ID{Project: "shop", Environment: "lab"}}
}

func startupRecord(invocation, rows, count string) string {
	return "berthy-startup-v1\t" + invocation + "\t" + count + "\n" + rows + "complete\n"
}

type startupFixture struct {
	resumeFixture
	result string
}

func newStartupFixture(t *testing.T, record, mode, endpoint string) startupFixture {
	t.Helper()
	f := newResumeFixture(t, "pending")
	result := filepath.Join(f.dir, "startup-result")
	if record != "" {
		if err := os.WriteFile(result, []byte(record), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(f.calls+".mode", []byte(mode), 0600); err != nil {
		t.Fatal(err)
	}
	shellTool(t, f.tools, "systemctl", `
echo "$*" >> "$CALLS"
mode=$(cat "$CALLS.mode")
case "$1" in
  restart)
    touch "$CALLS.restarted"
    if [ -e "$CALLS.restore-result" ] && [ "$(cat "${STATE%/*}/cfg")" = old-cfg ]; then
      cp "$CALLS.restore-result" "${STATE%/*}/startup-result"
    fi
    exit 0 ;;
  show)
    inv=`+oldInvocation+`
    if [ -e "$CALLS.restarted" ] && [ "$mode" != same-invocation ]; then inv=`+newInvocation+`; fi
    if [ -e "$CALLS.changed" ]; then inv=33333333333333333333333333333333; fi
    case " $* " in
      *' --property=ActiveState '*) echo active ;;
      *' --value '*) printf '%s\n' "$inv" ;;
      *) printf 'MainPID=123\nInvocationID=%s\n' "$inv" ;;
    esac ;;
esac
`)
	shellTool(t, f.tools, "berthy.setup-probe", `
if [ "$(cat "$CALLS.mode")" = invocation-changes ]; then touch "$CALLS.changed"; fi
exit 0
`)
	shellTool(t, f.tools, "id", "echo 4242")
	shellTool(t, f.tools, "stat", `case "$2" in '%u:%a:%h') echo 0:600:1;; *) echo 0:4242:620:1;; esac`)
	shellTool(t, f.tools, "ss", `
case "$(cat "$CALLS.mode")" in
  no-ss) exit 127 ;;
  spoofed-owner)
    printf '%s\n' 'LISTEN 0 128 127.0.0.1:7710 0.0.0.0:* users:(("fake",pid=123,fd=7",pid=1234,fd=9))'
    exit 0 ;;
esac
pid=123
if [ "$(cat "$CALLS.mode")" = wrong-listener ]; then pid=1234; fi
printf 'LISTEN 0 128 127.0.0.1:7710 0.0.0.0:* users:(("agent",pid=%s,fd=7))\n' "$pid"
`)
	proc := filepath.Join(f.dir, "proc")
	for _, dir := range []string{"net", "123/fd", "1234/fd"} {
		if err := os.MkdirAll(filepath.Join(proc, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	tcp := "  sl  local_address rem_address st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode\n" +
		"  0: " + endpoint + " 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 4578\n"
	if err := os.WriteFile(filepath.Join(proc, "net", "tcp"), []byte(tcp), 0600); err != nil {
		t.Fatal(err)
	}
	pid := "123"
	if mode == "wrong-listener" || mode == "spoofed-owner" {
		pid = "1234"
	}
	if err := os.Symlink("socket:[4578]", filepath.Join(proc, pid, "fd", "7")); err != nil {
		t.Fatal(err)
	}
	return startupFixture{resumeFixture: f, result: result}
}

func (f startupFixture) run(t *testing.T, script string, args ...string) (int, string, string, string) {
	t.Helper()
	script = strings.NewReplacer(testStartupPath, f.result, testReasonPath, filepath.Join(f.dir, "reason"),
		"proc_root=/proc", "proc_root="+shellQuote(filepath.Join(f.dir, "proc"))).Replace(script)
	return f.resumeFixture.run(t, script, args...)
}

func TestActivateRequiresCurrentInvocationAndStartedTarget(t *testing.T) {
	started := "other/lab\tfailed\tcannot open state\nshop/lab\tstarted\t\n"
	failed := "other/lab\tstarted\t\nshop/lab\tfailed\tcannot open state\n"
	valid := startupRecord(newInvocation, started, "2")
	for _, tc := range []struct {
		name, record, mode string
		want               bool
	}{
		{name: "started", record: valid, want: true},
		{name: "failed", record: startupRecord(newInvocation, failed, "2")},
		{name: "absent"},
		{name: "truncated", record: strings.TrimSuffix(valid, "complete\n")},
		{name: "unterminated", record: strings.TrimSuffix(valid, "\n")},
		{name: "count mismatch", record: startupRecord(newInvocation, started, "1")},
		{name: "duplicate target", record: startupRecord(newInvocation, started+"shop/lab\tstarted\t\n", "3")},
		{name: "target missing", record: startupRecord(newInvocation, "shop/labs\tstarted\t\n", "1")},
		{name: "invalid status", record: startupRecord(newInvocation, "shop/lab\thealthy\t\n", "1")},
		{name: "unexpected reason", record: startupRecord(newInvocation, "shop/lab\tstarted\tcannot open state\n", "1")},
		{name: "missing reason", record: startupRecord(newInvocation, "shop/lab\tfailed\t\n", "1")},
		{name: "stale invocation", record: startupRecord(oldInvocation, started, "2")},
		{name: "same invocation", record: startupRecord(oldInvocation, started, "2"), mode: "same-invocation"},
		{name: "wrong listener", record: valid, mode: "wrong-listener"},
		{name: "PID text in process name", record: valid, mode: "spoofed-owner"},
		{name: "ss unavailable", record: valid, mode: "no-ss", want: true},
		{name: "invocation changes", record: valid, mode: "invocation-changes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStartupFixture(t, tc.record, tc.mode, "0100007F:1E1E")
			scripts := activateScripts(stagedAddition(), "127.0.0.1:7710", time.Second)
			code, out, state, calls := f.run(t, scripts[1])
			if confirmed := code == 0 && state == "confirmed"; confirmed != tc.want {
				t.Errorf("confirmed=%v want %v, code=%d out=%q state=%q calls=%q", confirmed, tc.want, code, out, state, calls)
			}
		})
	}
}

func TestGuardChecksAddedEnvironmentBeforeKeepingCandidate(t *testing.T) {
	for _, tc := range []struct{ name, rows, previous, want string }{
		{"keep started target", "other/lab\tfailed\tcannot open state\nshop/lab\tstarted\t\n", "yes", "kept"},
		{"restore failed addition", "other/lab\tstarted\t\nshop/lab\tfailed\tcannot open state\n", "yes", "restored"},
		{"stop failed first install", "other/lab\tstarted\t\nshop/lab\tfailed\tcannot open state\n", "no", "stopped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStartupFixture(t, startupRecord(newInvocation, tc.rows, "2"), "", "0100007F:1E1E")
			script := guardScript(f.bin, f.cfg, stagedAddition(), "127.0.0.1:7710", 1)
			code, out, state, calls := f.run(t, script, "no", tc.previous)
			if code != 0 || state != tc.want {
				t.Errorf("state=%q, want %q, code=%d out=%q calls=%q", state, tc.want, code, out, calls)
			}
			if tc.want == "restored" {
				data, err := os.ReadFile(f.cfg)
				if err != nil || string(data) != "old-cfg" || strings.Count(calls, "restart berthy") != 2 {
					t.Errorf("restored config=%q, %v, calls=%q", data, err, calls)
				}
			}
		})
	}
}

type startupActivation struct {
	startupFixture
	t                  *testing.T
	timerFirst         bool
	journalUnavailable bool
}

func (*startupActivation) Target() string { return "fixture" }

func (r *startupActivation) shell(script string, input io.Reader, args ...string) (Result, error) {
	script = strings.NewReplacer(testStartupPath, r.result, testReasonPath, filepath.Join(r.dir, "reason"),
		"proc_root=/proc", "proc_root="+shellQuote(filepath.Join(r.dir, "proc")),
		guardProbePath, filepath.Join(r.tools, "berthy.setup-probe"),
		guardPath, filepath.Join(r.dir, "guard"), binPath, r.bin, configPath, r.cfg).Replace(script)
	code, out, _, _ := r.runInput(r.t, script, input, args...)
	return Result{Code: code, Stdout: []byte(out)}, nil
}

func TestTimerRollsBackWhenInterruptedAfterRecordingReason(t *testing.T) {
	f := newStartupFixture(t, startupRecord(newInvocation, "shop/lab\tstarted\t\n", "1"), "", "0100007F:1E1E")
	// Simulate the foreground exiting after the reason rename and before changing "pending".
	if err := os.WriteFile(filepath.Join(f.dir, "reason"), []byte(newInvocation+"\t"+testTarget+"\tcannot open state\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script := guardScript(f.bin, f.cfg, stagedAddition(), "127.0.0.1:7710", 1)
	code, out, word, calls := f.run(t, script, "no", "yes")
	cfg, err := os.ReadFile(f.cfg)
	if code != 0 || word != "restored" || string(cfg) != "old-cfg" || err != nil ||
		strings.Count(calls, "restart berthy") != 1 || !strings.Contains(out, "cannot open state") {
		t.Fatalf("interrupted rejection: code=%d state=%q config=%q output=%q calls=%q", code, word, cfg, out, calls)
	}
}

func (r *startupActivation) guard(args ...string) (Result, error) {
	raw, err := os.ReadFile(filepath.Join(r.dir, "guard"))
	if err != nil {
		return Result{}, err
	}
	return r.shell(string(raw), nil, args...)
}

func (r *startupActivation) Run(_ context.Context, script string, input io.Reader) (Result, error) {
	switch {
	case strings.HasPrefix(script, "journalctl "):
		if r.journalUnavailable {
			return Result{}, errors.New("journal unavailable")
		}
		return Result{Stdout: []byte("cannot open state\n" + strings.Repeat("later journal line\n", 25))}, nil
	case strings.HasPrefix(script, "/bin/sh "+guardPath):
		return r.guard("no", "yes", "now")
	case strings.Contains(script, "systemctl restart berthy") && r.timerFirst:
		r.timerFirst = false
		result, err := r.guard("no", "yes")
		if err != nil || result.Code != 0 {
			r.t.Fatalf("timer guard: %+v, %v", result, err)
		}
	}
	return r.shell(script, input)
}

func newStartupActivation(t *testing.T) *startupActivation {
	t.Helper()
	candidate := startupRecord(newInvocation, "shop/lab\tfailed\tcannot open state\n", "1")
	f := newStartupFixture(t, candidate, "", "0100007F:1E1E")
	r := &startupActivation{startupFixture: f, t: t}
	shellTool(t, f.tools, "systemd-run", "exit 0")
	for path, body := range map[string]string{
		f.cfg: "old-cfg", f.cfg + ".new": "new-cfg",
		f.calls + ".restore-result": startupRecord(oldInvocation, "other/lab\tstarted\t\n", "1"),
	} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestActivationReportsTargetReasonAfterJournalRollsPastIt(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		timer, journalUnavailable bool
	}{{"foreground", false, false}, {"timer", true, false}, {"foreground without journal", false, true}, {"timer without journal", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newStartupActivation(t)
			r.timerFirst, r.journalUnavailable = tc.timer, tc.journalUnavailable
			err := Activate(context.Background(), r, "127.0.0.1:7710", stagedAddition(), time.Second)
			var activation *ActivateError
			if !errors.As(err, &activation) || activation.ConfigOutcome != ConfigPrevious ||
				!strings.Contains(activation.Message, "shop/lab: cannot open state") {
				t.Errorf("activation = %+v, want previous config and exact target reason", err)
			}
			data, cfgErr := os.ReadFile(r.cfg)
			raw, resultErr := os.ReadFile(r.result)
			if cfgErr != nil || string(data) != "old-cfg" || resultErr != nil || !strings.Contains(string(raw), oldInvocation) {
				t.Errorf("restored config=%q (%v), restored result=%q (%v)", data, cfgErr, raw, resultErr)
			}
		})
	}
}

func TestNewActivationCannotReusePriorFailureReason(t *testing.T) {
	r := newStartupActivation(t)
	ctx := context.Background()
	err := Activate(ctx, r, "127.0.0.1:7710", stagedAddition(), time.Second)
	if err == nil || !strings.Contains(err.Error(), "shop/lab: cannot open state") {
		t.Errorf("first activation did not retain its reason: %v", err)
	}
	reason := filepath.Join(r.dir, "reason")
	if _, err := os.Stat(reason); err != nil {
		t.Errorf("first swap did not preserve its companion file: %v", err)
	}
	if err := os.WriteFile(r.cfg+".new", []byte("new-cfg"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(r.calls + ".restarted"); err != nil {
		t.Fatal(err)
	}
	// The result still names the restored invocation, even after the new candidate restarts.
	err = Activate(ctx, r, "127.0.0.1:7710", stagedAddition(), time.Second)
	var activation *ActivateError
	if !errors.As(err, &activation) || activation.ConfigOutcome != ConfigPrevious ||
		strings.Contains(activation.Message, "shop/lab: cannot open state") {
		t.Errorf("second activation = %v, want previous config without the old target reason", err)
	}
	if _, err := os.Stat(reason); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale reason survived the second swap: %v", err)
	}
}
