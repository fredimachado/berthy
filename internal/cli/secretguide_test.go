package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecretSetWithoutANameSkipsOnAnEmptyLine(t *testing.T) {
	reads := fakeTerminal(t, "")
	var requests []string
	server := recordingAgent(&requests)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := secretManifestDir(t, "project: demo\nnetwork: overlay\napps:\n  api:\n    kind: image\n    image: example.com/api\n    env:\n      secret: [API_KEY]\n    roles:\n      web:\n        port: 8080\n")
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "set", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if code != 1 || reads.Load() != 1 || !strings.Contains(out.String(), "skipped API_KEY") {
		t.Fatalf("exit %d reads %d requests %v stdout %q stderr %q", code, reads.Load(), requests, out.String(), errout.String())
	}
}

func TestSecretSetWithoutANameRejectsFlagsAndTailBeforeRequests(t *testing.T) {
	for _, extra := range [][]string{{"--json"}, {"--dry-run"}, {"--", "API_KEY"}} {
		t.Run(strings.Join(extra, " "), func(t *testing.T) {
			reads := fakeTerminal(t, "value")
			var requests []string
			server := recordingAgent(&requests)
			defer server.Close()
			cfg := ingressCLIConfig(t, server.URL)
			var out, errout bytes.Buffer
			args := append([]string{"secret", "set", "-e", "dev", "--config", cfg}, extra...)
			code := Run(args, &out, &errout)
			if code != 2 || reads.Load() != 0 || len(requests) != 0 {
				t.Fatalf("exit %d reads %d requests %v stderr %s", code, reads.Load(), requests, errout.String())
			}
		})
	}
}

func TestSecretSetWithoutANameChecksTheProjectFirst(t *testing.T) {
	reads := fakeTerminal(t, "value")
	var requests []string
	server := recordingAgent(&requests)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := secretManifestDir(t, "project: other\nnetwork: overlay\napps:\n  api:\n    kind: image\n    image: example.com/api\n    roles:\n      web:\n        port: 8080\n")
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "set", "-e", "dev", "--project", "demo", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if code != 2 || reads.Load() != 0 || len(requests) != 0 {
		t.Fatalf("exit %d reads %d requests %v stderr %s", code, reads.Load(), requests, errout.String())
	}
}

func TestSecretSetWithoutANameMovesAPasswordToTheManifestsRegistry(t *testing.T) {
	host, requests := fakeRegistry(t)
	reads := fakeTerminalLines(t, "wrong", "right")
	var bodies []string
	server := answeringAgent(&bodies, `[{"name":"REGISTRY_TOKEN","registry":"old.example.com"}]`, `{"ok":true,"registry":"`+host+`","services":[]}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := secretManifestDir(t, registryManifest(host, host+"/acme/api"))
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "set", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if code != 0 || reads.Load() != 2 || requests.Load() < 2 || len(bodies) != 2 || !strings.Contains(bodies[1], `"registry":"`+host+`"`) || strings.Contains(errout.String(), "remove it") || !strings.Contains(errout.String(), "not stored: registry refused") {
		t.Fatalf("exit %d reads %d registry requests %d agent request count %d stdout %q stderr %q", code, reads.Load(), requests.Load(), len(bodies), out.String(), errout.String())
	}
	assertNotPrinted(t, "wrong", out.String(), errout.String())
	assertNotPrinted(t, "right", out.String(), errout.String())
}

func TestGuideStopsWhenTheGenerateRequestIsHeld(t *testing.T) {
	reads := fakeTerminal(t, "value")
	var bodies []string
	server := replyingAgent(&bodies, 202, `{"approval_id":"ap1","expires":"2026-11-18T12:00:00Z","plan":{"action":"secret generate declared","secrets":[{"name":"GEN_PASS","outcome":"generate","bytes":32,"format":"hex"}]}}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "set", "-e", "dev", "--config", cfg, "--manifest-dir", declaredManifestDir(t)}, &out, &errout)
	if code != exitHeld || reads.Load() != 0 || len(bodies) != 1 || !strings.Contains(bodies[0], "/secrets/generate-declared") {
		t.Fatalf("exit %d reads %d calls %d stderr %s", code, reads.Load(), len(bodies), errout.String())
	}
}

