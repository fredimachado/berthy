package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const removalTestPaths = "12 34\n12 35\n12 36\nabsent\n/peer/state\x00/peer/key\x00/peer/key.next\x00/peer/key.prev\x00/\n"
const removalTestNetworkID = "wcxk9hlk7pka26iwkly94laa0"

type removalRunner struct {
	t                           *testing.T
	live, previous, new, record []byte
	guard, paths                string
	mutations                   []string
	restartErr, stageErr        bool
	restartCode, recoveryCode   int
	recordConfirmationErr       bool
	daemonCode, pathsCode       int
	failTimer, restoreScheduled bool
	onSwap                      func()
	stateClean, networkGone     bool
}

func newRemovalRunner(t *testing.T) (*removalRunner, Options) {
	t.Helper()
	config := `{"listen":"127.0.0.1:7710","environments":[` +
		strings.Replace(removalHostedJSON("alpha", "dev", "/custom/target", "1"), `"tokens"`, `"network":"alpha-dev","tokens"`, 1) +
		",\n" + removalHostedJSON("peer", "dev", "/peer", "2") + "]}\n"
	cli := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cli, []byte(`{"projects":{"alpha":{"environments":{"dev":{"url":"http://127.0.0.1:7710","token_env":"FIXTURE_TOKEN"}}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	r := &removalRunner{t: t, live: []byte(config), paths: removalTestPaths}
	return r, Options{Project: "alpha", Env: "dev", CLIConfig: cli, Now: func() time.Time {
		return time.Date(2026, 11, 28, 12, 0, 0, 0, time.FixedZone("AEST", 10*60*60))
	}}
}

func (*removalRunner) Target() string { return "fixture" }

func (r *removalRunner) Run(_ context.Context, script string, stdin io.Reader) (Result, error) {
	r.t.Helper()
	result := func(s string) (Result, error) { return Result{Stdout: []byte(s)}, nil }
	payload := func() []byte {
		r.t.Helper()
		if stdin == nil {
			r.t.Fatalf("command received no stdin: %s", script)
		}
		b, err := io.ReadAll(stdin)
		if err != nil {
			r.t.Fatal(err)
		}
		return b
	}
	switch {
	case script == "id -u; id -un":
		return result("0\nroot\n")
	case strings.HasPrefix(script, "# inspect removal configuration\n"):
		return result("present\n")
	case script == "cat "+configPath:
		return Result{Stdout: r.live}, nil
	case strings.HasPrefix(script, "# read removal record\n"):
		if r.record == nil {
			return Result{Code: 3}, nil
		}
		return Result{Stdout: r.record}, nil
	case strings.HasPrefix(script, "# read previous removal configuration\n"):
		if r.previous == nil {
			return Result{Code: 3}, nil
		}
		return Result{Stdout: r.previous}, nil
	case strings.HasPrefix(script, "# read removal guard state\n"):
		if r.guard == "" {
			return Result{Code: 3}, nil
		}
		return result(r.guard + "\n")
	case strings.HasPrefix(script, "# recover orphaned removal guard\n"):
		if r.restoreScheduled || r.guard != "pending" || !strings.Contains(script, shellQuote(configDigest(r.live))) {
			return Result{Code: 1}, nil
		}
		if strings.Contains(script, "rm -- "+shellQuote(guardState)) {
			r.guard = ""
			r.mutations = append(r.mutations, "clear-orphan")
		}
		return Result{}, nil
	case strings.HasPrefix(script, "# check applied apps\n"):
		if !strings.Contains(script, "'/custom/target/state'") {
			r.t.Fatalf("probe did not use configured state: %q", script)
		}
		return result("empty\n")
	case strings.HasPrefix(script, "# inspect removal paths\n"):
		return Result{Code: r.pathsCode, Stdout: []byte(r.paths)}, nil
	case strings.HasPrefix(script, "# verify removal Docker service\n"):
		return Result{Code: r.daemonCode}, nil
	case strings.Contains(script, "network ls"):
		if r.networkGone {
			return result("")
		}
		return result(removalTestNetworkID + " alpha-dev\n")
	case strings.Contains(script, "network inspect"):
		return result(removalNetworkJSON(removalTestNetworkID, "{}"))
	case strings.Contains(script, "container ls -aq"), strings.Contains(script, "service ls -q"):
		return result("")
	case strings.Contains(script, "network rm"):
		r.mutations = append(r.mutations, "network")
		r.networkGone = true
		return result(removalTestNetworkID + "\n")
	case strings.HasPrefix(script, "# remove state and keys\n"):
		if r.stateClean {
			return result("clean\n")
		}
		r.mutations = append(r.mutations, "state")
		r.stateClean = true
		return result("changed\n")
	case strings.HasPrefix(script, "# clear removal record\n"):
		if !r.stateClean || !r.networkGone {
			r.t.Fatal("record cleared before server cleanup")
		}
		r.mutations = append(r.mutations, "clear")
		r.record = nil
		return Result{}, nil
	case strings.HasPrefix(script, "# write removal record\n"):
		var rec removal
		b := payload()
		if err := json.Unmarshal(b, &rec); err != nil {
			r.t.Fatal(err)
		}
		r.mutations = append(r.mutations, "record:"+map[bool]string{true: "confirmed", false: "pending"}[rec.Activated])
		if rec.Activated && r.recordConfirmationErr {
			return Result{Code: 1, Stdout: []byte("private payload")}, nil
		}
		r.record = b
		return Result{}, nil
	case script == configWriteScript():
		r.mutations = append(r.mutations, "stage")
		r.new = payload()
		if r.stageErr {
			return Result{}, errors.New("private payload")
		}
		return Result{}, nil
	case strings.Contains(script, "mv -f "+configPath+".new "+configPath):
		r.mutations = append(r.mutations, "swap")
		if r.onSwap != nil {
			r.onSwap()
		}
		if strings.Contains(script, binPath) || !strings.Contains(string(payload()), "http://127.0.0.1:7710/healthz") {
			r.t.Fatalf("wrong activation script %q", script)
		}
		r.guard = "pending"
		if r.failTimer {
			return Result{Code: 1, Stderr: []byte("timer registration failed\n")}, nil
		}
		r.live, r.previous, r.new = r.new, r.live, nil
		return result("prev_cfg=yes\n")
	case strings.HasPrefix(script, "/bin/sh "+guardPath):
		r.mutations = append(r.mutations, "recover")
		if r.recoveryCode != 0 {
			r.guard = "restore-failed"
			return Result{Code: r.recoveryCode, Stdout: []byte("guard=restore-failed\n")}, nil
		}
		r.live, r.previous = r.previous, nil
		r.guard = "restored"
		return result("guard=restored\n")
	case strings.Contains(script, "systemctl restart berthy"):
		r.mutations = append(r.mutations, "restart")
		if !strings.Contains(script, "http://127.0.0.1:7710/healthz") {
			r.t.Fatalf("wrong process health URL: %q", script)
		}
		if r.restartErr {
			return Result{}, errors.New("SSH connection lost")
		}
		if r.restartCode == 0 {
			r.guard = "confirmed"
		}
		return Result{Code: r.restartCode}, nil
	case strings.HasPrefix(script, "journalctl -u berthy "):
		return result("fixture journal\n")
	default:
		r.t.Fatalf("unexpected remote command: %q", script)
		return Result{}, nil
	}
}

func runRemoval(r *removalRunner, opts Options, purge, preview bool) (*Session, string) {
	s := &Session{R: r, Opts: opts, DryRun: preview}
	var failure string
	steps := RemovalSteps(purge)
	for i, step := range steps {
		if step.Name == "service" {
			steps = steps[:i+1]
			break
		}
	}
	Run(context.Background(), s, steps, func(res StepResult) {
		if res.Status == StatusFailed {
			failure = res.Detail
		}
	})
	return s, failure
}

func (r *removalRunner) checkMutations(want string) {
	r.t.Helper()
	if got := strings.Join(r.mutations, ","); got != want {
		r.t.Fatalf("mutations = %q, want %q", got, want)
	}
}

func TestRemovalStagesOnlyConfigAndConfirmsProcessHealth(t *testing.T) {
	r, opts := newRemovalRunner(t)
	original := bytes.Clone(r.live)
	want := `{"listen":"127.0.0.1:7710","environments":[` + removalHostedJSON("peer", "dev", "/peer", "2") + "]}\n"
	if _, failure := runRemoval(r, opts, false, true); failure != "" || !bytes.Equal(r.live, original) {
		t.Fatalf("preview: %q, config changed=%t", failure, !bytes.Equal(r.live, original))
	}
	r.checkMutations("")
	s, failure := runRemoval(r, opts, false, false)
	if failure != "" || s.removal == nil || !s.removal.Activated || string(r.live) != want || !bytes.Equal(r.previous, original) {
		t.Fatalf("activation: %q, record=%+v, config=%q", failure, s.removal, r.live)
	}
	r.checkMutations("record:pending,stage,swap,restart,record:confirmed")
}

func TestRemovalRollbackLeavesTargetUsable(t *testing.T) {
	for _, tt := range []struct {
		name                     string
		restartCode, recoverCode int
		lost                     bool
		mutations, text          string
	}{
		{"failed health", 1, 0, false, "record:pending,stage,swap,restart,recover", "previous one was put back"},
		{"unconfirmed SSH", 0, 0, true, "record:pending,stage,swap,restart", "SSH connection lost"},
		{"failed recovery", 1, 1, false, "record:pending,stage,swap,restart,recover", "previous one could not be put back"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, opts := newRemovalRunner(t)
			original := bytes.Clone(r.live)
			local, err := os.ReadFile(opts.CLIConfig)
			if err != nil {
				t.Fatal(err)
			}
			r.restartCode, r.restartErr, r.recoveryCode = tt.restartCode, tt.lost, tt.recoverCode
			s, failure := runRemoval(r, opts, false, false)
			if !strings.Contains(failure, tt.text) || s.removal == nil || s.removal.Activated {
				t.Fatalf("failure=%q record=%+v", failure, s.removal)
			}
			r.checkMutations(tt.mutations)
			kept := r.live
			if r.guard != "restored" {
				kept = r.previous
			}
			after, err := os.ReadFile(opts.CLIConfig)
			if !bytes.Equal(kept, original) || err != nil || !bytes.Equal(local, after) {
				t.Fatalf("rollback lost original config or changed local context: %v", err)
			}
		})
	}
}

func TestRemovalRecordsFileIdentitiesBeforeActivation(t *testing.T) {
	r, opts := newRemovalRunner(t)
	r.onSwap = func() {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(r.record, &fields); err != nil {
			t.Fatal(err)
		}
		for k, want := range map[string]string{
			"StateIdentity": `{"Device":12,"Inode":34}`, "KeyIdentity": `{"Device":12,"Inode":35}`,
			"NextIdentity": `{"Device":12,"Inode":36}`, "PrevIdentity": "null",
			"Activated": "false", "NetworkID": `"` + removalTestNetworkID + `"`,
			"Archive": `"/custom/target/state.removed-20261128T020000Z"`,
		} {
			if got := string(fields[k]); got != want {
				t.Errorf("record %s = %s, want %s", k, got, want)
			}
		}
		if strings.Contains(string(r.record), "tokens") {
			t.Fatal("record contains a copy of the configuration")
		}
	}
	if _, failure := runRemoval(r, opts, false, false); failure != "" {
		t.Fatal(failure)
	}
	for _, name := range []string{"failed stat", "malformed stat", "inactive Docker", "invalid CLI config", "guard still running"} {
		t.Run(name, func(t *testing.T) {
			r, opts := newRemovalRunner(t)
			switch name {
			case "failed stat":
				r.pathsCode = 1
			case "malformed stat":
				r.paths = strings.Replace(r.paths, "12 36", "private payload", 1)
			case "inactive Docker":
				r.daemonCode = 1
			case "guard still running":
				r.guard = "pending"
			case "invalid CLI config":
				if err := os.WriteFile(opts.CLIConfig, []byte("malformed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, failure := runRemoval(r, opts, false, false); failure == "" || strings.Contains(failure, "private payload") {
				t.Fatalf("preflight result %q", failure)
			}
			r.checkMutations("")
		})
	}
}

func TestRemovalResumesOnlyItsConfirmedGeneration(t *testing.T) {
	for _, tt := range []struct {
		name, guard, want, text string
	}{
		{"after staging", "", "stage,swap,restart,record:confirmed", ""},
		{"restored", "restored", "stage,swap,restart,record:confirmed", ""},
		{"pending", "pending", "", "guard is still running"},
		{"recovering", "recovering", "", "guard is still running"},
		{"confirmed", "confirmed", "record:confirmed", ""},
		{"kept", "kept", "record:confirmed", ""},
		{"lost guard", "", "", configPath + ".prev"},
		{"mismatched previous", "confirmed", "", "previous configuration"},
		{"unrelated live", "confirmed", "", "configuration changed"},
		{"recording failed", "confirmed", "record:confirmed", ""},
		{"already activated", "", "", ""},
		{"missing sidecar field", "confirmed", "", "invalid removal record"},
		{"missing identity member", "confirmed", "", "invalid removal record"},
		{"replacement state", "restored", "", "file identities changed"},
		{"changed purge", "restored", "", "different purge choice"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, opts := newRemovalRunner(t)
			original := bytes.Clone(r.live)
			r.stageErr = tt.name == "after staging"
			r.recordConfirmationErr = tt.name == "recording failed"
			r.restartErr = !r.stageErr && !r.recordConfirmationErr && tt.name != "already activated"
			_, failure := runRemoval(r, opts, false, false)
			if (failure == "") != (tt.name == "already activated") || strings.Contains(failure, "private payload") {
				t.Fatalf("first run = %q", failure)
			}
			r.stageErr, r.restartErr, r.recordConfirmationErr = false, false, false
			r.guard, r.mutations = tt.guard, nil
			switch tt.name {
			case "restored", "replacement state", "changed purge":
				r.live, r.previous = original, nil
			case "mismatched previous":
				r.previous = append(bytes.Clone(original), '\n')
			case "unrelated live":
				r.live = append(bytes.Clone(r.live), '\n')
			case "already activated":
				r.pathsCode = 1 // Cleanup may already have moved the target.
			case "missing sidecar field":
				r.record = bytes.Replace(r.record, []byte(`"PrevIdentity":null,`), nil, 1)
			case "missing identity member":
				r.record = bytes.Replace(r.record, []byte(`"Device":12,`), nil, 1)
			}
			if tt.name == "replacement state" {
				r.paths = strings.Replace(r.paths, "12 34", "12 99", 1)
			}
			s, failure := runRemoval(r, opts, tt.name == "changed purge", false)
			if tt.text == "" {
				if failure != "" || s.removal == nil || !s.removal.Activated {
					t.Fatalf("resume = %q, record=%+v", failure, s.removal)
				}
			} else if !strings.Contains(failure, tt.text) {
				t.Fatalf("resume = %q, want %q", failure, tt.text)
			}
			r.checkMutations(tt.want)
		})
	}
}

func TestRemovalRejectsTruncatedRecordPayload(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses Linux stat and the shell runner")
	}
	file := filepath.Join(t.TempDir(), "record")
	raw := "invalid prefix" + strings.Repeat(" ", maxCaptured) + "{}"
	if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	r := funcRunner{run: func(script string, stdin io.Reader) Result {
		result, err := runProcess(context.Background(), "sh", []string{"-c", script}, stdin)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}}
	got, err := readRemovalFile(context.Background(), r, "removal record", file, "$(id -u)")
	if err == nil || len(got) != 0 {
		t.Fatalf("oversized payload returned %d bytes and %v", len(got), err)
	}
}
