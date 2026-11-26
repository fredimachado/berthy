package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/agentclient"
	"berthy/internal/deploy"
	"berthy/internal/gitcheck"
	"berthy/internal/manifest"
	"berthy/internal/version"
)

func TestF12NamedSecretDiagnostics(t *testing.T) {
	for _, tc := range []struct{ name, entered, token, diagnostic, want string }{
		{"duplicate raw and wire", "redact", "active-token", "redact", "[redacted]"},
		{"longer contains shorter", "redact", "prefix-redact-suffix", "prefix-redact-suffix", "[redacted]"},
		{"later longer overlaps", "xred", "redacted", "xredacted", "x[redacted]"},
		{"literal marker is input", "redact", "active-token", "[redacted]", "[[redacted]ed]"},
	} {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-json-%t", tc.name, asJSON), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == "GET" {
						fmt.Fprint(w, `[]`)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "registry": "registry.example", "services_error": tc.diagnostic})
				}))
				defer server.Close()
				fakeTerminal(t, tc.entered)
				cfg := ingressCLIConfig(t, server.URL)
				t.Setenv("BERTHY_TEST_TOKEN", tc.token)
				args := []string{"secret", "set", "PASS", "-e", "dev", "--config", cfg, "--manifest-dir", t.TempDir(), "--registry", "registry.example"}
				if asJSON {
					args = append(args, "--json")
				}
				var out, errs bytes.Buffer
				code := Run(args, &out, &errs)
				if code != 0 {
					t.Error("unexpected exit")
					return
				}
				if asJSON {
					var reply struct {
						ServicesError string `json:"services_error"`
					}
					if json.Unmarshal(out.Bytes(), &reply) != nil || reply.ServicesError != tc.want {
						t.Error("incorrect JSON redaction")
					}
				} else if !strings.Contains(out.String(), "services not updated: "+tc.want+"\n") {
					t.Error("incorrect text redaction")
				}
			})
		}
	}
}

func TestF12PromotionHeldRedactsOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
		fmt.Fprint(w, `{"approval_id":"ap1","expires":"2026-11-18T12:00:00Z","plan":{"action":"deploy","note":"source-token"}}`)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	for _, asJSON := range []bool{false, true} {
		var out, errs bytes.Buffer
		code := sendCommand(context.Background(), server.Client(), base, "/", "redact", "POST", "deploys", nil, commandFlags{json: asJSON, redactToken: "source-token"}, "deploy", &out, &errs)
		if code != exitHeld || !strings.Contains(out.String(), "[redacted]") || strings.Contains(out.String(), "[[redacted]") {
			t.Error("promotion held diagnostic rescanned a replacement")
		}
	}
}

func TestF12PromotionTransportRedactsOnce(t *testing.T) {
	t.Setenv("F12_SOURCE_TOKEN", "source-token")
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"projects":{"demo":{"environments":{"stage":{"url":"http://source.invalid","token_env":"F12_SOURCE_TOKEN"}}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("http://destination.invalid")
	target := &agentclient.Target{Base: base, Project: "demo", Env: "prod", Token: "redact"}
	c := &http.Client{Transport: progressRoundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("source-token") })}
	m := &manifest.Manifest{Apps: map[string]manifest.App{"api": {Kind: "image", Image: "example.com/api"}}}
	_, err := selectPromotion(context.Background(), c, target, cfg, "stage", m, "api")
	if err == nil || !strings.Contains(err.Error(), "[redacted]") || strings.Contains(err.Error(), "[[redacted]") {
		t.Error("promotion transport diagnostic rescanned a replacement")
	}
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte("project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: example.com/api\n    roles:\n      web: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, asJSON := range []bool{false, true} {
		var out, errs bytes.Buffer
		code := runPromote(context.Background(), c, target, cfg, "stage", commandFlags{dir: dir, env: "prod", pos: []string{"api"}, dry: true, json: asJSON}, &out, &errs)
		if code != 1 || !strings.Contains(errs.String(), "[redacted]") || strings.Contains(errs.String(), "[[redacted]") {
			t.Error("final promotion diagnostic rescanned a replacement")
		}
	}
}

func TestF12RedactorLeavesInputsUnchanged(t *testing.T) {
	values := []string{"redact", "prefix-redact-suffix", ""}
	before := append([]string(nil), values...)
	got := redactSecretText("prefix-redact-suffix", values...)
	if got != "[redacted]" || strings.Join(values, "|") != strings.Join(before, "|") {
		t.Error("redactor changed caller input or longest match")
	}
}

func TestF12KeptVersionDiagnosticRedactsOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"redact"}`)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	var errs bytes.Buffer
	code := checkKeptVersions(context.Background(), server.Client(), &agentclient.Target{Base: base, Token: "redact"}, []deployStep{{app: "api", req: deploy.Request{KeepVersion: true}}}, &errs, false)
	if code != 1 || errs.String() != "agent returned HTTP 403: [redacted] (nothing was deployed)\n" {
		t.Error("kept-version diagnostic rescanned a replacement")
	}
}

func TestF12BatchPreflightRedactsOnce(t *testing.T) {
	for _, stage := range []string{"policy", "guard"} {
		t.Run(stage, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stage == "guard" && r.Method == "GET" {
					fmt.Fprint(w, `{"project":"demo","environment":"dev","protected":false}`)
					return
				}
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"message":"redact"}`)
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			m := &manifest.Manifest{Apps: map[string]manifest.App{"api": {Kind: "image", Image: "example.com/api"}}}
			var out, errs bytes.Buffer
			handled, code := prepareBatchTags(context.Background(), server.Client(), &agentclient.Target{Base: base, Prefix: "/", Project: "demo", Env: "dev", Token: "redact"}, commandFlags{}, m, gitcheck.Checkout{}, []deployStep{{app: "api", req: deploy.Request{App: "api"}}}, map[string]string{"api": "stable"}, &out, &errs)
			if !handled || code != 1 || !strings.Contains(errs.String(), "[redacted]") || strings.Contains(errs.String(), "[[redacted]") {
				t.Error("batch preflight rescanned a replacement")
			}
		})
	}
}

func TestF12SingleTagPolicyRedactsOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"redact"}`)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	t.Setenv("BERTHY_TEST_TOKEN", "redact")
	dir := writeAliasDir(t, versionsManifest)
	var out, errs bytes.Buffer
	code := Run([]string{"deploy", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", "stable", "--dry-run", "--json"}, &out, &errs)
	if code != 1 || errs.String() != "agent returned HTTP 403: [redacted]\n" {
		t.Error("single tag policy rescanned a replacement")
	}
}

func TestF12VersionPreflightRedactsOnce(t *testing.T) {
	old := version.Version
	version.Version = "v0.8.0"
	t.Cleanup(func() { version.Version = old })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"version":"v0.6.0-redact"}`) }))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	t.Setenv("BERTHY_TEST_TOKEN", "redact")
	dir := writeAliasDir(t, "min_version: 0.7.0\n"+versionsManifest)
	var out, errs bytes.Buffer
	code := Run([]string{"versions", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--json"}, &out, &errs)
	if code != 1 || errs.String() != "this manifest needs Berthy 0.7.0 or newer; the agent of dev runs v0.6.0-[redacted]: upgrade it with berthy setup\n" {
		t.Error("version preflight rescanned a replacement")
	}
}
