package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"berthy/internal/credstore/credtest"
)

type outcomeRunner struct {
	stage    string
	calls    []string
	restarts int
}

type journalExitRunner struct {
	*outcomeRunner
	journalCode, rollbackCode int
}

func (r *journalExitRunner) Run(ctx context.Context, script string, stdin io.Reader) (Result, error) {
	if strings.HasPrefix(script, "journalctl") {
		r.calls = append(r.calls, script)
		return Result{Code: r.journalCode}, nil
	}
	if r.rollbackCode != 0 && script == "/bin/sh "+guardPath+" no yes now" {
		r.calls = append(r.calls, script)
		return Result{Code: r.rollbackCode}, nil
	}
	return r.outcomeRunner.Run(ctx, script, stdin)
}

func TestJournalExitDoesNotPreventRollback(t *testing.T) {
	for _, tc := range []struct {
		journal, rollback int
		want              ConfigOutcome
	}{
		{1, 0, ConfigPrevious}, {255, 0, ConfigPrevious}, {255, 255, ConfigUnconfirmed},
	} {
		t.Run(fmt.Sprintf("journal_%d_rollback_%d", tc.journal, tc.rollback), func(t *testing.T) {
			r := &journalExitRunner{outcomeRunner: &outcomeRunner{}, journalCode: tc.journal, rollbackCode: tc.rollback}
			err := Activate(context.Background(), r, "127.0.0.1:7710", Staged{Config: true}, time.Second)
			found := false
			for _, script := range r.calls {
				if script == "/bin/sh "+guardPath+" no yes now" {
					found = true
				}
			}
			if !found {
				t.Fatal("journal exit prevented rollback")
			}
			var ae *ActivateError
			if !errors.As(err, &ae) || ae.ConfigOutcome != tc.want {
				t.Fatal("the configuration outcome did not reflect rollback")
			}
		})
	}
}

type uncertainSetupRunner struct {
	*agentFake
	swap bool
	hash bool
}

func (r *uncertainSetupRunner) Run(ctx context.Context, script string, stdin io.Reader) (Result, error) {
	if strings.Contains(script, "sha256sum "+configPath) {
		r.hash = true
	}
	if strings.Contains(script, "mv -f "+configPath+".new "+configPath) {
		r.swap = true
		return Result{Code: 255}, nil
	}
	return r.agentFake.Run(ctx, script, stdin)
}

func TestUnconfirmedTokenIsNotAnAdminCredential(t *testing.T) {
	f := freshAgentServer(t)
	r := &uncertainSetupRunner{agentFake: f}
	store := credtest.NewMemory()
	s := agentSession(t, r, elfFile(t, emX86), false, func(o *Options) { o.Tokens = store; o.Getenv = func(string) string { return "" } })
	_, ok := runSteps(s)
	if ok || !r.swap || r.hash || s.Token == "" || s.TokenLive || !s.TokenUncertain || s.AdminToken() != "" || store.Calls() != 0 {
		t.Fatal("unconfirmed token became a live admin credential")
	}
}

func (r *outcomeRunner) Target() string { return "fixture" }
func (r *outcomeRunner) Run(_ context.Context, script string, _ io.Reader) (Result, error) {
	r.calls = append(r.calls, script)
	switch {
	case strings.Contains(script, "sha256sum "+configPath):
		return Result{}, errors.New("unexpected hash inquiry")
	case strings.Contains(script, "mv -f "+configPath+".new "+configPath):
		switch r.stage {
		case "swap cancelled":
			return Result{}, context.Canceled
		case "swap 255":
			return Result{Code: 255}, nil
		}
		if r.stage == "stop error" || r.stage == "stop 255" {
			return Result{}, nil
		}
		if r.stage == "binary rollback error" {
			return Result{Stdout: []byte("prev_bin=yes\n")}, nil
		}
		return Result{Stdout: []byte("prev_cfg=yes\n")}, nil
	case strings.HasPrefix(script, "journalctl") && r.stage == "journal error":
		return Result{}, errors.New("journal transport lost")
	case strings.HasPrefix(script, "journalctl") && r.stage == "journal 255":
		return Result{Code: 255}, nil
	case strings.HasPrefix(script, "/bin/sh "+guardPath+" "):
		switch r.stage {
		case "rollback error", "binary rollback error", "stop error", "after rollback":
			return Result{}, errors.New("transport lost")
		case "rollback 255", "stop 255":
			return Result{Code: 255}, nil
		}
		if strings.Contains(script, " no no now") {
			return Result{Stdout: []byte("guard=stopped\n")}, nil
		}
		return Result{Stdout: []byte("guard=restored\n")}, nil
	case strings.Contains(script, "systemctl restart berthy"):
		r.restarts++
		if r.stage == "recovery answers" && r.restarts == 2 {
			return Result{}, nil
		}
		if r.stage == "restart error" || r.stage == "after rollback" && r.restarts == 2 {
			return Result{}, errors.New("transport lost")
		}
		return Result{Code: 1}, nil
	}
	return Result{}, nil
}

func TestActivationReportsConfigurationOutcome(t *testing.T) {
	for _, tc := range []struct{ stage, want string }{
		{"already cancelled", "previous"}, {"swap cancelled", "unconfirmed"}, {"swap 255", "unconfirmed"},
		{"rollback 255", "unconfirmed"}, {"rollback error", "unconfirmed"},
		{"after rollback", "unconfirmed"}, {"restart error", "unconfirmed"}, {"journal error", "unconfirmed"}, {"journal 255", "previous"}, {"stop error", "unconfirmed"}, {"stop 255", "unconfirmed"}, {"binary rollback error", "unconfirmed"},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.stage == "already cancelled" {
				cancel()
			}
			r := &outcomeRunner{stage: tc.stage}
			err := Activate(ctx, r, "127.0.0.1:7710", Staged{Config: true, Binary: tc.stage == "binary rollback error"}, time.Second)
			var ae *ActivateError
			if !errors.As(err, &ae) {
				t.Fatal("activation did not report its configuration outcome")
			}
			if string(ae.ConfigOutcome) != tc.want {
				t.Fatalf("configuration outcome is not %s", tc.want)
			}
			if (tc.stage == "already cancelled" || tc.stage == "swap cancelled") && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation cause was lost")
			}
			if tc.stage == "already cancelled" && len(r.calls) != 0 {
				t.Fatal("a swap was attempted after cancellation")
			}
			for _, script := range r.calls {
				if strings.Contains(script, "sha256sum "+configPath) {
					t.Fatal("activation made a second remote inquiry")
				}
			}
		})
	}
}