func TestGuideStopsWhenAReadersStateCannotBeEstablished(t *testing.T) {
	reads := fakeTerminal(t, "value")
	var bodies []string
	server := replyingAgent(&bodies, 400, `{"code":"operation_failed","message":"the state of app cache cannot be established, so nothing is generated"}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "set", "-e", "dev", "--config", cfg, "--manifest-dir", declaredManifestDir(t)}, &out, &errout)
	if code != 1 || reads.Load() != 0 || len(bodies) != 1 || !strings.Contains(errout.String(), "cannot be established") {
		t.Fatalf("exit %d reads %d calls %d stderr %s", code, reads.Load(), len(bodies), errout.String())
	}
}

func TestTheGuidePrintsNoControlCharacterFromTheManifest(t *testing.T) {
	reads := fakeTerminal(t, "")
	var bodies []string
	server := answeringAgent(&bodies, `[]`, `{}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := secretManifestDir(t, registryManifest("ghcr.io\x1b[31m", "example.com/api"))
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "set", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if code != 1 || reads.Load() != 1 || strings.Contains(out.String()+errout.String(), "\x1b") {
		t.Fatalf("exit %d reads %d stdout %q stderr %q", code, reads.Load(), out.String(), errout.String())
	}
	out.Reset()
	errout.Reset()
	code = Run([]string{"secret", "list", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if code != 0 || strings.Contains(out.String()+errout.String(), "\x1b") {
		t.Fatalf("exit %d stdout %q stderr %q", code, out.String(), errout.String())
	}
}

func TestSecretSetWithoutANameReportsASecretSharedByTwoRegistries(t *testing.T) {
	reads := fakeTerminal(t, "unused")
	var bodies []string
	server := answeringAgent(&bodies, `[{"name":"REGISTRY_TOKEN","registry":"ghcr.io"}]`, `{}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	text := registryManifest("ghcr.io", "ghcr.io/acme/api")
	text = strings.Replace(text, "apps:\n", "  registry.example.com:\n    username: u\n    password_secret: REGISTRY_TOKEN\napps:\n", 1)
	dir := secretManifestDir(t, text)
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "set", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if code != 1 || reads.Load() != 0 || len(bodies) != 1 || !strings.Contains(errout.String(), "ghcr.io and registry.example.com") {
		t.Fatalf("exit %d reads %d requests %d stderr %q", code, reads.Load(), len(bodies), errout.String())
	}
	out.Reset()
	errout.Reset()
	code = Run([]string{"secret", "list", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if code != 0 || !strings.Contains(out.String(), "conflict: REGISTRY_TOKEN is the password_secret of ghcr.io and registry.example.com") || strings.Contains(out.String(), "every secret") {
		t.Fatalf("exit %d stdout %q stderr %q", code, out.String(), errout.String())
	}
}

func TestSecretSetWithoutANameGeneratesThenAsksForEachMissingSecret(t *testing.T) {
	reads := fakeTerminal(t, "a-private-value")
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/secrets/generate-declared"):
			fmt.Fprint(w, declaredReply)
		case r.Method == "GET":
			fmt.Fprint(w, `[{"name":"DB_PASS"},{"name":"GEN_PASS"}]`)
		default:
			fmt.Fprint(w, `{"ok":true}`)
		}
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "set", "-e", "dev", "--config", cfg, "--manifest-dir", declaredManifestDir(t)}, &out, &errout)
	if code != 0 || reads.Load() != 1 || len(paths) != 3 || !strings.HasSuffix(paths[0], "/secrets/generate-declared") || !strings.HasPrefix(paths[1], "GET ") || !strings.HasPrefix(paths[2], "POST ") || !strings.Contains(out.String(), "generated GEN_PASS") || !strings.Contains(out.String(), "stored OTHER") {
		t.Fatalf("exit %d reads %d paths %v stdout %q stderr %q", code, reads.Load(), paths, out.String(), errout.String())
	}
	assertNotPrinted(t, "a-private-value", out.String(), errout.String())
}

func TestGuideStopsWhenAStoreIsHeld(t *testing.T) {
	reads := fakeTerminal(t, "private")
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.Method == "GET" {
			fmt.Fprint(w, `[]`)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"approval_id":"ap1","expires":"2026-11-18T12:00:00Z","plan":{"action":"secret set","name":"API_KEY","registry":"","exists":false}}`)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := secretManifestDir(t, "project: demo\nnetwork: overlay\napps:\n  api:\n    kind: image\n    image: example.com/api\n    env:\n      secret: [API_KEY, SECOND]\n    roles:\n      web:\n        port: 8080\n")
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "set", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if code != exitHeld || reads.Load() != 1 || len(paths) != 2 {
		t.Fatalf("exit %d reads %d paths %v stdout %q stderr %q", code, reads.Load(), paths, out.String(), errout.String())
	}
	assertNotPrinted(t, "private", out.String(), errout.String())
}

func TestSecretSetWithoutANameAndNoTerminalIsRefused(t *testing.T) {
	old := hiddenInput
	hiddenInput = func() (func() ([]byte, error), bool) { return nil, false }
	t.Cleanup(func() { hiddenInput = old })
	var bodies []string
	server := recordingAgent(&bodies)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "set", "-e", "dev", "--config", cfg}, &out, &errout)
	if code != 2 || len(bodies) != 0 || !strings.Contains(errout.String(), "secret set without a NAME asks at a terminal") {
		t.Fatalf("exit %d requests %d stderr %q", code, len(bodies), errout.String())
	}
}

func TestGuideRejectsInvalidStoreResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"empty ok", 200, `{}`}, {"false ok", 200, `{"ok":false}`}, {"wrong registry", 200, `{"ok":true,"registry":"unexpected"}`}, {"invalid held", 202, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := fakeTerminal(t, "secret-value")
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.Method+" "+r.URL.Path)
				if r.Method == "GET" {
					fmt.Fprint(w, `[]`)
					return
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			cfg := ingressCLIConfig(t, server.URL)
			dir := secretManifestDir(t, "project: demo\nnetwork: overlay\napps:\n  api:\n    kind: image\n    image: example.com/api\n    env:\n      secret: [API_KEY, SECOND]\n    roles:\n      web:\n        port: 8080\n")
			var out, errout bytes.Buffer
			code := Run([]string{"secret", "set", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
			if code != 1 || reads.Load() != 1 || len(paths) != 2 || !strings.Contains(errout.String(), "invalid agent response") || strings.Contains(out.String(), "stored API_KEY") {
				t.Fatalf("exit %d reads %d paths %v stdout %q stderr %q", code, reads.Load(), paths, out.String(), errout.String())
			}
		})
	}
}

