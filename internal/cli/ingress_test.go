package cli

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ingressBerthyYML is a minimal manifest with managed ingress and one hostname.
const ingressBerthyYML = `project: demo
network: demo
ingress:
  cloudflare:
    account_id: acct
    api_token_secret: CF_TOKEN
    image: cloudflare/cloudflared:2026.11.0
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    roles:
      web:
        port: 8080
        hostnames:
          - api.example.com
`

// ingressCLIConfig writes a CLI config that points dev at url, and sets the token it names.
func ingressCLIConfig(t *testing.T, url string) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}`, url)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	return cfg
}

func TestIngressApplySendsManifestAndReplaceFlag(t *testing.T) {
	var got string
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("bad request %s", r.Method)
		}
		path, got = r.URL.Path, string(body)
		fmt.Fprint(w, `{"create_tunnel":true}`)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(ingressBerthyYML), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if code := Run([]string{"ingress", "apply", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--dry-run", "--replace-existing"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	if path != "/v1/environments/dev/ingress/apply" {
		t.Fatalf("path %s", path)
	}
	for _, want := range []string{`"replace_existing":true`, `"dry_run":true`, `"project":"demo"`, `"hostnames":["api.example.com"]`} {
		if !strings.Contains(got, want) {
			t.Fatalf("body lacks %s: %s", want, got)
		}
	}
	if code := Run([]string{"ingress", "apply", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--dry-run"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	if strings.Contains(got, "replace_existing") {
		t.Fatalf("flag sent without --replace-existing: %s", got)
	}
}

func TestIngressStatusGetsStatusEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/environments/dev/ingress/status" {
			t.Errorf("bad request %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"configured":false,"routes":[]}`)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errs bytes.Buffer
	if code := Run([]string{"ingress", "status", "-e", "dev", "--config", cfg}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
}

func TestIngressRejectsUnknownSubcommand(t *testing.T) {
	cfg := ingressCLIConfig(t, "http://127.0.0.1:9")
	for _, args := range [][]string{{"ingress", "bogus"}, {"ingress"}, {"ingress", "apply", "status"}} {
		var out, errs bytes.Buffer
		full := append(append([]string{}, args...), "-e", "dev", "--config", cfg)
		if code := Run(full, &out, &errs); code != 2 {
			t.Fatalf("%v: exit %d: %s", args, code, errs.String())
		}
	}
}
