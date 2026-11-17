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

	"berthy/internal/cloudflare/cftest"
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
	fakeCloudflare(t, "s3cret-value", "example.com")
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

// fakeCloudflare starts an in-memory Cloudflare account that accepts token and holds zones, and points the CLI at it. It
// returns the account, so a test can break its endpoints, and a counter of the requests the account received.
func fakeCloudflare(t *testing.T, token string, zones ...string) (*cftest.Fake, *atomic.Int32) {
	t.Helper()
	fake := cftest.New(token, "acct")
	for _, z := range zones {
		fake.AddZone(z)
	}
	handler := fake.Handler()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	t.Setenv("BERTHY_CLOUDFLARE_API_URL", server.URL+"/client/v4")
	return fake, &requests
}

// fakeRegistry starts a registry that lets user u with password right pull acme/api and refuses everyone else. It returns
// the registry's host as a manifest names it, and a counter of the requests the registry received.
func fakeRegistry(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/v2/acme/api/tags/list" {
			http.NotFound(w, r)
			return
		}
		if user, pass, ok := r.BasicAuth(); !ok || user != "u" || pass != "right" {
			w.Header().Set("WWW-Authenticate", `Basic realm="t"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://"), &requests
}

// registryManifest is a manifest that names registry with the password secret REGISTRY_TOKEN and has one image app on image.
func registryManifest(registry, image string) string {
	return fmt.Sprintf("project: demo\nnetwork: demo\nregistries:\n  %q:\n    username: u\n    password_secret: REGISTRY_TOKEN\napps:\n  api:\n    kind: image\n    image: %s\n    roles:\n      web:\n        port: 8080\n", registry, image)
}

// ingressBerthyYMLWithoutDNS is ingressBerthyYML with DNS turned off, so the token needs no zone.
var ingressBerthyYMLWithoutDNS = strings.Replace(ingressBerthyYML, "    image: cloudflare/cloudflared:2026.11.0\n", "    image: cloudflare/cloudflared:2026.11.0\n    dns: false\n", 1)

// secretSetAtTerminal runs secret set name at a terminal that types value, with the manifest in dir and the agent at agentURL.
// It returns the exit code and what was printed to standard output and standard error.
func secretSetAtTerminal(t *testing.T, value, agentURL, dir, name string, extra ...string) (int, string, string) {
	t.Helper()
	fakeTerminal(t, value)
	cfg := ingressCLIConfig(t, agentURL)
	var out, errout bytes.Buffer
	args := append([]string{"secret", "set", name, "-e", "dev", "--config", cfg, "--manifest-dir", dir}, extra...)
	code := Run(args, &out, &errout)
	return code, out.String(), errout.String()
}

// assertNotPrinted fails when the typed value appears in what was printed.
func assertNotPrinted(t *testing.T, value, out, errout string) {
	t.Helper()
	if strings.Contains(out, value) || strings.Contains(errout, value) {
		t.Fatalf("the value was printed: stdout %q stderr %q", out, errout)
	}
}

// TestSecretSetStoresAGoodCloudflareTokenAfterCheckingIt checks that a token Cloudflare accepts, which sees the zone of
// the hostname, is stored and says what it checked.
func TestSecretSetStoresAGoodCloudflareTokenAfterCheckingIt(t *testing.T) {
	fakeCloudflare(t, "right-token", "example.com")
	var bodies []string
	server := recordingAgent(&bodies)
	defer server.Close()
	code, _, errout := secretSetAtTerminal(t, "right-token", server.URL, secretManifestDir(t, ingressBerthyYML), "CF_TOKEN")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errout)
	}
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"value":"right-token"`) {
		t.Fatalf("requests %q, want the value sent once", bodies)
	}
	for _, want := range []string{
		"checked: Cloudflare accepts the token, and it can read tunnels in account acct",
		"checked: the token sees the zone of api.example.com",
	} {
		if !strings.Contains(errout, want) {
			t.Errorf("stderr %q lacks %q", errout, want)
		}
	}
}

// TestSecretSetRefusesACloudflareTokenThatIsNotAccepted checks that a token Cloudflare rejects is not sent to the agent.
func TestSecretSetRefusesACloudflareTokenThatIsNotAccepted(t *testing.T) {
	fakeCloudflare(t, "right-token", "example.com")
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	code, out, errout := secretSetAtTerminal(t, "wrong-token-8c1d", server.URL, secretManifestDir(t, ingressBerthyYML), "CF_TOKEN")
	if code != 1 || !strings.Contains(errout, "not stored: Cloudflare does not accept this token") || calls.Load() != 0 {
		t.Fatalf("exit %d stderr %q (agent calls %d), want exit 1 with the refusal and no call", code, errout, calls.Load())
	}
	assertNotPrinted(t, "wrong-token-8c1d", out, errout)
}

