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

const applyBerthyYML = `project: demo
network: demo
peers:
  prod: https://prod.example.com:443
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    roles:
      web:
        port: 8080
`

func applyAgent(t *testing.T, answer string) (url string, path, body *string) {
	t.Helper()
	var gotPath, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("bad request %s", r.Method)
		}
		gotPath, gotBody = r.URL.Path, string(b)
		fmt.Fprint(w, answer)
	}))
	t.Cleanup(server.Close)
	return server.URL, &gotPath, &gotBody
}

func TestApplySendsTheManifestAndPrintsTheChanges(t *testing.T) {
	url, path, body := applyAgent(t, `{"changes":["probes: + site","alerts: changed"]}`)
	cfg := ingressCLIConfig(t, url)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(applyBerthyYML), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if code := Run([]string{"apply", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--dry-run", "--commit", "abc123"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	if *path != "/v1/environments/dev/apply" {
		t.Fatalf("path %s", *path)
	}
	for _, want := range []string{`"dry_run":true`, `"commit":"abc123"`, `"project":"demo"`, `"prod":"https://prod.example.com:443"`} {
		if !strings.Contains(*body, want) {
			t.Fatalf("body lacks %s: %s", want, *body)
		}
	}
	if want := "would change the environment settings:\n  probes: + site\n  alerts: changed\n"; out.String() != want {
		t.Fatalf("dry run output %q", out.String())
	}
	out.Reset()
	if code := Run([]string{"apply", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	if strings.Contains(*body, "dry_run") || !strings.HasPrefix(out.String(), "changed the environment settings:") {
		t.Fatalf("body %s output %q", *body, out.String())
	}
	out.Reset()
	if code := Run([]string{"apply", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--json"}, &out, &errs); code != 0 || !strings.Contains(out.String(), `"changes"`) {
		t.Fatalf("json output %q: %s", out.String(), errs.String())
	}
}

func TestApplyWithNothingToChangePrintsNoChanges(t *testing.T) {
	url, _, _ := applyAgent(t, `{"changes":[]}`)
	cfg := ingressCLIConfig(t, url)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(applyBerthyYML), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if code := Run([]string{"apply", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errs); code != 0 || out.String() != "no changes\n" {
		t.Fatalf("exit %d output %q: %s", code, out.String(), errs.String())
	}
}

func TestApplyTakesNoApp(t *testing.T) {
	cfg := ingressCLIConfig(t, "http://127.0.0.1:1")
	var out, errs bytes.Buffer
	if code := Run([]string{"apply", "api", "-e", "dev", "--config", cfg}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "usage: apply") {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
}
