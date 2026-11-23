package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/agentclient"
	"berthy/internal/credstore/credtest"
)

func TestIntegratedDeclaredAnswerMustCoverManifest(t *testing.T) {
	row := `{"name":"DB_PASS","outcome":"generate","bytes":32,"format":"hex"}`
	for _, tc := range []struct{ name, rows string }{{"missing", row}, {"duplicate", row + "," + row}, {"extra", row + `,{"name":"OTHER","outcome":"generate","bytes":32,"format":"hex"}`}, {"refusal without reader", `{"name":"DB_PASS","outcome":"refuse","bytes":32,"format":"hex"},{"name":"GEN_PASS","outcome":"generate","bytes":32,"format":"hex"}`}} {
		t.Run(tc.name, func(t *testing.T) {
			reads := fakeTerminal(t, "")
			var bodies []string
			server := answeringAgent(&bodies, `[]`, `{"action":"secret generate declared","secrets":[`+tc.rows+`]}`)
			defer server.Close()
			cfg := ingressCLIConfig(t, server.URL)
			dir := declaredManifestDir(t)
			var out, errout bytes.Buffer
			for _, command := range [][]string{{"secret", "generate", "--json"}, {"secret", "set"}} {
				bodies = nil
				out.Reset()
				errout.Reset()
				args := append(command, "-e", "dev", "--config", cfg, "--manifest-dir", dir)
				code := Run(args, &out, &errout)
				if code != 1 || len(bodies) != 1 || reads.Load() != 0 || !strings.Contains(errout.String(), "invalid agent response") {
					t.Errorf("command %v exit %d calls %d reads %d stdout %q stderr %q", command, code, len(bodies), reads.Load(), out.String(), errout.String())
				}
			}
		})
	}
}

func TestIntegratedInventoryRejectsInvalidRows(t *testing.T) {
	for _, body := range []string{`[null]`, `[{}]`, `[42]`, `[{"name":"A","registry":""},{"name":"A","registry":"ghcr.io"}]`} {
		t.Run(body, func(t *testing.T) {
			reads := fakeTerminal(t, "")
			var bodies []string
			server := answeringAgent(&bodies, body, `{"ok":true}`)
			defer server.Close()
			cfg := ingressCLIConfig(t, server.URL)
			dir := secretManifestDir(t, "project: demo\nnetwork: overlay\napps:\n  api:\n    kind: image\n    image: example.com/api\n    env:\n      secret: [API_KEY]\n    roles:\n      web:\n        port: 8080\n")
			var out, errout bytes.Buffer
			code := Run([]string{"secret", "set", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
			if code != 1 || len(bodies) != 1 || reads.Load() != 0 || !strings.Contains(errout.String(), "invalid agent response") {
				t.Fatalf("exit %d calls %d reads %d stdout %q stderr %q", code, len(bodies), reads.Load(), out.String(), errout.String())
			}
		})
	}
}

func TestIntegratedGuideHidesBearerInAgentErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared bool
	}{{"generation", true}, {"inventory", false}} {
		t.Run(tc.name, func(t *testing.T) {
			reads := fakeTerminal(t, "")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(400)
				fmt.Fprintf(w, `{"message":"refused %s"}`, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			}))
			defer server.Close()
			cfg := ingressCLIConfig(t, server.URL)
			dir := secretManifestDir(t, ingressBerthyYML)
			if tc.declared {
				dir = declaredManifestDir(t)
			}
			var out, errout bytes.Buffer
			code := Run([]string{"secret", "set", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
			if code != 1 || reads.Load() != 0 || strings.Contains(out.String()+errout.String(), "test-token") || !strings.Contains(errout.String(), "refused") {
				t.Fatalf("exit %d reads %d stdout %q stderr %q", code, reads.Load(), out.String(), errout.String())
			}
		})
	}
}

func TestIntegratedGuideRedactsRegistryRedirectCredential(t *testing.T) {
	const password = "synthetic-registry-value"
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Location", "/bad%xx/"+password)
		w.WriteHeader(302)
	}))
	defer reg.Close()
	host := strings.TrimPrefix(reg.URL, "http://")
	reads := fakeTerminal(t, password)
	var bodies []string
	server := answeringAgent(&bodies, `[]`, fmt.Sprintf(`{"ok":true,"registry":%q}`, host))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "set", "-e", "dev", "--config", cfg, "--manifest-dir", secretManifestDir(t, registryManifest(host, host+"/acme/api"))}, &out, &errout)
	if code != 0 || reads.Load() != 1 || !strings.Contains(errout.String(), "not checked:") || strings.Contains(out.String()+errout.String(), password) {
		t.Fatalf("exit %d reads %d stdout %q stderr %q", code, reads.Load(), out.String(), errout.String())
	}
}

func TestIntegratedSetupHidesSavedBearerFromInventoryError(t *testing.T) {
	store := credtest.NewMemory()
	old := agentclient.TokenStore
	agentclient.TokenStore = store
	t.Cleanup(func() { agentclient.TokenStore = old; store.Wait() })
	runner := freshServer(t, "ops@s1")
	runner.probe = strings.ReplaceAll(runner.probe, "172.31.250.10", "127.0.0.1")
	withSetupRunner(t, runner)
	reads := fakeTerminal(t, "")
	var bearer string
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.WriteHeader(400)
		fmt.Fprintf(w, `{"message":"refused %s"}`, bearer)
	}))
	defer agent.Close()
	dir := projectDir(t)
	var out, errout bytes.Buffer
	code := Run([]string{"setup", "-e", "p5", "--host", "ops@s1", "--listen", strings.TrimPrefix(agent.URL, "http://"), "--manifest-dir", dir, "--config", filepath.Join(t.TempDir(), "config.json")}, &out, &errout)
	if code != 0 || bearer == "" || reads.Load() != 0 || strings.Contains(out.String()+errout.String(), bearer) || !strings.Contains(out.String(), "set the secrets the manifest names:") || !strings.Contains(errout.String(), "refused") {
		t.Fatalf("exit %d reads %d bearer leaked %t", code, reads.Load(), strings.Contains(out.String()+errout.String(), bearer))
	}
	if store.Len() != 1 {
		t.Fatal("token was not saved")
	}
}