// TestSecretSetRefusesACloudflareTokenThatSeesNoZoneOfAHostname checks that a token which sees no zone for a published
// hostname is not stored.
func TestSecretSetRefusesACloudflareTokenThatSeesNoZoneOfAHostname(t *testing.T) {
	fakeCloudflare(t, "right-token")
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	code, out, errout := secretSetAtTerminal(t, "right-token", server.URL, secretManifestDir(t, ingressBerthyYML), "CF_TOKEN")
	if code != 1 || !strings.Contains(errout, "not stored: the token sees no zone for api.example.com in account acct") || calls.Load() != 0 {
		t.Fatalf("exit %d stderr %q (agent calls %d), want exit 1 with the refusal and no call", code, errout, calls.Load())
	}
	assertNotPrinted(t, "right-token", out, errout)
}

// TestSecretSetWithDNSOffDoesNotNeedAZone checks that with DNS off in the manifest the zones are not asked about.
func TestSecretSetWithDNSOffDoesNotNeedAZone(t *testing.T) {
	fakeCloudflare(t, "right-token")
	var bodies []string
	server := recordingAgent(&bodies)
	defer server.Close()
	code, _, errout := secretSetAtTerminal(t, "right-token", server.URL, secretManifestDir(t, ingressBerthyYMLWithoutDNS), "CF_TOKEN")
	if code != 0 || len(bodies) != 1 {
		t.Fatalf("exit %d, %d requests: stderr %q", code, len(bodies), errout)
	}
	if !strings.Contains(errout, "checked: Cloudflare accepts the token") || strings.Contains(errout, "zone") {
		t.Fatalf("stderr %q, want the token check and no zone line", errout)
	}
}

// TestSecretSetRefusesACloudflareTokenThatCannotReadTunnels checks that a token refused the tunnel listing is not stored.
func TestSecretSetRefusesACloudflareTokenThatCannotReadTunnels(t *testing.T) {
	fake, _ := fakeCloudflare(t, "right-token", "example.com")
	fake.FailRequests("GET", "/accounts/acct/cfd_tunnel", http.StatusForbidden)
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	code, out, errout := secretSetAtTerminal(t, "right-token", server.URL, secretManifestDir(t, ingressBerthyYML), "CF_TOKEN")
	if code != 1 || !strings.Contains(errout, "not stored: the token cannot read tunnels in account acct") || calls.Load() != 0 {
		t.Fatalf("exit %d stderr %q (agent calls %d), want exit 1 with the refusal and no call", code, errout, calls.Load())
	}
	assertNotPrinted(t, "right-token", out, errout)
}

// storedWhenCloudflareFails runs secret set with the right token while Cloudflare answers the GET requests on paths with
// status. It checks that the value is stored and that the check says it could not be asked: a server error or a rate
// limit is Cloudflare not answering, not Cloudflare refusing the token.
func storedWhenCloudflareFails(t *testing.T, status int, paths ...string) {
	t.Helper()
	fake, _ := fakeCloudflare(t, "right-token", "example.com")
	for _, p := range paths {
		fake.FailRequests("GET", p, status)
	}
	var bodies []string
	server := recordingAgent(&bodies)
	defer server.Close()
	code, _, errout := secretSetAtTerminal(t, "right-token", server.URL, secretManifestDir(t, ingressBerthyYML), "CF_TOKEN")
	if code != 0 || len(bodies) != 1 {
		t.Fatalf("HTTP %d on %v: exit %d, %d agent requests: stderr %q", status, paths, code, len(bodies), errout)
	}
	if !strings.Contains(errout, "not checked: Cloudflare could not be asked from this machine") || strings.Contains(errout, "not stored") {
		t.Fatalf("HTTP %d on %v: stderr %q, want the not-checked note and no refusal", status, paths, errout)
	}
}

// TestSecretSetStoresACloudflareTokenWhenCloudflareIsDown checks that a 503 on token verification, on both endpoints the
// check may ask, stores the value rather than refusing it.
func TestSecretSetStoresACloudflareTokenWhenCloudflareIsDown(t *testing.T) {
	storedWhenCloudflareFails(t, http.StatusServiceUnavailable, "/user/tokens/verify", "/accounts/acct/tokens/verify")
}

