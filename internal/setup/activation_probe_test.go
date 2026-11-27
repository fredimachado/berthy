package setup

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/agent"
)

type probePreparationRunner struct {
	calls []string
	run   func(string) (Result, error)
}

func (r *probePreparationRunner) Target() string { return "fixture" }

func (r *probePreparationRunner) Run(_ context.Context, script string, _ io.Reader) (Result, error) {
	r.calls = append(r.calls, script)
	if strings.Contains(script, "agent setup-probe version") {
		return r.run(script)
	}
	return Result{}, errors.New("unexpected server call")
}

func probePreparationSession(r Runner) *Session {
	return &Session{
		R: r, F: &Facts{},
		Opts:   Options{ListenIP: net.ParseIP(listenIP), ListenPort: 7710},
		staged: Staged{Binary: true, Config: true}, Token: "new-test-token", newToken: &agent.Token{},
	}
}

func TestSwapRequiresUsableProbeBeforeRestart(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		res           Result
		err           error
	}{
		{"no capable binary", "probe-capable Berthy build", Result{Code: 1, Stderr: []byte("a probe-capable Berthy build is needed")}, nil},
		{"publication failure", "cannot publish the setup probe", Result{Code: 1, Stderr: []byte("cannot publish the setup probe")}, nil},
		{"runner error", "SSH disconnected", Result{}, errors.New("SSH disconnected")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &probePreparationRunner{run: func(string) (Result, error) { return tc.res, tc.err }}
			s := probePreparationSession(r)
			_, _, err := serviceStep(t.Context(), s)
			var ae *ActivateError
			if !errors.As(err, &ae) || ae.ConfigOutcome != ConfigPrevious || !strings.Contains(err.Error(), "preparing the setup probe") ||
				!strings.Contains(err.Error(), tc.message) || strings.Contains(err.Error(), s.Token) || strings.Contains(err.Error(), "guard") ||
				s.TokenLive || s.TokenUncertain || len(r.calls) != 1 || !strings.Contains(r.calls[0], "agent setup-probe version") {
				t.Fatalf("preparation failure should precede unit installation: error=%v, calls=%q, live=%t, uncertain=%t", err, r.calls, s.TokenLive, s.TokenUncertain)
			}
		})
	}
	t.Run("failed publication preserves retained probe", func(t *testing.T) {
		dir, tools := shellFixture(t)
		shellTool(t, tools, "berthy.new", `printf 'berthy-setup-probe/1\n'`)
		const retained = "printf 'berthy-setup-probe/1\\n' # retained"
		shellTool(t, tools, "berthy.setup-probe", retained)
		shellTool(t, tools, "timeout", `shift 2; exec "$@"`)
		shellTool(t, tools, "mv", `echo "$*" >> "$CALLS"; exit 1`)
		shellTool(t, tools, "install", `shift 7; exec /bin/cp "$@"`)
		r := &probePreparationRunner{run: func(script string) (Result, error) {
			script = strings.ReplaceAll(script, binPath, filepath.Join(tools, "berthy"))
			if runShell(t, script, "", tools, dir) == nil {
				t.Fatal("publication should fail")
			}
			return Result{Code: 1, Stderr: []byte("cannot publish the setup probe")}, nil
		}}
		_, _, err := serviceStep(t.Context(), probePreparationSession(r))
		if err == nil || !strings.Contains(err.Error(), "cannot publish the setup probe") {
			t.Fatalf("publication error: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(tools, "berthy.setup-probe"))
		if err != nil || string(got) != "#!/bin/sh\n"+retained+"\n" {
			t.Fatalf("retained probe changed: %q, %v", got, err)
		}
		log, _ := os.ReadFile(filepath.Join(dir, "calls"))
		if !strings.Contains(string(log), "berthy.setup-probe") {
			t.Fatalf("publication never attempted a rename: %q", log)
		}
	})
}

func TestStalledStagedProbeUsesInstalledBinary(t *testing.T) {
	dir, tools := shellFixture(t)
	shellTool(t, tools, "berthy.new", `
[ "${SUPERVISED:-}" = yes ] || { echo unsupervised >> "$CALLS"; exit 1; }
if read -r unexpected; then observation=has-input; else observation=eof; fi
printf '%s\n' "$observation" > "$CALLS.ready"
trap '' TERM
read -r release < "$CALLS.block"`)
	shellTool(t, tools, "berthy", `printf 'berthy-setup-probe/1\n'`)
	shellTool(t, tools, "install", `shift 7; exec /bin/cp "$@"`)
	shellTool(t, tools, "mv", `[ "$1" = -fT ] && [ "$2" = -- ] || exit 2; shift 2; exec /bin/mv -f "$@"`)
	shellTool(t, tools, "timeout", `
exec 3<&0
echo "timeout $*" >> "$CALLS"
[ "$1" = --kill-after=1s ] && [ "$2" = 3s ] || exit 2
shift 2
case "$*" in
  *'berthy.new agent setup-probe version')
    mkfifo "$CALLS.ready" "$CALLS.block" || exit 2
    SUPERVISED=yes "$@" <&3 3<&- &
    child=$!
    trap 'kill -KILL "$child" 2>/dev/null; wait "$child" 2>/dev/null' EXIT
    IFS= read -r observation < "$CALLS.ready"
    echo "staged stdin: $observation" >> "$CALLS"
    exit 124 ;;
  *) exec "$@" ;;
esac`)
	prepared := false
	r := &probePreparationRunner{run: func(script string) (Result, error) {
		prepared = true
		script = strings.ReplaceAll(script, binPath, filepath.Join(tools, "berthy"))
		return Result{}, runShell(t, script, "do not read me\n", tools, dir)
	}}
	_, _, err := serviceStep(t.Context(), probePreparationSession(r))
	if !prepared || err == nil || !strings.Contains(err.Error(), "installing berthy.service") {
		t.Fatalf("preparation did not complete before unit installation: %v", err)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "calls"))
	want := "timeout --kill-after=1s 3s setpriv --reuid=berthy --regid=berthy --clear-groups "
	if !strings.Contains(string(log), want+filepath.Join(tools, "berthy.new")+" agent setup-probe version\nstaged stdin: eof\n"+
		want+filepath.Join(tools, "berthy")+" agent setup-probe version\n") {
		t.Fatalf("candidate selection calls: %q", log)
	}
	got, err := os.ReadFile(filepath.Join(tools, "berthy.setup-probe"))
	if err != nil || string(got) != "#!/bin/sh\nprintf 'berthy-setup-probe/1\\n'\n" {
		t.Fatalf("installed candidate not retained: %q, %v", got, err)
	}
}

