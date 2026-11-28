package cli

import (
	"berthy/internal/dialtest"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func checkConnectionResult(t *testing.T, code int, out, errs *bytes.Buffer, jsonMode bool, authority, reason string) {
	t.Helper()
	var message string
	if jsonMode {
		var got struct {
			Code   string `json:"code"`
			Reason string `json:"reason"`
		}
		err := json.Unmarshal(out.Bytes(), &got)
		if err != nil || got.Code != "agent_connection_failed" || errs.Len() != 0 {
			t.Errorf("exit=%d stdout=%q stderr=%q decode=%v", code, out.String(), errs.String(), err)
		}
		message = got.Reason
	} else {
		if out.Len() != 0 {
			t.Errorf("stdout=%q", out.String())
		}
		message = strings.TrimSuffix(errs.String(), "\n")
	}
	if code != 1 || !strings.HasPrefix(message, "agent connection failed at "+authority+": ") || !strings.Contains(message, reason) {
		t.Errorf("exit=%d diagnostic=%q want authority %q reason %q", code, message, authority, reason)
	}
}

func TestRemoteCommandsReportAgentConnectionFailure(t *testing.T) {
	for _, args := range [][]string{{"approvals"}, {"approve", "a1"}, {"reject", "a1"}, {"backup", "list"}, {"backup", "run", "daily"}, {"job", "logs", "task"}, {"job", "run", "task"}, {"job", "cancel", "task"}, {"state-backup"}, {"token", "list"}} {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%v", strings.Join(args, "-"), asJSON), func(t *testing.T) {
				address := dialtest.HangUp(t)
				u, _ := url.Parse(address)
				cfg := ingressCLIConfig(t, address)
				argv := append(append([]string{}, args...), "-e", "dev", "--config", cfg)
				if asJSON {
					argv = append(argv, "--json")
				}
				var out, errs bytes.Buffer
				code := Run(argv, &out, &errs)
				checkConnectionResult(t, code, &out, &errs, asJSON, u.Host, "EOF")
			})
		}
	}
}

func TestSendCommandRedactsConnectionFailure(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(fmt.Sprint(asJSON), func(t *testing.T) {
			base, _ := url.Parse("http://agent.invalid:7710")
			client := &http.Client{Transport: progressRoundTrip(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("failure active-token promotion-token entered-value")
			})}
			var out, errs bytes.Buffer
			code := sendCommand(context.Background(), client, base, "/", "active-token", "POST", "deploys", nil, commandFlags{json: asJSON, redactToken: "promotion-token"}, "deploy", &out, &errs, "entered-value")
			checkConnectionResult(t, code, &out, &errs, asJSON, base.Host, "failure [redacted] [redacted] [redacted]")
			for _, secret := range []string{"active-token", "promotion-token", "entered-value"} {
				if strings.Contains(out.String()+errs.String(), secret) {
					t.Error("credential leaked")
				}
			}
		})
	}
}

func TestSecretImportRedactsConnectionFailure(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(fmt.Sprint(asJSON), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				conn, buf, err := http.NewResponseController(w).Hijack()
				if err != nil {
					return
				}
				defer conn.Close()
				_, _ = fmt.Fprint(buf, "HTTP/1.1 value-alpha-SEP-value-beta invalid\r\n\r\n")
				_ = buf.Flush()
			}))
			defer server.Close()
			cfg := ingressCLIConfig(t, server.URL)
			withStdin(t, "FIRST=value-alpha\nSECOND=value-beta\n")
			argv := []string{"secret", "import", "-e", "dev", "--config", cfg}
			if asJSON {
				argv = append(argv, "--json")
			}
			var out, errs bytes.Buffer
			code := Run(argv, &out, &errs)
			u, _ := url.Parse(server.URL)
			checkConnectionResult(t, code, &out, &errs, asJSON, u.Host, "[redacted]-SEP-[redacted]")
			for _, secret := range []string{"value-alpha", "value-beta"} {
				if strings.Contains(out.String()+errs.String(), secret) {
					t.Error("import value leaked")
				}
			}
		})
	}
}

func TestTaggedDeployReportsPolicyConnectionFailure(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprint(batch), func(t *testing.T) {
			var mu sync.Mutex
			var requests []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests = append(requests, r.Method+" "+r.URL.Path)
				mu.Unlock()
				conn, _, err := http.NewResponseController(w).Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}))
			defer server.Close()
			cfg := ingressCLIConfig(t, server.URL)
			dir := writeAliasDir(t, versionsManifest)
			argv := []string{"deploy"}
			if !batch {
				argv = append(argv, "api")
			}
			ver := "stable"
			if batch {
				ver = "api=stable"
			}
			argv = append(argv, "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", ver, "--dry-run", "--json")
			var out, errs bytes.Buffer
			code := Run(argv, &out, &errs)
			u, _ := url.Parse(server.URL)
			checkConnectionResult(t, code, &out, &errs, true, u.Host, "EOF")
			mu.Lock()
			defer mu.Unlock()
			if len(requests) != 1 || requests[0] != "GET /v1/projects/demo/environments/dev/agent" {
				t.Errorf("requests=%v", requests)
			}
		})
	}
}
