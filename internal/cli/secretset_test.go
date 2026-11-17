package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"berthy/internal/manifest"
)

// fakeTerminal makes standard input a terminal that answers line for the rest of the test.
func fakeTerminal(t *testing.T, line string) {
	t.Helper()
	old := hiddenInput
	hiddenInput = func() (func() ([]byte, error), bool) {
		return func() ([]byte, error) { return []byte(line), nil }, true
	}
	t.Cleanup(func() { hiddenInput = old })
}

// secretManifestDir returns a directory whose berthy.yml is manifestText.
func secretManifestDir(t *testing.T, manifestText string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(manifestText), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// recordingAgent answers every request with a secret plan and records the path and body of each one.
func recordingAgent(bodies *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, r.URL.Path+" "+string(data))
		fmt.Fprint(w, `{"action":"secret set","name":"CF_TOKEN","exists":false,"registry":""}`)
	}))
}

// cloudflareManifest is a manifest with managed ingress whose API token is the secret CF_TOKEN.
func cloudflareManifest(dns *bool) *manifest.Manifest {
	return &manifest.Manifest{Project: "demo", Ingress: &manifest.Ingress{Cloudflare: &manifest.Cloudflare{APITokenSecret: "CF_TOKEN", DNS: dns}}}
}

// guideText joins the lines of a guide's intro.
func guideText(g secretGuide) string { return strings.Join(g.intro, "\n") }

// urlLine returns the link in text, without the indent the guide puts before it.
func urlLine(t *testing.T, text string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "https://") {
			return line
		}
	}
	t.Fatalf("no link in %q", text)
	return ""
}