func TestMixedRegistryAndPlainNeedIsAConflict(t *testing.T) {
	for _, held := range []string{`[]`, `[{"name":"REGISTRY_TOKEN","registry":""}]`, `[{"name":"REGISTRY_TOKEN","registry":"ghcr.io"}]`} {
		t.Run(held, func(t *testing.T) {
			reads := fakeTerminal(t, "")
			var bodies []string
			server := answeringAgent(&bodies, held, `{"ok":true}`)
			defer server.Close()
			cfg := ingressCLIConfig(t, server.URL)
			text := strings.Replace(registryManifest("ghcr.io", "ghcr.io/acme/api"), "    roles:\n", "    env:\n      secret: [REGISTRY_TOKEN]\n    roles:\n", 1)
			dir := secretManifestDir(t, text)
			var out, errout bytes.Buffer
			args := []string{"-e", "dev", "--config", cfg, "--manifest-dir", dir}
			code := Run(append([]string{"secret", "set"}, args...), &out, &errout)
			if code != 1 || reads.Load() != 0 || len(bodies) != 1 || !strings.Contains(errout.String(), "registry ghcr.io and app api") || !strings.Contains(errout.String(), "separate secret names") {
				t.Fatalf("exit %d reads %d calls %d stdout %q stderr %q", code, reads.Load(), len(bodies), out.String(), errout.String())
			}
			out.Reset()
			errout.Reset()
			code = Run(append([]string{"secret", "list"}, args...), &out, &errout)
			if code != 0 || !strings.Contains(out.String(), "conflict:") || !strings.Contains(out.String(), "registry ghcr.io and app api") {
				t.Fatalf("exit %d stdout %q stderr %q", code, out.String(), errout.String())
			}
			out.Reset()
			errout.Reset()
			code = Run(append([]string{"secret", "list"}, append(args, "--json")...), &out, &errout)
			var result struct {
				Missing []struct {
					Name     string   `json:"name"`
					NeededBy []string `json:"needed_by"`
				} `json:"missing"`
			}
			if code != 0 || json.Unmarshal(out.Bytes(), &result) != nil || len(result.Missing) != 1 || result.Missing[0].Name != "REGISTRY_TOKEN" || len(result.Missing[0].NeededBy) != 2 {
				t.Fatalf("exit %d stdout %q stderr %q", code, out.String(), errout.String())
			}
		})
	}
}