func closedProbeTools(t *testing.T, tools string) {
	t.Helper()
	for _, name := range []string{"cat", "grep", "mktemp", "mv"} {
		if _, err := os.Stat(filepath.Join(tools, name)); err == nil {
			continue
		}
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		shellTool(t, tools, name, "exec "+shellQuote(path)+` "$@"`)
	}
}

func TestSetupWaitWithoutFetchTools(t *testing.T) {
	dir, tools := shellFixture(t)
	closedProbeTools(t, tools)
	shellClock(t, tools)
	shellTool(t, tools, "systemctl", `case "$1" in show) echo active ;; esac`)
	shellTool(t, tools, "berthy.setup-probe", `
echo "$*" >> "$CALLS"
[ "$*" = 'agent setup-probe health --listen 127.0.0.1:7710 --timeout 3s' ]`)
	state := filepath.Join(dir, "state")
	if err := os.WriteFile(state, []byte("pending\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script := strings.NewReplacer(guardLock, filepath.Join(dir, "lock"), guardState, state,
		guardProbePath, filepath.Join(tools, "berthy.setup-probe")).Replace(restartScript("127.0.0.1:7710", 3))
	err := runShell(t, "PATH="+shellQuote(tools)+"\n"+script, "", tools, dir)
	got, _ := os.ReadFile(state)
	log, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil || string(got) != "confirmed\n" || !strings.Contains(string(log), "agent setup-probe health") {
		t.Fatalf("restart with neither fetch tool: error=%v, guard=%q, probe calls=%q", err, got, log)
	}
}

func TestSetupWaitStopsOnUnitState(t *testing.T) {
	const listen = "127.0.0.1:7710"
	for _, state := range []string{"failed", "inactive", "deactivating", "unreadable", "activating"} {
		t.Run(state, func(t *testing.T) {
			dir, tools := shellFixture(t)
			shellTool(t, tools, "systemctl", `case "$1" in
  show)
    echo show >> "$CALLS"
    if [ "`+state+`" = unreadable ]; then exit 1; fi
    if [ -f "$CLOCK" ]; then echo active; else echo `+state+`; fi ;;
esac`)
			shellTool(t, tools, "date", `if [ -f "$CLOCK" ]; then cat "$CLOCK"; else echo 0; fi`)
			shellTool(t, tools, "sleep", `echo sleep >> "$CALLS"; n=$(date +%s); echo $((n+1)) > "$CLOCK"`)
			shellTool(t, tools, "curl", `echo legacy-http >> "$CALLS"; exit 0`)
			shellTool(t, tools, "berthy.setup-probe", `echo "$*" >> "$CALLS"; [ -f "$CLOCK" ]`)
			script := strings.ReplaceAll(restartScriptWithoutGuard(listen, 3), guardProbePath, filepath.Join(tools, "berthy.setup-probe"))
			err := runShell(t, script, "", tools, dir)
			log, _ := os.ReadFile(filepath.Join(dir, "calls"))
			if state != "activating" {
				if err == nil || string(log) != "show\n" {
					t.Fatalf("%s unit: error=%v, calls=%q, want a state check only", state, err, log)
				}
				return
			}
			probe := "agent setup-probe health --listen " + listen + " --timeout "
			if want := "show\n" + probe + "3s\nsleep\nshow\n" + probe + "2s\n"; err != nil || string(log) != want {
				t.Fatalf("activating unit: error=%v, calls=%q, want %q", err, log, want)
			}
		})
	}
}

func TestGuardProbesRestoredLegacyAgent(t *testing.T) {
	for _, word := range []string{"pending", "recovering"} {
		t.Run(word, func(t *testing.T) {
			f := newResumeFixture(t, word)
			closedProbeTools(t, f.tools)
			shellTool(t, f.tools, "date", `if [ -f "$CALLS.clock" ]; then cat "$CALLS.clock"; else echo 0; fi`)
			shellTool(t, f.tools, "sleep", `n=$(date +%s); echo $((n+1)) > "$CALLS.clock"`)
			legacy := "#!/bin/sh\nexit 2 # legacy agent\n"
			if err := os.WriteFile(f.bin+".prev", []byte(legacy), 0700); err != nil {
				t.Fatal(err)
			}
			if word == "recovering" {
				if err := os.Rename(f.bin+".prev", f.bin); err != nil {
					t.Fatal(err)
				}
			}
			probe := `[ "$*" = 'agent setup-probe health --listen 127.0.0.1:7710 --timeout 1s' ] || exit 2
if grep -q 'legacy agent' "$BIN" && [ "$(cat ` + shellQuote(f.cfg) + `)" = old-cfg ]; then
  echo 'retained probe answered after restore' >> "$CALLS"
  exit 0
fi
echo 'retained probe saw unhealthy new agent' >> "$CALLS"
echo 1 > "$CALLS.clock"
exit 1`
			shellTool(t, f.tools, "berthy.setup-probe", probe)
			if word == "pending" {
				shellTool(t, f.dir, filepath.Base(f.bin), probe)
				if err := os.Chmod(f.bin, 0700); err != nil {
					t.Fatal(err)
				}
			}
			script := "PATH=" + shellQuote(f.tools) + "\n" + guardScript(f.bin, f.cfg, Staged{Binary: true, Config: true}, "127.0.0.1:7710", 1)
			code, output, state, log := f.run(t, script, "yes", "yes")
			restarts := 1
			if word == "pending" {
				restarts++
			}
			bin, _ := os.ReadFile(f.bin)
			cfg, _ := os.ReadFile(f.cfg)
			if code != 0 || state != "restored" || string(bin) != legacy || string(cfg) != "old-cfg" ||
				strings.Count(log, "restart berthy") != restarts || !strings.Contains(log, "retained probe answered after restore") ||
				(word == "pending") != strings.Contains(log, "retained probe saw unhealthy new agent") {
				t.Fatalf("legacy restore: code=%d, output=%q, state=%q, binary=%q, config=%q, calls=%q", code, output, state, bin, cfg, log)
			}
		})
	}
}
