package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"berthy/internal/api"
	"berthy/internal/keywrap"
	"berthy/internal/scope"
)

type transportInjection struct {
	Runner
	match string
	calls []string
}

func (r *transportInjection) Run(ctx context.Context, script string, in io.Reader) (Result, error) {
	r.calls = append(r.calls, script)
	if r.match == "service inspect" && strings.Contains(script, "service ls -q") {
		return Result{Stdout: []byte("wcxk9hlk7pka26iwkly94laa0\n")}, nil
	}
	if strings.Contains(script, r.match) {
		return Result{Stdout: []byte("private-secret"), Stderr: []byte("private-secret")}, &SSHTransportError{Target: "ops@s1"}
	}
	return r.Runner.Run(ctx, script, in)
}
func (r *transportInjection) runSSHLogin(context.Context, string) (Result, error) {
	r.calls = append(r.calls, "login")
	return Result{}, &SSHTransportError{Target: "ops@s1"}
}

func TestVerifyReportsSSHFailure(t *testing.T) {
	r := &transportInjection{Runner: &erroring{}, match: ""}
	s := &Session{R: r, Opts: Options{ListenIP: []byte{127, 0, 0, 1}, ListenPort: 7710}}
	_, _, err := verifyStep(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "SSH connection to ops@s1 failed") {
		t.Fatalf("err=%v", err)
	}
}
func TestVerifyDoctorPreservesSafeSSHFailure(t *testing.T) {
	const token = "bty.private.token"
	r := &transportInjection{Runner: &erroring{}, match: "agent setup-probe doctor"}
	s := &Session{R: r, Token: token, TokenLive: true, Opts: Options{Project: "alpha", Env: "dev", ListenIP: []byte{127, 0, 0, 1}, ListenPort: 7710, Doctor: func(context.Context, string, string, string, string) ([]api.Check, error) { return nil, ErrUnreachable }}}
	_, _, err := verifyStep(context.Background(), s)
	var transport *SSHTransportError
	if !errors.As(err, &transport) || !strings.Contains(err.Error(), "SSH connection to ops@s1 failed") || strings.Contains(err.Error(), token) {
		t.Fatalf("err=%v", err)
	}
}

func TestServiceUnitInstallPreservesSSHFailure(t *testing.T) {
	r := &transportInjection{Runner: &erroring{}, match: "systemctl daemon-reload"}
	s := &Session{R: r, F: &Facts{}}
	_, _, err := serviceStep(context.Background(), s)
	var transport *SSHTransportError
	if !errors.As(err, &transport) || !strings.Contains(err.Error(), "SSH connection to ops@s1 failed") {
		t.Fatalf("err=%v", err)
	}
}

func TestKeyCopyReportsSSHFailure(t *testing.T) {
	wrap, err := keywrap.Generate()
	if err != nil {
		t.Fatal(err)
	}
	r := &copyRunner{answer: func(context.Context, []byte) (Result, error) {
		return Result{Stdout: []byte("private-secret"), Stderr: []byte("private-secret")}, &SSHTransportError{Target: "ops@s1", reason: " private-secret"}
	}}
	s := &Session{R: r, Opts: Options{Project: "alpha", Env: "dev", KeyHome: t.TempDir(), NewWrapKey: func() (*keywrap.Private, error) { return wrap, nil }}}
	status, detail, err := keyCopyStep(context.Background(), s)
	if err != nil || status != StatusSkipped || s.KeyCopyPath != "" || !strings.Contains(detail, "SSH connection to ops@s1 failed") || strings.Contains(detail, "private-secret") {
		t.Fatalf("status=%s detail=%q path=%q err=%v", status, detail, s.KeyCopyPath, err)
	}
}
func TestUploadMismatchReportsLostCleanupConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(path, []byte("binary"), 0600); err != nil {
		t.Fatal(err)
	}
	r := &transportInjection{Runner: &erroring{err: nil}, match: "rm -f"}
	err := uploadBinary(context.Background(), &Session{R: r}, &binarySource{local: path, sha: "expected"}, "upload")
	if err == nil || !strings.Contains(err.Error(), "SHA-256 differs") || !strings.Contains(err.Error(), "SSH connection to ops@s1 failed") {
		t.Fatalf("err=%v", err)
	}
}

type journalTransportRunner struct {
	*outcomeRunner
	decided bool
}