func TestGuideContinuesAfterEachDeclaredRefusal(t *testing.T) {
	for _, tc := range []struct{ name, volume, want string }{{"deployed", "", "app db was deployed here: enter the value it runs with"}, {"volume", `,"volume":"fixed"`, "the volume fixed of app db exists: enter the value it was created with"}} {
		t.Run(tc.name, func(t *testing.T) {
			reads := fakeTerminalLines(t, "private-db", "private-other")
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.Method+" "+r.URL.Path)
				switch {
				case strings.HasSuffix(r.URL.Path, "/secrets/generate-declared"):
					fmt.Fprintf(w, `{"action":"secret generate declared","secrets":[{"name":"DB_PASS","outcome":"refuse","bytes":32,"format":"hex","app":"db"%s},{"name":"GEN_PASS","outcome":"generate","bytes":32,"format":"hex"}]}`, tc.volume)
				case r.Method == "GET":
					fmt.Fprint(w, `[{"name":"GEN_PASS"}]`)
				default:
					fmt.Fprint(w, `{"ok":true}`)
				}
			}))
			defer server.Close()
			cfg := ingressCLIConfig(t, server.URL)
			var out, errout bytes.Buffer
			code := Run([]string{"secret", "set", "-e", "dev", "--config", cfg, "--manifest-dir", declaredManifestDir(t)}, &out, &errout)
			if code != 0 || reads.Load() != 2 || len(paths) != 4 || !strings.Contains(errout.String(), tc.want) || !strings.Contains(out.String(), "stored DB_PASS") || !strings.Contains(out.String(), "stored OTHER") {
				t.Fatalf("exit %d reads %d paths %v stdout %q stderr %q", code, reads.Load(), paths, out.String(), errout.String())
			}
			assertNotPrinted(t, "private-db", out.String(), errout.String())
			assertNotPrinted(t, "private-other", out.String(), errout.String())
		})
	}
}

func TestGuideInventoryFailureAndNothingMissingAskNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   int
	}{{"failure", 500, `{"message":"inventory failed"}`, 1}, {"complete", 200, `[{"name":"CF_TOKEN"}]`, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			reads := fakeTerminal(t, "unused")
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.Method)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			cfg := ingressCLIConfig(t, server.URL)
			var out, errout bytes.Buffer
			code := Run([]string{"secret", "set", "-e", "dev", "--config", cfg, "--manifest-dir", secretManifestDir(t, ingressBerthyYML)}, &out, &errout)
			if code != tc.want || reads.Load() != 0 || len(paths) != 1 || paths[0] != "GET" || (code == 0 && !strings.Contains(out.String(), "every secret the manifest needs is stored")) {
				t.Fatalf("exit %d reads %d paths %v stdout %q stderr %q", code, reads.Load(), paths, out.String(), errout.String())
			}
		})
	}
}

func TestGuideRetriesARefusedCloudflareToken(t *testing.T) {
	fakeCloudflare(t, "right-token", "example.com")
	reads := fakeTerminalLines(t, "wrong-token", "right-token")
	var bodies []string
	server := answeringAgent(&bodies, `[]`, `{"ok":true}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "set", "-e", "dev", "--config", cfg, "--manifest-dir", secretManifestDir(t, ingressBerthyYML)}, &out, &errout)
	if code != 0 || reads.Load() != 2 || len(bodies) != 2 || !strings.Contains(errout.String(), "not stored: Cloudflare does not accept this token") || !strings.Contains(errout.String(), "checked: Cloudflare accepts the token") || !strings.Contains(out.String(), "stored CF_TOKEN") {
		t.Fatalf("exit %d reads %d request count %d stdout %q stderr %q", code, reads.Load(), len(bodies), out.String(), errout.String())
	}
	assertNotPrinted(t, "wrong-token", out.String(), errout.String())
	assertNotPrinted(t, "right-token", out.String(), errout.String())
}

func TestGuideChecksRegistryAcknowledgementAndShowsRefreshWarnings(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		code        int
		want        string
	}{
		{"missing registry", `{"ok":true}`, 1, "invalid agent response"},
		{"wrong registry", `{"ok":true,"registry":"old.example.com"}`, 1, "invalid agent response"},
		{"refresh warning", `{"ok":true,"registry":%q,"services_error":"refresh failed"}`, 0, "services not updated: refresh failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, _ := fakeRegistry(t)
			reads := fakeTerminal(t, "right")
			var bodies []string
			reply := tc.reply
			if strings.Contains(reply, "%q") {
				reply = fmt.Sprintf(reply, host)
			}
			server := answeringAgent(&bodies, `[]`, reply)
			defer server.Close()
			cfg := ingressCLIConfig(t, server.URL)
			var out, errout bytes.Buffer
			code := Run([]string{"secret", "set", "-e", "dev", "--config", cfg, "--manifest-dir", secretManifestDir(t, registryManifest(host, host+"/acme/api"))}, &out, &errout)
			if code != tc.code || reads.Load() != 1 || len(bodies) != 2 || !strings.Contains(out.String()+errout.String(), tc.want) {
				t.Fatalf("exit %d reads %d calls %d stdout %q stderr %q", code, reads.Load(), len(bodies), out.String(), errout.String())
			}
			if code == 0 && !strings.Contains(out.String(), "Run the command again to retry") {
				t.Fatalf("missing retry advice: %q", out.String())
			}
		})
	}
}
