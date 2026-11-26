package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"berthy/internal/agentclient"
)

func TestOlderAgentFallbackStylesOnlyFixedGuidance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/environments/dev/agent" {
			fmt.Fprint(w, `{"version":"v0.8.0"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"code":"not_found","message":"not found"}`)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	for _, command := range []string{"ps", "restart", "token"} {
		for _, jsonMode := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%v", command, jsonMode), func(t *testing.T) {
				var out, errs bytes.Buffer
				slice5Destination(t, &errs)
				args := []string{command}
				if command == "restart" {
					args = append(args, "api")
				} else if command == "token" {
					args = append(args, "list")
				}
				args = append(args, "-e", "dev", "--config", cfg)
				if jsonMode {
					args = append(args, "--json")
				}
				code := Run(args, &out, &errs)
				want := "agent returned HTTP 404: " + agentclient.OlderAgentMessage + "\n"
				if !jsonMode {
					want = "\x1b[31magent returned HTTP 404: \x1b[0m\x1b[36m" + agentclient.OlderAgentMessage + "\x1b[0m\n"
				}
				if code != 1 || out.Len() != 0 || errs.String() != want {
					t.Fatalf("exit %d stdout %q stderr %q want %q", code, out.String(), errs.String(), want)
				}
			})
		}
	}
	t.Run("redaction", func(t *testing.T) {
		t.Setenv("BERTHY_TEST_TOKEN", "release")
		for _, command := range []string{"ps", "restart", "token"} {
			var out, errs bytes.Buffer
			args := []string{command}
			if command == "restart" {
				args = append(args, "api")
			} else if command == "token" {
				args = append(args, "list")
			}
			args = append(args, "-e", "dev", "--config", cfg)
			code := Run(args, &out, &errs)
			want := "agent returned HTTP 404: the agent runs an older [redacted] than this CLI; upgrade it with berthy setup\n"
			if code != 1 || out.Len() != 0 || errs.String() != want {
				t.Fatalf("%s: %d %q %q", command, code, out.String(), errs.String())
			}
		}
	})
	var errs bytes.Buffer
	slice5Destination(t, &errs)
	arbitrary := "agent returned HTTP 404: " + agentclient.OlderAgentMessage + " extra"
	printRefusal(&errs, arbitrary, true)
	if want := "\x1b[31m" + arbitrary + "\x1b[0m\n"; errs.String() != want {
		t.Fatalf("arbitrary: %q", errs.String())
	}
	if strings.Contains(errs.String(), "\x1b[36m") {
		t.Fatal("arbitrary response styled as guidance")
	}
}