func (r *journalTransportRunner) Run(ctx context.Context, script string, in io.Reader) (Result, error) {
	if strings.HasPrefix(script, "journalctl") {
		r.calls = append(r.calls, script)
		return Result{}, &SSHTransportError{Target: "ops@s1"}
	}
	if r.decided && strings.Contains(script, "systemctl restart berthy") {
		r.calls = append(r.calls, script)
		return Result{Code: guardDecided, Stdout: []byte("guard=restored\n")}, nil
	}
	return r.outcomeRunner.Run(ctx, script, in)
}
func TestJournalSSHFailurePreservesDecision(t *testing.T) {
	for _, decided := range []bool{false, true} {
		t.Run(map[bool]string{false: "added environment", true: "known decision"}[decided], func(t *testing.T) {
			r := &journalTransportRunner{outcomeRunner: &outcomeRunner{}, decided: decided}
			err := Activate(context.Background(), r, "127.0.0.1:7710", Staged{Config: true, Added: &scope.ID{Project: "alpha", Environment: "dev"}}, time.Second)
			var transport *SSHTransportError
			var ae *ActivateError
			want := ConfigUnconfirmed
			if decided {
				want = ConfigPrevious
			}
			if !errors.As(err, &transport) || !errors.As(err, &ae) || ae.ConfigOutcome != want || !strings.HasPrefix(r.calls[len(r.calls)-1], "journalctl") {
				t.Fatalf("err=%v call count=%v outcome=%+v", err, len(r.calls), ae)
			}
		})
	}
}
func TestRemovalReportsSSHFailure(t *testing.T) {
	for _, tc := range []struct{ match, step string }{
		{"# inspect removal configuration", "check"}, {"cat /etc/berthy/agent.json", "check"}, {"# read removal record", "check"}, {"# read removal guard state", "check"}, {"# inspect removal paths", "check"}, {"# check applied apps", "check"}, {"# verify removal Docker service", "check"}, {"network ls", "check"}, {"network inspect", "check"}, {"container ls -aq", "check"}, {"service ls -q", "check"}, {"service inspect", "check"}, {"# write removal record", "check"}, {"cat > /etc/berthy/agent.json.new", "config"}, {"# remove state and keys", "state"}, {"network rm", "network"}, {"# clear removal record", "cli_config"},
	} {
		t.Run(tc.match, func(t *testing.T) {
			base, opts := newRemovalRunner(t)
			r := &transportInjection{Runner: base, match: tc.match}
			s := &Session{R: r, Opts: opts}
			var failure StepResult
			Run(context.Background(), s, RemovalSteps(false), func(result StepResult) {
				if result.Status == StatusFailed {
					failure = result
				}
			})
			if failure.Step != tc.step || !strings.Contains(failure.Detail, "SSH connection to ops@s1 failed") || !strings.Contains(r.calls[len(r.calls)-1], tc.match) {
				t.Fatalf("failed=%+v last=%q mutations=%v", failure, r.calls[len(r.calls)-1], base.mutations)
			}
		})
	}
	t.Run("login address retains entry and record", func(t *testing.T) {
		base, opts := newRemovalRunner(t)
		opts.ListenIP = []byte{10, 1, 2, 3}
		opts.ListenPort = 7710
		before := []byte(`{"projects":{"alpha":{"environments":{"dev":{"url":"http://10.1.2.3:7710","token_env":"FIXTURE_TOKEN"}}}}}`)
		if err := os.WriteFile(opts.CLIConfig, before, 0600); err != nil {
			t.Fatal(err)
		}
		base.record = []byte("pending")
		base.stateClean, base.networkGone = true, true
		r := &transportInjection{Runner: base, match: "never"}
		s := &Session{R: r, Opts: opts, removal: &removal{ID: scope.ID{Project: "alpha", Environment: "dev"}, Activated: true}}
		_, _, err := removeCLIConfigStep(context.Background(), s)
		after, readErr := os.ReadFile(opts.CLIConfig)
		if err == nil || !strings.Contains(err.Error(), "SSH connection to ops@s1 failed") || !bytes.Equal(before, after) || readErr != nil || string(base.record) != "pending" || len(r.calls) != 1 || r.calls[0] != "login" {
			t.Fatalf("err=%v config=%q record=%q calls=%v", err, after, base.record, r.calls)
		}
	})
	for _, mode := range []string{"orphan", "previous"} {
		t.Run(mode, func(t *testing.T) {
			base, opts := newRemovalRunner(t)
			if mode == "orphan" {
				base.failTimer = true
				_, _ = runRemoval(base, opts, false, false)
				base.failTimer = false
			} else {
				_, failure := runRemoval(base, opts, false, false)
				if failure != "" {
					t.Fatal(failure)
				}
				var rec removal
				if err := json.Unmarshal(base.record, &rec); err != nil {
					t.Fatal(err)
				}
				rec.Activated = false
				var err error
				base.record, err = json.Marshal(rec)
				if err != nil {
					t.Fatal(err)
				}
			}
			match := "# check for orphaned removal guard"
			if mode == "previous" {
				match = "# read previous removal configuration"
			}
			r := &transportInjection{Runner: base, match: match}
			var failure StepResult
			Run(context.Background(), &Session{R: r, Opts: opts}, RemovalSteps(false), func(result StepResult) {
				if result.Status == StatusFailed {
					failure = result
				}
			})
			if failure.Step != "check" || !strings.Contains(failure.Detail, "SSH connection to ops@s1 failed") || !strings.Contains(r.calls[len(r.calls)-1], match) {
				t.Fatalf("failed=%+v last=%q", failure, r.calls[len(r.calls)-1])
			}
		})
	}

}

// clearThenLoseRunner models a deletion that completed before the response was lost.
type clearThenLoseRunner struct{ *removalRunner }

func (r *clearThenLoseRunner) Run(ctx context.Context, script string, in io.Reader) (Result, error) {
	res, err := r.removalRunner.Run(ctx, script, in)
	if strings.HasPrefix(script, "# clear removal record\n") && err == nil {
		return Result{}, &SSHTransportError{Target: "ops@s1"}
	}
	return res, err
}

func TestRemovalRecordDeletionResponseLost(t *testing.T) {
	base, opts := newRemovalRunner(t)
	opts.Local = true
	base.record = []byte("pending")
	base.stateClean, base.networkGone = true, true
	r := &clearThenLoseRunner{base}
	id := scope.ID{Project: "alpha", Environment: "dev"}
	s := &Session{R: r, Opts: opts, removal: &removal{ID: id, Activated: true}}
	_, _, err := removeCLIConfigStep(context.Background(), s)
	var transport *SSHTransportError
	if !errors.As(err, &transport) || base.record != nil || !strings.Contains(err.Error(), "unconfirmed") || !strings.Contains(err.Error(), removalRecordPath(id)) || !strings.Contains(err.Error(), "retry") || !strings.Contains(err.Error(), "if it remains") {
		t.Fatalf("err=%v record=%q mutations=%v", err, base.record, base.mutations)
	}
}
