package setup

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"

	"berthy/internal/keywrap"
)

// funcRunner is a Runner whose script runs are answered by run.
type funcRunner struct {
	run func(script string, stdin io.Reader) Result
}

func (f funcRunner) Target() string { return "ops@s1" }

func (f funcRunner) Run(_ context.Context, script string, stdin io.Reader) (Result, error) {
	return f.run(script, stdin), nil
}

func TestRecordingPrintsEachScriptBeforeItRunsAndItsStatusAfter(t *testing.T) {
	var out bytes.Buffer
	log := NewCommandLog(&out)
	log.begin("docker")
	r := Recording(funcRunner{run: func(string, io.Reader) Result {
		out.WriteString("RAN\n")
		return Result{Code: 3}
	}}, log)
	if _, err := r.Run(context.Background(), "set -e\necho hi", nil); err != nil {
		t.Fatal(err)
	}
	want := "# docker\n$ set -e\n$ echo hi\nRAN\n→ exit 3\n"
	if out.String() != want {
		t.Fatalf("log =\n%q\nwant\n%q", out.String(), want)
	}
}

func TestRecordingNeverShowsStdinOrOutput(t *testing.T) {
	var out bytes.Buffer
	log := NewCommandLog(&out)
	log.begin("key")
	r := Recording(funcRunner{run: func(string, io.Reader) Result {
		return Result{Stdout: []byte("STDOUT-SECRET"), Stderr: []byte("STDERR-SECRET")}
	}}, log)
	if _, err := r.Run(context.Background(), "cat > /x", strings.NewReader("STDIN-SECRET")); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"STDIN-SECRET", "STDOUT-SECRET", "STDERR-SECRET"} {
		if strings.Contains(out.String(), secret) {
			t.Errorf("the log shows %s:\n%s", secret, out.String())
		}
	}
	if !strings.Contains(out.String(), "$ cat > /x\n→ exit 0\n") {
		t.Errorf("log = %q", out.String())
	}
}

func TestRecordingPassesSudoAndTargetThrough(t *testing.T) {
	f := &fakeServer{uid: "1000", user: "ops", sudoOK: true}
	r := Recording(f, NewCommandLog(io.Discard))
	login, err := Connect(context.Background(), r)
	if err != nil || !login.Sudo || !f.sudoSet {
		t.Fatalf("login = %+v, err = %v, sudo set = %v", login, err, f.sudoSet)
	}
	if r.Target() != "ops@s1" {
		t.Fatalf("target = %q", r.Target())
	}
}

func TestWouldListsACommandWithoutSendingIt(t *testing.T) {
	var out bytes.Buffer
	log := NewCommandLog(&out)
	log.begin("docker")
	log.Would("systemctl start docker")
	if want := "# docker\n$ systemctl start docker\n→ not run: dry run\n"; out.String() != want {
		t.Fatalf("log = %q, want %q", out.String(), want)
	}
	if got := log.end(); got == nil || !reflect.DeepEqual(*got, []string{"systemctl start docker"}) {
		t.Fatalf("commands = %v", got)
	}
}

func TestRunReportsTheCommandsOfEachStep(t *testing.T) {
	var out bytes.Buffer
	log := NewCommandLog(&out)
	f := &fakeServer{}
	steps := []Step{
		{"sends", func(ctx context.Context, s *Session) (string, string, error) {
			_, _ = s.R.Run(ctx, "echo one", nil)
			_, _ = s.R.Run(ctx, "echo two", nil)
			return StatusOK, "", nil
		}},
		{"quiet", func(context.Context, *Session) (string, string, error) { return StatusOK, "", nil }},
	}

	var got []StepResult
	Run(context.Background(), &Session{R: Recording(f, log), Log: log}, steps, func(r StepResult) { got = append(got, r) })
	if got[0].Commands == nil || !reflect.DeepEqual(*got[0].Commands, []string{"echo one", "echo two"}) {
		t.Fatalf("sends reported %v", got[0].Commands)
	}
	if got[1].Commands == nil || len(*got[1].Commands) != 0 {
		t.Fatalf("quiet reported %v, want an empty list", got[1].Commands)
	}

	// Without a log a run reports no commands at all.
	var plain []StepResult
	Run(context.Background(), &Session{R: f}, steps, func(r StepResult) { plain = append(plain, r) })
	for _, r := range plain {
		if r.Commands != nil {
			t.Fatalf("%s reported commands without a log: %v", r.Step, *r.Commands)
		}
	}
}

