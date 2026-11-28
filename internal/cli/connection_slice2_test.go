package cli

import (
	"berthy/internal/agentclient"
	"berthy/internal/dialtest"
	"berthy/internal/manifest"
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
	if code != 1 || message != "agent connection failed at "+authority+": "+reason {
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
				checkConnectionResult(t, code, &out, &errs, asJSON, u.Host, "connection failed")
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
			checkConnectionResult(t, code, &out, &errs, asJSON, base.Host, "connection failed")
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
				_, _ = fmt.Fprint(buf, "HTTP/1.1 value-alpha\\path-SEP-value-beta invalid\r\n\r\n")
				_ = buf.Flush()
			}))
			defer server.Close()
			cfg := ingressCLIConfig(t, server.URL)
			withStdin(t, "FIRST=value-alpha\\path\nSECOND=value-beta\n")
			argv := []string{"secret", "import", "-e", "dev", "--config", cfg}
			if asJSON {
				argv = append(argv, "--json")
			}
			var out, errs bytes.Buffer
			code := Run(argv, &out, &errs)
			u, _ := url.Parse(server.URL)
			checkConnectionResult(t, code, &out, &errs, asJSON, u.Host, "connection failed")
			if asJSON {
				var decoded struct {
					Reason string `json:"reason"`
				}
				if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
					t.Fatal(err)
				}
				for _, secret := range []string{`value-alpha\path`, `value-alpha\\path`, "value-beta"} {
					if strings.Contains(decoded.Reason, secret) {
						t.Errorf("decoded reason exposes %q", secret)
					}
				}
			}
			for _, secret := range []string{`value-alpha\path`, `value-alpha\\path`, "value-beta"} {
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
			checkConnectionResult(t, code, &out, &errs, true, u.Host, "connection failed")
			mu.Lock()
			defer mu.Unlock()
			if len(requests) != 1 || requests[0] != "GET /v1/projects/demo/environments/dev/agent" {
				t.Errorf("requests=%v", requests)
			}
		})
	}
}

func TestTaggedDeployInvalidPolicyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) }))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := writeAliasDir(t, versionsManifest)
	for _, args := range [][]string{{"deploy", "api", "--version", "stable"}, {"deploy", "--version", "api=stable"}} {
		argv := append(append([]string{}, args...), "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--dry-run", "--json")
		var out, errs bytes.Buffer
		if code := Run(argv, &out, &errs); code != 1 || out.Len() != 0 || errs.String() != "invalid agent response\n" {
			t.Errorf("exit=%d stdout=%q stderr=%q", code, out.String(), errs.String())
		}
	}
}

func TestGuideDoesNotExposeEarlierSecretOnConnectionFailure(t *testing.T) {
	dir := secretManifestDir(t, "project: demo\nnetwork: overlay\napps:\n  api:\n    kind: image\n    image: example.com/api\n    env:\n      secret: [FIRST, SECOND]\n    roles:\n      web: {}\n")
	m, err := manifest.Load(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("http://agent.invalid:7710")
	posts, reads := 0, 0
	client := &http.Client{Transport: progressRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("[]"))}, nil
		}
		posts++
		if posts == 1 {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
		}
		return nil, errors.New(`reflect first\secret`)
	})}
	var out, errs bytes.Buffer
	_, result := guideSecrets(context.Background(), client, &agentclient.Target{Base: base, Prefix: "/", Token: "fixture-token"}, m, dir, "dev", func() ([]byte, error) {
		reads++
		if reads == 1 {
			return []byte(`first\secret`), nil
		}
		return []byte("second-value"), nil
	}, &out, &errs)
	if result != guideFailed || posts != 2 || reads != 2 {
		t.Fatalf("result=%v posts=%d reads=%d stdout=%q stderr=%q", result, posts, reads, out.String(), errs.String())
	}
	if !strings.HasSuffix(errs.String(), "agent connection failed at agent.invalid:7710: connection failed\n") {
		t.Errorf("stderr=%q", errs.String())
	}
	for _, text := range []string{`first\secret`, `first\\secret`, "reflect"} {
		if strings.Contains(out.String()+errs.String(), text) {
			t.Errorf("output exposes %q", text)
		}
	}
}