// TestSecretSetStoresACloudflareTokenWhenTheTunnelListingFailsWithAServerError checks that a 500 on the tunnel listing is
// not taken for a refusal of the token.
func TestSecretSetStoresACloudflareTokenWhenTheTunnelListingFailsWithAServerError(t *testing.T) {
	storedWhenCloudflareFails(t, http.StatusInternalServerError, "/accounts/acct/cfd_tunnel")
}

// TestSecretSetStoresACloudflareTokenWhenCloudflareRateLimitsIt checks that a 429 on token verification, on both endpoints,
// stores the value rather than refusing it.
func TestSecretSetStoresACloudflareTokenWhenCloudflareRateLimitsIt(t *testing.T) {
	storedWhenCloudflareFails(t, http.StatusTooManyRequests, "/user/tokens/verify", "/accounts/acct/tokens/verify")
}

// TestSecretSetStoresACloudflareTokenWhenCloudflareCannotBeReached checks that an unreachable Cloudflare stores the value
// and says it was not checked.
func TestSecretSetStoresACloudflareTokenWhenCloudflareCannotBeReached(t *testing.T) {
	t.Setenv("BERTHY_CLOUDFLARE_API_URL", "http://127.0.0.1:1/client/v4")
	var bodies []string
	server := recordingAgent(&bodies)
	defer server.Close()
	code, _, errout := secretSetAtTerminal(t, "right-token", server.URL, secretManifestDir(t, ingressBerthyYML), "CF_TOKEN")
	if code != 0 || len(bodies) != 1 {
		t.Fatalf("exit %d, %d requests: stderr %q", code, len(bodies), errout)
	}
	if !strings.Contains(errout, "not checked: Cloudflare could not be asked from this machine") {
		t.Fatalf("stderr %q lacks the not-checked note", errout)
	}
}

// TestSecretSetRefusesACloudflareURLThatIsNotLoopbackHTTP checks that a plain-HTTP override that is not a loopback host
// is refused before any request, so the token is never sent there.
func TestSecretSetRefusesACloudflareURLThatIsNotLoopbackHTTP(t *testing.T) {
	_, requests := fakeCloudflare(t, "right-token", "example.com")
	t.Setenv("BERTHY_CLOUDFLARE_API_URL", "http://example.com/client/v4")
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	code, out, errout := secretSetAtTerminal(t, "right-token", server.URL, secretManifestDir(t, ingressBerthyYML), "CF_TOKEN")
	if code != 1 || !strings.Contains(errout, "not stored: BERTHY_CLOUDFLARE_API_URL must use https, or http on a loopback host") || calls.Load() != 0 {
		t.Fatalf("exit %d stderr %q (agent calls %d), want exit 1 with the refusal and no call", code, errout, calls.Load())
	}
	if requests.Load() != 0 {
		t.Fatalf("the test Cloudflare received %d requests, want none", requests.Load())
	}
	assertNotPrinted(t, "right-token", out, errout)
}

