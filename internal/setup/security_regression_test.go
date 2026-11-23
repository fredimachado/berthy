package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"

	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/credstore/credtest"
)

type interruptedActivation struct {
	*s15RollbackServer
	cancel      context.CancelFunc
	stage       string
	unreachable bool
}

type blockingReconcile struct{ *interruptedActivation }

type doctorErrorRunner struct {
	*agentFake
	message string
}

func (f *doctorErrorRunner) Run(ctx context.Context, script string, stdin io.Reader) (Result, error) {
	if strings.Contains(script, "-H @-") {
		return Result{}, errors.New(f.message)
	}
	return f.agentFake.Run(ctx, script, stdin)
}

func (f *blockingReconcile) Run(ctx context.Context, script string, stdin io.Reader) (Result, error) {
	if strings.Contains(script, "sha256sum "+configPath) {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
	return f.interruptedActivation.Run(ctx, script, stdin)
}

func TestInterruptedActivationBoundsReconciliation(t *testing.T) {
	f := s15Server(t, s15File(t, "other", "prod", "other-prod"))
	server := &blockingReconcile{&interruptedActivation{s15RollbackServer: &s15RollbackServer{agentFake: f, live: f.config}, stage: "after swap"}}
	s := agentSession(t, server, elfFile(t, emX86), false, nil)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		server.cancel = cancel
		var results []StepResult
		ok := Run(ctx, s, Steps(), func(r StepResult) { results = append(results, r) })
		if ok || s.TokenLive || !strings.Contains(lastResult(results).Detail, "could not confirm which agent configuration remains") {
			t.Fatal("timed out reconciliation was not reported")
		}
	})
}

func (f *interruptedActivation) Run(ctx context.Context, script string, stdin io.Reader) (Result, error) {
	if strings.Contains(script, "sha256sum "+configPath) {
		if f.unreachable {
			return Result{}, errors.New("connection lost")
		}
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{Stdout: []byte(sha256Hex([]byte(f.live)) + "  " + configPath + "\n")}, nil
	}
	if f.stage == "before swap" && strings.Contains(script, "mv -f "+configPath+".new "+configPath) {
		f.cancel()
		return Result{}, context.Canceled
	}
	if (f.stage == "after swap" || f.stage == "unreachable" || f.stage == "after rollback" && f.restarts > 0) && strings.Contains(script, "systemctl restart berthy") {
		f.cancel()
		return Result{}, context.Canceled
	}
	return f.s15RollbackServer.Run(ctx, script, stdin)
}

func TestInterruptedActivationReportsRetainedToken(t *testing.T) {
	for _, stage := range []string{"before swap", "after swap", "after rollback", "unreachable"} {
		t.Run(stage, func(t *testing.T) {
			f := s15Server(t, s15File(t, "other", "prod", "other-prod"))
			if stage == "after rollback" {
				f.waits = []int{1}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := &interruptedActivation{s15RollbackServer: &s15RollbackServer{agentFake: f, live: f.config}, cancel: cancel, stage: stage, unreachable: stage == "unreachable"}
			store := credtest.NewMemory()
			s := agentSession(t, server, elfFile(t, emX86), false, func(o *Options) { o.Tokens = store })
			var results []StepResult
			ok := Run(ctx, s, Steps(), func(r StepResult) { results = append(results, r) })
			if ok || lastResult(results).Step != "service" || !strings.Contains(lastResult(results).Detail, "context canceled") {
				t.Fatal("activation error was not preserved")
			}
			if s.TokenLive != (stage == "after swap") {
				t.Errorf("retained token live = %v", s.TokenLive)
			}
			if stage == "unreachable" && !strings.Contains(lastResult(results).Detail, "could not confirm which agent configuration remains") {
				t.Fatal("unknown configuration was not reported")
			}
			if store.Calls() != 0 {
				t.Fatal("a failed activation accessed the store")
			}
		})
	}
}

func TestVerifyRedactsStoredTokenFromDoctorDiagnostics(t *testing.T) {
	const token = "bty.fake.secret"
	for _, kind := range []string{"success", "critical", "http error", "fallback", "fallback critical", "fallback error"} {
		t.Run(kind, func(t *testing.T) {
			store := credtest.NewMemory()
			e := agentclient.Environment{URL: "http://127.0.0.1:7710", TokenEnv: "TEST_SETUP_TOKEN"}
			if err := store.Set(agentclient.StoreKey("shop", "p5", e), token); err != nil {
				t.Fatal(err)
			}
			f := &agentFake{doctorBody: doctorBody([]api.Check{{Name: "agent", OK: true, Detail: "version " + token}, {Name: "warning " + token, Detail: "detail " + token}})}
			var runner Runner = f
			var doctor DoctorFunc
			switch kind {
			case "success":
				doctor = func(context.Context, string, string, string, string) ([]api.Check, error) {
					return []api.Check{{Name: "agent", OK: true, Detail: "version " + token}, {Name: "warning " + token, Detail: "detail " + token}}, nil
				}
			case "critical":
				doctor = func(context.Context, string, string, string, string) ([]api.Check, error) {
					return []api.Check{{Name: "critical " + token, Critical: true, Detail: "detail " + token}}, nil
				}
			case "http error":
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(400)
					fmt.Fprintf(w, `{"message":%q}`, "rejected "+token)
				}))
				defer server.Close()
				doctor = func(ctx context.Context, _, project, env, bearer string) ([]api.Check, error) {
					return httpDoctor(ctx, server.URL, project, env, bearer)
				}
			case "fallback", "fallback critical", "fallback error":
				if kind == "fallback critical" {
					f.doctorBody = doctorBody([]api.Check{{Name: "critical " + token, Critical: true, Detail: "detail " + token}})
				}
				if kind == "fallback error" {
					runner = &doctorErrorRunner{agentFake: f, message: "remote failure " + token}
				}
				doctor = func(context.Context, string, string, string, string) ([]api.Check, error) { return nil, ErrUnreachable }
			}
			s := &Session{R: runner, F: &Facts{Curl: true}, Opts: Options{Project: "shop", Env: "p5", ListenIP: net.ParseIP("127.0.0.1"), ListenPort: 7710, Tokens: store, Getenv: func(string) string { return "" }, Doctor: doctor}, TokenEnv: e.TokenEnv, cliEntry: e, cliConfigKnown: true}
			_, detail, err := verifyStep(context.Background(), s)
			if strings.Contains(detail, token) || err != nil && strings.Contains(err.Error(), token) {
				t.Fatal("doctor diagnostics exposed the stored token")
			}
			if (kind == "critical" || kind == "fallback critical") && (err == nil || !strings.Contains(err.Error(), "critical")) {
				t.Fatal("critical finding was lost")
			}
			if (kind == "http error" || kind == "fallback error") && err == nil {
				t.Fatal("HTTP error was lost")
			}
		})
	}
}
