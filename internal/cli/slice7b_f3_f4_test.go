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

func TestExactLegacyMessageRequiresSuccessfulProbe(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"message":%q}`, agentclient.OlderAgentMessage)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	for _, cmd := range []string{"ps", "restart", "token"} {
		for _, jsonMode := range []bool{false, true} {
			for _, terminal := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/json=%v/terminal=%v", cmd, jsonMode, terminal), func(t *testing.T) {
					calls = nil
					var out, errs bytes.Buffer
					if terminal {
						slice5Destination(t, &errs)
					}
					args := []string{cmd}
					if cmd == "restart" {
						args = append(args, "api")
					} else if cmd == "token" {
						args = append(args, "list")
					}
					args = append(args, "-e", "dev", "--config", cfg)
					if jsonMode {
						args = append(args, "--json")
					}
					code := Run(args, &out, &errs)
					msg := "agent returned HTTP 404: " + agentclient.OlderAgentMessage
					if terminal && !jsonMode {
						msg = "\x1b[31m" + msg + "\x1b[0m"
					}
					if code != 1 || out.Len() != 0 || errs.String() != msg+"\n" {
						t.Fatalf("exit=%d stdout=%q stderr=%q want=%q", code, out.String(), errs.String(), msg+"\n")
					}
					if len(calls) != 2 || calls[1] != "GET /v1/environments/dev/agent" {
						t.Fatalf("calls %q", calls)
					}
				})
			}
		}
	}
}

func TestDoctorNoteStylesOnlyLead(t *testing.T) {
	stampCLI(t, "v1.0.0")
	server := doctorVersionServer(t, "v0.9.0", false)
	cfg := ingressCLIConfig(t, server.URL)
	for _, jsonMode := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%v", jsonMode), func(t *testing.T) {
			var out, errs bytes.Buffer
			if jsonMode {
				slice5Destination(t, &errs)
			} else {
				slice5Destination(t, &out)
			}
			args := []string{"doctor", "-e", "dev", "--config", cfg}
			if jsonMode {
				args = append(args, "--json")
			}
			code := Run(args, &out, &errs)
			sentence := " the agent runs v0.9.0, this CLI is v1.0.0; berthy setup upgrades the agent\n"
			if code != 0 {
				t.Fatalf("exit %d: %q", code, errs.String())
			}
			if jsonMode {
				if errs.String() != "note:"+sentence || strings.Contains(out.String(), "\x1b") {
					t.Fatalf("stdout %q stderr %q", out.String(), errs.String())
				}
			} else if !strings.Contains(out.String(), "\x1b[36mnote:\x1b[0m"+sentence) || errs.Len() != 0 {
				t.Fatalf("stdout %q stderr %q", out.String(), errs.String())
			}
		})
	}
}