// parseLink parses a link the guide printed.
func parseLink(t *testing.T, link string) *url.URL {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// permissionGroup is one entry of the permissionGroupKeys a Cloudflare token link asks for.
type permissionGroup struct {
	Key  string `json:"key"`
	Type string `json:"type"`
}

// TestGuideForGHCRLinksAClassicTokenThatOnlyReadsPackages checks the guide for the ghcr.io password.
func TestGuideForGHCRLinksAClassicTokenThatOnlyReadsPackages(t *testing.T) {
	g := guideFor(&manifest.Manifest{Project: "demo"}, "prod", "GHCR_TOKEN", "ghcr.io")
	if len(g.intro) == 0 || !strings.Contains(g.intro[0], "GHCR_TOKEN") || !strings.Contains(g.intro[0], "ghcr.io") {
		t.Fatalf("first line %q does not name the secret and ghcr.io", g.intro)
	}
	u := parseLink(t, urlLine(t, guideText(g)))
	if u.Host != "github.com" || u.Path != "/settings/tokens/new" || u.Query().Get("scopes") != "read:packages" || u.Query().Get("description") != "berthy-demo-prod" {
		t.Fatalf("link %s", u)
	}

	g = guideFor(nil, "prod", "GHCR_TOKEN", "ghcr.io")
	if got := parseLink(t, urlLine(t, guideText(g))).Query().Get("description"); got != "berthy-prod" {
		t.Fatalf("without a manifest the description is %q, want berthy-prod", got)
	}
}

// TestGuideForCloudflareTokenLinksTheTemplateWithBothPermissions checks the guide for the managed ingress API token.
func TestGuideForCloudflareTokenLinksTheTemplateWithBothPermissions(t *testing.T) {
	on := true
	g := guideFor(cloudflareManifest(&on), "prod", "CF_TOKEN", "")
	link := urlLine(t, guideText(g))
	if !strings.HasPrefix(link, "https://dash.cloudflare.com/?to=/:account/api-tokens&permissionGroupKeys=") {
		t.Fatalf("link %s is not Cloudflare's account token template", link)
	}
	want := []permissionGroup{{"c07321b023e944ff818fec44d8203567", "edit"}, {"dns", "edit"}}
	if got := permissionGroups(t, link); !slices.Equal(got, want) {
		t.Fatalf("permission groups %+v, want %+v", got, want)
	}
	if name := parseLink(t, link).Query().Get("name"); name != "berthy-demo-prod" {
		t.Fatalf("token name %q, want berthy-demo-prod", name)
	}
	if !strings.Contains(guideText(g), "Limit it to the zones") {
		t.Fatalf("guide %q does not say to limit the zones", guideText(g))
	}

	off := false
	g = guideFor(cloudflareManifest(&off), "prod", "CF_TOKEN", "")
	link = urlLine(t, guideText(g))
	if want := []permissionGroup{{"c07321b023e944ff818fec44d8203567", "edit"}}; !slices.Equal(permissionGroups(t, link), want) {
		t.Fatalf("with dns off the permission groups are %+v, want %+v", permissionGroups(t, link), want)
	}
	if strings.Contains(guideText(g), "Limit it to the zones") {
		t.Fatalf("guide %q says to limit the zones although DNS is off", guideText(g))
	}
}

// permissionGroups decodes the permissionGroupKeys of a Cloudflare token link.
func permissionGroups(t *testing.T, link string) []permissionGroup {
	t.Helper()
	var groups []permissionGroup
	if err := json.Unmarshal([]byte(parseLink(t, link).Query().Get("permissionGroupKeys")), &groups); err != nil {
		t.Fatalf("permissionGroupKeys is not JSON: %v", err)
	}
	return groups
}

// TestGuideForOtherSecrets checks that secrets Berthy does not know get no link.
func TestGuideForOtherSecrets(t *testing.T) {
	on := true
	m := cloudflareManifest(&on)

	other := guideFor(nil, "prod", "PRIVATE_TOKEN", "registry.example.com")
	if len(other.intro) != 1 || !strings.Contains(other.intro[0], "registry.example.com") || strings.Contains(guideText(other), "https://") {
		t.Fatalf("other registry gives %q, want one line naming the host and no link", other.intro)
	}
	for _, tc := range []struct {
		name string
		m    *manifest.Manifest
	}{
		{"DB_URL", nil},
		{"DB_URL", m},
	} {
		if g := guideFor(tc.m, "prod", tc.name, ""); len(g.intro) != 0 {
			t.Fatalf("%s gives %q, want no intro", tc.name, g.intro)
		}
	}
	ghcr := guideText(guideFor(m, "prod", "CF_TOKEN", "ghcr.io"))
	if !strings.Contains(ghcr, "https://github.com/settings/tokens/new") || strings.Contains(ghcr, "dash.cloudflare.com") {
		t.Fatalf("the Cloudflare secret with --registry ghcr.io gives %q, want the GHCR guide", ghcr)
	}
}

// TestSecretSetAtATerminalAsksHiddenAndSendsTheValue checks the terminal path: the guide, the hidden prompt and the value sent.
func TestSecretSetAtATerminalAsksHiddenAndSendsTheValue(t *testing.T) {
	fakeTerminal(t, "s3cret-value")
	var bodies []string
	server := recordingAgent(&bodies)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	args := []string{"secret", "set", "CF_TOKEN", "-e", "dev", "--config", cfg, "--manifest-dir", secretManifestDir(t, ingressBerthyYML)}
	if code := Run(args, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if len(bodies) != 1 || !strings.HasSuffix(strings.Fields(bodies[0])[0], "/v1/environments/dev/secrets") || !strings.Contains(bodies[0], `"value":"s3cret-value"`) {
		t.Fatalf("requests %q, want one secret set with the value", bodies)
	}
	if !strings.Contains(errout.String(), "Value of CF_TOKEN (hidden): ") || !strings.Contains(errout.String(), "https://dash.cloudflare.com/?to=/:account/api-tokens") {
		t.Fatalf("stderr %q lacks the prompt or the Cloudflare link", errout.String())
	}
	if strings.Contains(out.String()+errout.String(), "s3cret-value") {
		t.Fatalf("the value was printed: stdout %q stderr %q", out.String(), errout.String())
	}
}

// TestSecretSetAtATerminalRefusesAnEmptyValue checks that an empty line is refused before anything is sent.
func TestSecretSetAtATerminalRefusesAnEmptyValue(t *testing.T) {
	fakeTerminal(t, "")
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	args := []string{"secret", "set", "CF_TOKEN", "-e", "dev", "--config", cfg, "--manifest-dir", secretManifestDir(t, ingressBerthyYML)}
	if code := Run(args, &out, &errout); code != 1 || !strings.Contains(errout.String(), "no value entered") || calls.Load() != 0 {
		t.Fatalf("exit %d stderr %q (agent calls %d), want exit 1 with %q and no call", code, errout.String(), calls.Load(), "no value entered")
	}
}

// TestSecretSetAtATerminalWithoutAManifestStillAsks checks that a missing or broken manifest does not stop the prompt.
func TestSecretSetAtATerminalWithoutAManifestStillAsks(t *testing.T) {
	fakeTerminal(t, "s3cret-value")
	var bodies []string
	server := recordingAgent(&bodies)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)

	var out, errout bytes.Buffer
	args := []string{"secret", "set", "DB_URL", "-e", "dev", "--config", cfg, "--manifest-dir", t.TempDir()}
	if code := Run(args, &out, &errout); code != 0 {
		t.Fatalf("empty directory: exit %d: %s", code, errout.String())
	}
	if !strings.Contains(errout.String(), "Value of DB_URL (hidden): ") || strings.Contains(errout.String(), "manifest not loaded") {
		t.Fatalf("empty directory: stderr %q, want the prompt and no manifest error", errout.String())
	}

	out.Reset()
	errout.Reset()
	broken := secretManifestDir(t, "project: demo\nnetwork: demo\n")
	args = []string{"secret", "set", "DB_URL", "-e", "dev", "--config", cfg, "--manifest-dir", broken}
	if code := Run(args, &out, &errout); code != 0 {
		t.Fatalf("broken manifest: exit %d: %s", code, errout.String())
	}
	if !strings.Contains(errout.String(), "Value of DB_URL (hidden): ") || !strings.Contains(errout.String(), "manifest not loaded: ") {
		t.Fatalf("broken manifest: stderr %q, want the prompt and %q", errout.String(), "manifest not loaded: ")
	}
}

// TestSecretSetFromAPipeIsUnchanged checks that a value piped in is stored as before, with no prompt and no guide.
func TestSecretSetFromAPipeIsUnchanged(t *testing.T) {
	var bodies []string
	server := recordingAgent(&bodies)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	withStdin(t, "postgres://db\n")
	var out, errout bytes.Buffer
	if code := Run([]string{"secret", "set", "DB_URL", "-e", "dev", "--config", cfg}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"value":"postgres://db"`) {
		t.Fatalf("requests %q, want the piped value", bodies)
	}
	if errout.Len() != 0 {
		t.Fatalf("stderr %q, want nothing from a pipe", errout.String())
	}
}