func TestNilCommandLogRecordsNothing(t *testing.T) {
	var log *CommandLog
	log.begin("docker")
	log.Would("systemctl start docker")
	if log.end() != nil {
		t.Fatal("a nil log reported commands")
	}
	if _, err := Recording(&fakeServer{}, nil).Run(context.Background(), "id", nil); err != nil {
		t.Fatal(err)
	}
}

// The JSON report has a commands key on every step of a verbose run, an empty list when the step sent nothing, and no key at all
// when the run is not verbose.
func TestStepResultJSONCommandsOnlyWhenVerbose(t *testing.T) {
	plain, err := json.Marshal(StepResult{Step: "docker", Status: StatusOK, Detail: "x"})
	if err != nil || strings.Contains(string(plain), "commands") {
		t.Fatalf("plain = %s, %v", plain, err)
	}
	none := []string{}
	verbose, err := json.Marshal(StepResult{Step: "token", Status: StatusOK, Detail: "x", Commands: &none})
	if err != nil || !strings.Contains(string(verbose), `"commands":[]`) {
		t.Fatalf("verbose = %s, %v", verbose, err)
	}
}

// verboseRun runs every step of setup against f as the CLI's --verbose does. It returns the results, each with its commands,
// and what the log printed.
func verboseRun(t *testing.T, f *agentFake, exe string, dry bool, mod func(*Options)) ([]StepResult, string) {
	t.Helper()
	s := agentSession(t, f, exe, dry, mod)
	var log bytes.Buffer
	s.Log = NewCommandLog(&log)
	s.R = Recording(f, s.Log)
	got, ok := runSteps(s)
	if !ok {
		t.Fatalf("setup stopped: %+v", got)
	}
	return got, log.String()
}

// withoutProbeScripts drops the probes of a step: a step that changed the server probes it again to read what changed, and a dry
// run, which changes nothing, does not. The probe is listed once for the run, under the check step.
func withoutProbeScripts(scripts []string) []string {
	var out []string
	for _, sc := range scripts {
		if !strings.Contains(sc, "uname -s") {
			out = append(out, sc)
		}
	}
	return out
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// The dry run of --verbose lists, for every step that would change the server, the commands a real run sends in that step, and
// every script a real run sends is in the dry run's log.
func TestDryRunListsWhatARealRunSendsInEachStep(t *testing.T) {
	wrap, err := keywrap.Generate()
	if err != nil {
		t.Fatal(err)
	}
	exe := elfFile(t, emX86)
	scenarios := []struct {
		name string
		fake func(t *testing.T) *agentFake
		mod  func(t *testing.T, o *Options)
	}{
		{"agent installed on a fresh server", freshAgentServer, nil},
		{"new binary", func(t *testing.T) *agentFake {
			f := setUpAgent(t, exe)
			f.probe = with(without(f.probe, "bin_sha="), "bin_sha="+strings.Repeat("c", 64))
			return f
		}, nil},
		{"new configuration", func(t *testing.T) *agentFake {
			f := setUpAgent(t, exe)
			f.probe = with(without(f.probe, "network="), "network=overlay swarm true newnet")
			return f
		},
			func(_ *testing.T, o *Options) { o.Network = "newnet" }},
		{"unit changed", func(t *testing.T) *agentFake {
			f := setUpAgent(t, exe)
			f.probe = with(without(f.probe, "unit_sha="), "unit_sha="+strings.Repeat("d", 64))
			return f
		}, nil},
		{"key restored from a file", func(t *testing.T) *agentFake {
			f := setUpAgent(t, exe)
			f.probe = without(f.probe, "key_sha=", "key_stat=", "state_db=")
			return f
		}, func(t *testing.T, o *Options) { o.KeyFile = writeKey(t, keyBytes()) }},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			home := t.TempDir()
			mod := func(o *Options) {
				o.KeyHome = home
				o.NewWrapKey = func() (*keywrap.Private, error) { return wrap, nil }
				if sc.mod != nil {
					sc.mod(t, o)
				}
			}
			dryServer, liveServer := sc.fake(t), sc.fake(t)
			dryServer.key, liveServer.key = bytes.Repeat([]byte{37}, 32), bytes.Repeat([]byte{37}, 32)
			dry, dryLog := verboseRun(t, dryServer, exe, true, mod)
			live, _ := verboseRun(t, liveServer, exe, false, mod)
			realByStep := map[string]StepResult{}
			for _, r := range live {
				realByStep[r.Step] = r
			}
			var dryAll, realAll []string
			for _, d := range dry {
				dryAll = append(dryAll, *d.Commands...)
			}
			for _, r := range live {
				realAll = append(realAll, *r.Commands...)
			}
			for _, c := range realAll {
				if !slices.Contains(dryAll, c) {
					t.Errorf("a real run sends a script the dry run does not list:\n%s", c)
				}
			}
			changes := 0
			for _, d := range dry {
				// The steps that would change, and the skipped verify step, which a real run checks the agent in.
				if d.Status != StatusWouldChange && d.Status != StatusSkipped {
					continue
				}
				r := realByStep[d.Step]
				if d.Status == StatusWouldChange {
					changes++
					if r.Status != StatusChanged && !(d.Step == "cli_token" && r.Status == StatusSkipped) {
						t.Errorf("%s: the dry run says would_change, the real run says %s", d.Step, r.Status)
					}
				}
				if !slices.Equal(sortedCopy(withoutProbeScripts(*d.Commands)), sortedCopy(withoutProbeScripts(*r.Commands))) {
					t.Errorf("%s: the dry run lists\n%q\nthe real run sends\n%q", d.Step, *d.Commands, *r.Commands)
				}
			}
			if changes == 0 {
				t.Fatal("no step would change: the scenario tests nothing")
			}
			if !strings.Contains(dryLog, "→ not run: dry run") {
				t.Errorf("the dry run's log has no command marked as not run:\n%s", dryLog)
			}
		})
	}
}

