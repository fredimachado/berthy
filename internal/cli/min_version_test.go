package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// minVersionManifest is an image app that needs Berthy 0.9.0.
const minVersionManifest = `min_version: 0.9.0
project: demo
network: demo
apps:
  web:
    kind: image
    image: ghcr.io/acme/web
    roles:
      web: {}
`

// minVersionAgent answers the agent endpoint with agentVersion and every POST with postStatus and postBody, and counts the POSTs.
type minVersionAgent struct {
	agentVersion string
	postStatus   int
	postBody     string
	mu           sync.Mutex
	posts        int
}

func (a *minVersionAgent) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/agent"):
		fmt.Fprintf(w, `{"project":"demo","environment":"dev","hostname":"lab","protected":false,"effective_protected":false,"version":%q}`, a.agentVersion)
	case r.Method == http.MethodPost:
		a.mu.Lock()
		a.posts++
		a.mu.Unlock()
		w.WriteHeader(a.postStatus)
		fmt.Fprint(w, a.postBody)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (a *minVersionAgent) postCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.posts
}

// minVersionFixture starts the fake agent, and writes the manifest and a configuration that points at it. It returns the
// manifest directory, the configuration file, and the agent.
func minVersionFixture(t *testing.T, manifestYAML string, a *minVersionAgent) (string, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(manifestYAML), 0600); err != nil {
		t.Fatal(err)
	}
	return dir, ingressCLIConfig(t, server.URL)
}

func TestDeployRefusesAManifestNewerThanTheCLI(t *testing.T) {
	stampCLI(t, "v0.8.2")
	a := &minVersionAgent{agentVersion: "v0.8.2", postStatus: http.StatusOK, postBody: `{}`}
	dir, cfg := minVersionFixture(t, minVersionManifest, a)
	var out, errout bytes.Buffer
	code := Run([]string{"deploy", "web", "--version", "v1", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	want := "this manifest needs Berthy 0.9.0 or newer; this CLI is v0.8.2"
	if code == 0 || !strings.Contains(errout.String(), want) || a.postCount() != 0 {
		t.Fatalf("exit %d, %d posts, stderr %q", code, a.postCount(), errout.String())
	}
}

func TestConfigValidateRefusesAManifestNewerThanTheCLI(t *testing.T) {
	stampCLI(t, "v0.8.2")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(minVersionManifest), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	code := Run([]string{"config", "validate", "-e", "dev", "--manifest-dir", dir}, &out, &errout)
	if code == 0 || !strings.Contains(errout.String(), "this manifest needs Berthy 0.9.0 or newer; this CLI is v0.8.2") {
		t.Fatalf("exit %d, stderr %q", code, errout.String())
	}
}

func TestDeployRefusesAnAgentOlderThanTheManifestNeeds(t *testing.T) {
	stampCLI(t, "v0.9.0")
	a := &minVersionAgent{agentVersion: "v0.8.2", postStatus: http.StatusOK, postBody: `{}`}
	dir, cfg := minVersionFixture(t, minVersionManifest, a)
	var out, errout bytes.Buffer
	code := Run([]string{"deploy", "web", "--version", "v1", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	want := "this manifest needs Berthy 0.9.0 or newer; the agent of dev runs v0.8.2: upgrade it with berthy setup"
	if code == 0 || !strings.Contains(errout.String(), want) || a.postCount() != 0 {
		t.Fatalf("exit %d, %d posts, stderr %q", code, a.postCount(), errout.String())
	}
}

func TestDeployPassesAnAgentThatMeetsTheManifest(t *testing.T) {
	stampCLI(t, "v0.9.0")
	a := &minVersionAgent{agentVersion: "v0.9.0", postStatus: http.StatusBadRequest, postBody: `{"code":"operation_failed","message":"stopped by the test"}`}
	dir, cfg := minVersionFixture(t, minVersionManifest, a)
	var out, errout bytes.Buffer
	Run([]string{"deploy", "web", "--version", "v1", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if a.postCount() != 1 || strings.Contains(errout.String(), "needs Berthy") {
		t.Fatalf("%d posts, stderr %q", a.postCount(), errout.String())
	}
}

func TestIngressApplyRefusesAnAgentOlderThanTheManifestNeeds(t *testing.T) {
	stampCLI(t, "v0.9.0")
	a := &minVersionAgent{agentVersion: "v0.8.2", postStatus: http.StatusOK, postBody: `{}`}
	dir, cfg := minVersionFixture(t, minVersionManifest, a)
	var out, errout bytes.Buffer
	code := Run([]string{"ingress", "apply", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if code == 0 || !strings.Contains(errout.String(), "the agent of dev runs v0.8.2") || a.postCount() != 0 {
		t.Fatalf("exit %d, %d posts, stderr %q", code, a.postCount(), errout.String())
	}
}

func TestDeployAllRefusesAnAgentOlderThanTheManifestNeeds(t *testing.T) {
	stampCLI(t, "v0.9.0")
	a := &minVersionAgent{agentVersion: "v0.8.2", postStatus: http.StatusOK, postBody: `{}`}
	dir, cfg := minVersionFixture(t, minVersionManifest, a)
	var out, errout bytes.Buffer
	code := Run([]string{"deploy", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", "web=v1"}, &out, &errout)
	if code == 0 || !strings.Contains(errout.String(), "the agent of dev runs v0.8.2") || a.postCount() != 0 {
		t.Fatalf("exit %d, %d posts, stderr %q", code, a.postCount(), errout.String())
	}
}

const olderAgentHint = "the agent runs an older release than this CLI; upgrade it with berthy setup"

func TestDeployHintsAnOlderAgentThatRefusesTheRequestAsInvalid(t *testing.T) {
	stampCLI(t, "v0.9.0")
	a := &minVersionAgent{agentVersion: "v0.8.0", postStatus: http.StatusBadRequest, postBody: `{"code":"bad_request","message":"invalid request"}`}
	dir, cfg := minVersionFixture(t, "project: demo\nnetwork: demo\napps:\n  web:\n    kind: image\n    image: ghcr.io/acme/web\n    roles:\n      web: {}\n", a)
	var out, errout bytes.Buffer
	code := Run([]string{"deploy", "web", "--version", "v1", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	want := "agent returned HTTP 400: invalid request\n" + olderAgentHint + "\n"
	if code == 0 || !strings.HasSuffix(errout.String(), want) {
		t.Fatalf("exit %d, stderr %q", code, errout.String())
	}
}

func TestDeployGivesNoHintForAnAgentOfThisRelease(t *testing.T) {
	stampCLI(t, "v0.9.0")
	a := &minVersionAgent{agentVersion: "v0.9.0", postStatus: http.StatusBadRequest, postBody: `{"code":"bad_request","message":"invalid request"}`}
	dir, cfg := minVersionFixture(t, "project: demo\nnetwork: demo\napps:\n  web:\n    kind: image\n    image: ghcr.io/acme/web\n    roles:\n      web: {}\n", a)
	var out, errout bytes.Buffer
	Run([]string{"deploy", "web", "--version", "v1", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if strings.Contains(errout.String(), "older release") || !strings.Contains(errout.String(), "agent returned HTTP 400: invalid request") {
		t.Fatalf("stderr %q", errout.String())
	}
}
