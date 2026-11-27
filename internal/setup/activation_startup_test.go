package setup

import (
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

func newStartupFixture(t *testing.T, record, mode string) startupFixture {
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
  is-failed) exit 0 ;;
  restart) touch "$CALLS.restarted"; exit 0 ;;
  show)
    inv=`+oldInvocation+`
    if [ -e "$CALLS.restarted" ] && [ "$mode" != same-invocation ]; then inv=`+newInvocation+`; fi
    if [ -e "$CALLS.changed" ]; then inv=33333333333333333333333333333333; fi
    case " $* " in
      *' --value '*) printf '%s\n' "$inv" ;;
      *) printf 'MainPID=123\nInvocationID=%s\n' "$inv" ;;
    esac ;;
esac
`)
	shellTool(t, f.tools, "curl", `
if [ "$(cat "$CALLS.mode")" = invocation-changes ]; then touch "$CALLS.changed"; fi
exit 0
`)
	shellTool(t, f.tools, "id", "echo 4242")
	shellTool(t, f.tools, "stat", "echo 0:4242:620:1")
	shellTool(t, f.tools, "ss", `
pid=123
if [ "$(cat "$CALLS.mode")" = wrong-listener ]; then pid=1234; fi
printf 'LISTEN 0 128 127.0.0.1:7710 0.0.0.0:* users:(("agent",pid=%s,fd=7))\n' "$pid"
`)
	return startupFixture{resumeFixture: f, result: result}
}

func (f startupFixture) run(t *testing.T, script string, args ...string) (int, string, string, string) {
	t.Helper()
	script = strings.ReplaceAll(script, testStartupPath, f.result)
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
		{name: "invocation changes", record: valid, mode: "invocation-changes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStartupFixture(t, tc.record, tc.mode)
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
			f := newStartupFixture(t, startupRecord(newInvocation, tc.rows, "2"), "")
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