// The verbose log of a real run holds none of the key, in any form, and no stdin: the key travels on stdin only.
func TestVerboseLogOfAKeyFileInstallNeverHoldsTheKey(t *testing.T) {
	exe := elfFile(t, emX86)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(0x90 + 7*i)
	}
	f := freshAgentServer(t)
	got, log := verboseRun(t, f, exe, false, func(o *Options) { o.KeyFile = writeKey(t, key) })
	forms := []string{string(key), hex.EncodeToString(key), base64.StdEncoding.EncodeToString(key), base64.RawStdEncoding.EncodeToString(key),
		base64.URLEncoding.EncodeToString(key), base64.RawURLEncoding.EncodeToString(key)}
	if !f.ran("cat > \"$tmp\"") {
		t.Fatal("the key was not uploaded: the test proves nothing")
	}
	for _, form := range forms {
		if strings.Contains(log, form) {
			t.Errorf("the log holds the key in some form")
		}
		for _, r := range got {
			for _, c := range *r.Commands {
				if strings.Contains(c, form) {
					t.Errorf("%s: a command holds the key", r.Step)
				}
			}
		}
	}
}

func TestStyledCommandLogPreservesRecording(t *testing.T) {
	var out bytes.Buffer
	log := NewStyledCommandLog(&out, CommandLogStyle{Prompt: func(s string) string { return "[" + s + "]" }, Exit: func(_ int, s string) string { return "[" + s + "]" }})
	log.begin("test")
	r := Recording(funcRunner{run: func(string, io.Reader) Result { return Result{Code: 3, Stdout: []byte("hidden-output")} }}, log)
	_, err := r.Run(context.Background(), "echo hello", strings.NewReader("hidden-input"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "# test\n[$] echo hello\n[→ exit 3]\n"; got != want {
		t.Fatalf("got %q", got)
	}
	log.exited(Result{Code: 0}, nil)
	if !strings.Contains(out.String(), "[→ exit 0]\n") {
		t.Fatal(out.String())
	}
	if got := log.end(); !reflect.DeepEqual(*got, []string{"echo hello"}) {
		t.Fatal(got)
	}
}
