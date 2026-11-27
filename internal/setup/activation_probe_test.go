package setup

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/agent"
)

const testProbePath = binPath + ".setup-probe"

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