// TestSecretSetStoresARegistryPasswordThatCanPull checks that a password the registry accepts for the manifest's image
// repository is stored, and says what it checked.
func TestSecretSetStoresARegistryPasswordThatCanPull(t *testing.T) {
	host, _ := fakeRegistry(t)
	var bodies []string
	server := recordingAgent(&bodies)
	defer server.Close()
	dir := secretManifestDir(t, registryManifest(host, host+"/acme/api"))
	code, _, errout := secretSetAtTerminal(t, "right", server.URL, dir, "REGISTRY_TOKEN", "--registry", host)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errout)
	}
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"registry":"`+host+`"`) {
		t.Fatalf("requests %q, want the password sent for %s", bodies, host)
	}
	if want := "checked: " + host + " lets this credential pull acme/api"; !strings.Contains(errout, want) {
		t.Fatalf("stderr %q lacks %q", errout, want)
	}
}

// TestSecretSetRefusesARegistryPasswordThatCannotPull checks that a password the registry refuses is not stored.
func TestSecretSetRefusesARegistryPasswordThatCannotPull(t *testing.T) {
	host, _ := fakeRegistry(t)
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	dir := secretManifestDir(t, registryManifest(host, host+"/acme/api"))
	code, out, errout := secretSetAtTerminal(t, "wrong-pass-4e2b", server.URL, dir, "REGISTRY_TOKEN", "--registry", host)
	if code != 1 || !strings.Contains(errout, "not stored: registry refused the credential") || calls.Load() != 0 {
		t.Fatalf("exit %d stderr %q (agent calls %d), want exit 1 with the refusal and no call", code, errout, calls.Load())
	}
	assertNotPrinted(t, "wrong-pass-4e2b", out, errout)
}

// TestSecretSetSaysWhenNoImageAppPullsFromTheRegistry checks that a registry the manifest names but no image uses is not
// asked about, and the value is stored.
func TestSecretSetSaysWhenNoImageAppPullsFromTheRegistry(t *testing.T) {
	host, requests := fakeRegistry(t)
	var bodies []string
	server := recordingAgent(&bodies)
	defer server.Close()
	dir := secretManifestDir(t, registryManifest(host, "ghcr.io/acme/api"))
	code, _, errout := secretSetAtTerminal(t, "right", server.URL, dir, "REGISTRY_TOKEN", "--registry", host)
	if code != 0 || len(bodies) != 1 {
		t.Fatalf("exit %d, %d requests: stderr %q", code, len(bodies), errout)
	}
	if want := "not checked: no image app in the manifest pulls from " + host; !strings.Contains(errout, want) {
		t.Fatalf("stderr %q lacks %q", errout, want)
	}
	if requests.Load() != 0 {
		t.Fatalf("the registry received %d requests, want none", requests.Load())
	}
}

// TestSecretSetWithARegistryTheManifestDoesNotNameIsNotChecked checks that a --registry the manifest does not name, or a
// missing manifest, is stored without any check and without a note about one.
func TestSecretSetWithARegistryTheManifestDoesNotNameIsNotChecked(t *testing.T) {
	host, requests := fakeRegistry(t)
	for _, dir := range []string{secretManifestDir(t, ingressBerthyYML), t.TempDir()} {
		var bodies []string
		server := recordingAgent(&bodies)
		code, _, errout := secretSetAtTerminal(t, "right", server.URL, dir, "REGISTRY_TOKEN", "--registry", host)
		server.Close()
		if code != 0 || len(bodies) != 1 {
			t.Fatalf("%s: exit %d, %d requests: stderr %q", dir, code, len(bodies), errout)
		}
		if strings.Contains(errout, "checked:") || strings.Contains(errout, "not checked:") {
			t.Fatalf("%s: stderr %q, want no check note", dir, errout)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("the registry received %d requests, want none", requests.Load())
	}
}

// TestSecretSetStoresARegistryPasswordWhenTheRegistryCannotBeReached checks that an unreachable registry stores the value
// and says it was not checked.
func TestSecretSetStoresARegistryPasswordWhenTheRegistryCannotBeReached(t *testing.T) {
	var bodies []string
	server := recordingAgent(&bodies)
	defer server.Close()
	dir := secretManifestDir(t, registryManifest("127.0.0.1:1", "127.0.0.1:1/acme/api"))
	code, _, errout := secretSetAtTerminal(t, "right", server.URL, dir, "REGISTRY_TOKEN", "--registry", "127.0.0.1:1")
	if code != 0 || len(bodies) != 1 {
		t.Fatalf("exit %d, %d requests: stderr %q", code, len(bodies), errout)
	}
	if want := "not checked: 127.0.0.1:1 could not be asked about acme/api from this machine"; !strings.Contains(errout, want) {
		t.Fatalf("stderr %q lacks %q", errout, want)
	}
}

// TestSecretSetFromAPipeIsNeverChecked checks that a value piped in is stored without any request to Cloudflare, even
// when Cloudflare would refuse it.
func TestSecretSetFromAPipeIsNeverChecked(t *testing.T) {
	_, requests := fakeCloudflare(t, "right-token", "example.com")
	var bodies []string
	server := recordingAgent(&bodies)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	withStdin(t, "wrong-token\n")
	var out, errout bytes.Buffer
	args := []string{"secret", "set", "CF_TOKEN", "-e", "dev", "--config", cfg, "--manifest-dir", secretManifestDir(t, ingressBerthyYML)}
	if code := Run(args, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"value":"wrong-token"`) {
		t.Fatalf("requests %q, want the piped value", bodies)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("the test Cloudflare received %d requests from a pipe, want none", n)
	}
	if errout.Len() != 0 {
		t.Fatalf("stderr %q, want nothing from a pipe", errout.String())
	}
}
