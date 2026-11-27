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

	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/credstore/credtest"
)

type doctorErrorRunner struct {
	*agentFake
	message string
}

func (f *doctorErrorRunner) Run(ctx context.Context, script string, stdin io.Reader) (Result, error) {
	if strings.Contains(script, "agent setup-probe doctor") {
		return Result{}, errors.New(f.message)
	}
	return f.agentFake.Run(ctx, script, stdin)
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
			s := &Session{R: runner, Opts: Options{Project: "shop", Env: "p5", ListenIP: net.ParseIP("127.0.0.1"), ListenPort: 7710, Tokens: store, Getenv: func(string) string { return "" }, Doctor: doctor}, TokenEnv: e.TokenEnv, cliEntry: e, cliConfigKnown: true}
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