func TestIntegratedGuideHidesBearerInHeldAndDeclaredMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		reply  string
	}{
		{"refusal metadata", 200, `{"action":"secret generate declared","secrets":[{"name":"DB_PASS","outcome":"refuse","bytes":32,"format":"hex","app":"test-token","volume":"test-token"},{"name":"GEN_PASS","outcome":"generate","bytes":32,"format":"hex"}]}`},
		{"held metadata", 202, `{"approval_id":"test-token","expires":"2030-01-01T00:00:00Z","plan":{"action":"secret generate declared","secrets":[{"name":"test-token","app":"test-token"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := fakeTerminal(t, "")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					fmt.Fprint(w, `[]`)
					return
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.reply)
			}))
			defer server.Close()
			cfg := ingressCLIConfig(t, server.URL)
			var out, errout bytes.Buffer
			code := Run([]string{"secret", "set", "-e", "dev", "--config", cfg, "--manifest-dir", declaredManifestDir(t)}, &out, &errout)
			if strings.Contains(out.String()+errout.String(), "test-token") || (tc.status == 202 && (code != exitHeld || reads.Load() != 0)) || (tc.status == 200 && code != 1) {
				t.Fatalf("exit %d reads %d bearer leaked %t", code, reads.Load(), strings.Contains(out.String()+errout.String(), "test-token"))
			}
		})
	}
}

func TestIntegratedGuideHidesBearerInStoreResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		code   int
	}{
		{"refusal", 400, `{"message":"refused test-token"}`, 1},
		{"held", 202, `{"approval_id":"test-token","expires":"2030-01-01T00:00:00Z","plan":{"action":"secret set","name":"API_KEY","registry":"","exists":false,"services_error":"test-token"}}`, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeTerminal(t, "private")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					fmt.Fprint(w, `[]`)
					return
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			cfg := ingressCLIConfig(t, server.URL)
			dir := secretManifestDir(t, "project: demo\nnetwork: overlay\napps:\n  api:\n    kind: image\n    image: example.com/api\n    env:\n      secret: [API_KEY]\n    roles:\n      web:\n        port: 8080\n")
			var out, errout bytes.Buffer
			code := Run([]string{"secret", "set", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
			if code != tc.code || strings.Contains(out.String()+errout.String(), "test-token") {
				t.Fatalf("exit %d bearer leaked %t", code, strings.Contains(out.String()+errout.String(), "test-token"))
			}
		})
	}
	host, _ := fakeRegistry(t)
	fakeTerminal(t, "right")
	var bodies []string
	server := answeringAgent(&bodies, `[]`, fmt.Sprintf(`{"ok":true,"registry":%q,"services_error":"test-token"}`, host))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "set", "-e", "dev", "--config", cfg, "--manifest-dir", secretManifestDir(t, registryManifest(host, host+"/acme/api"))}, &out, &errout)
	if code != 0 || strings.Contains(out.String()+errout.String(), "test-token") {
		t.Fatalf("exit %d bearer leaked %t", code, strings.Contains(out.String()+errout.String(), "test-token"))
	}
}

func TestIntegratedInventoryAcceptsValidRowsAndReservedTunnelName(t *testing.T) {
	for _, body := range []string{`[]`, `[{"name":"TUNNEL_TOKEN"}]`, `[{"name":"TUNNEL_TOKEN","registry":""}]`, `[{"name":"A"},{"name":"A","registry":""}]`} {
		t.Run(body, func(t *testing.T) {
			var requests []string
			server := answeringAgent(&requests, body, `{"ok":true}`)
			defer server.Close()
			cfg := ingressCLIConfig(t, server.URL)
			var out, errout bytes.Buffer
			code := Run([]string{"secret", "list", "--json", "-e", "dev", "--config", cfg, "--manifest-dir", secretManifestDir(t, ingressBerthyYML)}, &out, &errout)
			if code != 0 || len(requests) != 1 || !strings.Contains(out.String(), `"missing"`) {
				t.Fatalf("exit %d calls %d stdout %q stderr %q", code, len(requests), out.String(), errout.String())
			}
		})
	}
}

func TestIntegratedBadInventoryStopsNamedSetAndManifestList(t *testing.T) {
	for _, cmd := range [][]string{{"secret", "set", "API_KEY"}, {"secret", "list", "--manifest-dir"}} {
		t.Run(strings.Join(cmd, "-"), func(t *testing.T) {
			reads := fakeTerminal(t, "value")
			var requests []string
			server := answeringAgent(&requests, `[{"name":"A","registry":"one"},{"name":"A","registry":"two"}]`, `{"ok":true}`)
			defer server.Close()
			cfg := ingressCLIConfig(t, server.URL)
			args := append([]string{}, cmd...)
			if cmd[1] == "list" {
				args = append(args, secretManifestDir(t, ingressBerthyYML))
			}
			args = append(args, "-e", "dev", "--config", cfg)
			var out, errout bytes.Buffer
			code := Run(args, &out, &errout)
			if code != 1 || len(requests) != 1 || reads.Load() != 0 || !strings.Contains(errout.String(), "invalid agent response") {
				t.Fatalf("exit %d requests %d reads %d stderr %q", code, len(requests), reads.Load(), errout.String())
			}
		})
	}
}
