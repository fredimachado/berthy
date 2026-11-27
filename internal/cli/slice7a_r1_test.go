package cli

import (
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
	"testing"

	"berthy/internal/agentclient"
	"berthy/internal/deploy"
	"berthy/internal/manifest"
)

func TestR1MalformedRegistrySubtree(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"registry":"registry.example","services":{"error":[{"detail":"fixture-token"}]},"unknown":"safe"}`)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	for _, asJSON := range []bool{false, true} {
		var out, errs bytes.Buffer
		code := sendCommand(context.Background(), server.Client(), base, "/", "fixture-token", "POST", "secrets", nil, commandFlags{pos: []string{"set", "PASS"}, json: asJSON}, "secret", &out, &errs)
		if code != 0 || strings.Contains(out.String()+errs.String(), "fixture-token") || !strings.Contains(out.String(), "[redacted]") || !strings.Contains(out.String(), "safe") {
			t.Error("malformed registry subtree exposed token or changed response")
		}
	}
}

func TestScalarServicesFallbackRedactsCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"registry":"registry.example","services":"fixture-token","other":"unchanged"}`)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	for _, asJSON := range []bool{false, true} {
		t.Run(fmt.Sprintf("json-%t", asJSON), func(t *testing.T) {
			var out, errs bytes.Buffer
			code := sendCommand(context.Background(), server.Client(), base, "/", "fixture-token", "POST", "secrets", nil, commandFlags{pos: []string{"set", "PASS"}, json: asJSON}, "secret", &out, &errs)
			if code != 0 || strings.Contains(out.String()+errs.String(), "fixture-token") || !strings.Contains(out.String(), "[redacted]") || !strings.Contains(out.String(), "unchanged") {
				t.Error("scalar services fallback exposed a credential or changed the response")
			}
		})
	}
}

func TestRegistryDiagnosticDescendantsPreserveOtherFields(t *testing.T) {
	input := `{"other":{"message":"token"},"registry":{"nested":["token",{"key":"token"}]},"services_error":[{"nested":"token"}],"services":[["token"],{"child":{"leaf":"token"}}],"n":1e+100}`
	want := `{"other":{"message":"token"},"registry":{"nested":["[redacted]",{"key":"[redacted]"}]},"services_error":[{"nested":"[redacted]"}],"services":[["[redacted]"],{"child":{"leaf":"[redacted]"}}],"n":1e+100}`
	if string(redactRegistryJSON([]byte(input), "token")) != want {
		t.Error("diagnostic descendant redaction changed unrelated fields, types or order")
	}
}

func TestR1InheritedRegistryDisplay(t *testing.T) {
	fakeTerminal(t, "value")
	for _, asJSON := range []bool{false, true} {
		var errs bytes.Buffer
		value, registry, code := secretValue(context.Background(), commandFlags{pos: []string{"set", "PASS"}, dir: t.TempDir(), json: asJSON}, &errs, func() ([]storedSecret, bool) {
			return []storedSecret{{Name: "PASS", Registry: "registry-fixture-token.example"}}, true
		}, "fixture-token")
		if code != 0 || value != "value" || registry != "registry-fixture-token.example" || strings.Contains(errs.String(), "fixture-token") || strings.Count(errs.String(), "[redacted]") < 2 || !strings.Contains(errs.String(), "Value of PASS") {
			t.Error("inherited registry display exposed token or changed submitted value")
		}
	}
}

func TestR1SpecializedHeldCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
		fmt.Fprint(w, `{"approval_id":"ap1","expires":"2026-11-18T12:00:00Z","plan":{"target":"fixture-token"}}`)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	for _, asJSON := range []bool{false, true} {
		for _, kind := range []string{"backup", "job", "cancel", "state", "declared"} {
			t.Run(fmt.Sprintf("%s-%t", kind, asJSON), func(t *testing.T) {
				var out, errs bytes.Buffer
				ctx := context.Background()
				code := 0
				switch kind {
				case "backup":
					code = runBackupCommand(ctx, commandFlags{pos: []string{"run", "task"}, json: asJSON}, base, "/", "fixture-token", &out, &errs)
				case "job":
					code = runJobRun(ctx, server.Client(), base, "/", "fixture-token", "task", asJSON, &out, &errs)
				case "cancel":
					code = runJobCancel(ctx, commandFlags{pos: []string{"cancel", "task"}, json: asJSON}, base, "/", "fixture-token", &out, &errs)
				case "state":
					code = runStateBackup(ctx, server.Client(), base, "/", "fixture-token", asJSON, &out, &errs)
				case "declared":
					dir := declaredManifestDir(t)
					m, err := manifest.Load(dir, "dev")
					if err != nil {
						t.Fatal(err)
					}
					_, result := generateDeclared(ctx, server.Client(), &agentclient.Target{Base: base, Prefix: "/", Token: "fixture-token"}, m, dir, false, &out, &errs, asJSON, false)
					if result == declaredHeld {
						code = exitHeld
					}
				}
				if code != exitHeld || strings.Contains(out.String()+errs.String(), "fixture-token") || !strings.Contains(out.String(), "[redacted]") {
					t.Error("held response exposed token or changed status")
				}
			})
		}
	}
}

func TestR1HeldEscapedC1(t *testing.T) {
	data := []byte(`{"approval_id":"ap1","expires":"2026-11-18T12:00:00Z","plan":{"detail":"\u009bfixture-token"}}`)
	var out, errs bytes.Buffer
	code, held := reportHeldMarkers(data, &out, &errs, true, nil, "fixture-token")
	var doc struct {
		Plan struct {
			Detail string `json:"detail"`
		} `json:"plan"`
	}
	if !held || code != exitHeld || bytes.Contains(out.Bytes(), []byte("\u009b")) || json.Unmarshal(out.Bytes(), &doc) != nil || doc.Plan.Detail != "\u009b[redacted]" {
		t.Error("held JSON exposed a control or changed decoded content")
	}
}

func TestR1PromotionTransportOverlap(t *testing.T) {
	base, _ := url.Parse("http://longshorttail.invalid")
	c := &http.Client{Transport: progressRoundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("unavailable") })}
	_, _, err := readApplied(context.Background(), c, &agentclient.Target{Base: base, Token: "short"}, "app", "longshorttail")
	if err == nil || strings.Contains(err.Error(), "long") || strings.Contains(err.Error(), "tail") || !strings.Contains(err.Error(), "[redacted]") {
		t.Error("transport error exposed part of overlapping credential")
	}
}

func TestR1InvalidUTF8WireSecret(t *testing.T) {
	value := string([]byte{'x', 0xff, 0xfe, 'y'})
	wire := "x\ufffd\ufffdy"
	gotWire := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotWire = body["value"] == wire
		w.WriteHeader(403)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": wire})
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	var out, errs bytes.Buffer
	code := sendCommand(context.Background(), server.Client(), base, "/", "token", "POST", "secrets", map[string]string{"value": value}, commandFlags{pos: []string{"set", "PASS"}}, "secret", &out, &errs, value)
	if code != 1 || !gotWire || strings.Contains(out.String()+errs.String(), wire) || !strings.Contains(errs.String(), "[redacted]") {
		t.Error("wire-normalized secret was not redacted")
	}
}

func TestR1GuidedInvalidUTF8WireSecret(t *testing.T) {
	value := string([]byte{'x', 0xff, 0xfe, 'y'})
	wire := "x\ufffd\ufffdy"
	for _, mode := range []string{"registry", "refusal", "held"} {
		t.Run(mode, func(t *testing.T) {
			gotWire := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					fmt.Fprint(w, `[]`)
					return
				}
				var body map[string]string
				_ = json.NewDecoder(r.Body).Decode(&body)
				gotWire = body["value"] == wire
				switch mode {
				case "registry":
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "registry": "registry.example", "services_error": wire})
				case "refusal":
					w.WriteHeader(403)
					_ = json.NewEncoder(w).Encode(map[string]string{"message": wire})
				case "held":
					w.WriteHeader(202)
					_ = json.NewEncoder(w).Encode(map[string]any{"approval_id": "ap1", "expires": "2026-11-18T12:00:00Z", "plan": map[string]any{"action": "secret set", "name": "REGISTRY_TOKEN", "registry": "registry.example", "exists": false, "services_error": wire}})
				}
			}))
			defer server.Close()
			dir := secretManifestDir(t, registryManifest("registry.example", "example.com/api"))
			m, err := manifest.Load(dir, "dev")
			if err != nil {
				t.Fatal(err)
			}
			base, _ := url.Parse(server.URL)
			var out, errs bytes.Buffer
			_, result := guideSecrets(context.Background(), server.Client(), &agentclient.Target{Base: base, Prefix: "/", Token: "token"}, m, dir, "dev", func() ([]byte, error) { return []byte(value), nil }, &out, &errs)
			want := guideDone
			if mode == "refusal" {
				want = guideFailed
			}
			if mode == "held" {
				want = guideHeld
			}
			if !gotWire || result != want || strings.Contains(out.String()+errs.String(), wire) || !strings.Contains(out.String()+errs.String(), "[redacted]") {
				t.Error("guide did not redact the submitted wire value")
			}
		})
	}
}

func TestR1InheritedRegistryRequestUnchanged(t *testing.T) {
	fakeTerminal(t, "value")
	registry := "registry-test-token.example"
	got := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			_ = json.NewEncoder(w).Encode([]storedSecret{{Name: "PASS", Registry: registry}})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got, _ = body["registry"].(string)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errs bytes.Buffer
	code := Run([]string{"secret", "set", "PASS", "-e", "dev", "--config", cfg, "--manifest-dir", t.TempDir(), "--json"}, &out, &errs)
	if code != 0 || got != registry || strings.Contains(out.String()+errs.String(), "test-token") || strings.Count(errs.String(), "[redacted]") < 2 {
		t.Error("inherited registry changed request or exposed bearer")
	}
}

func TestR1InheritedRegistryCheckerUnchanged(t *testing.T) {
	host, calls := fakeRegistry(t)
	fakeTerminal(t, "right")
	dir := secretManifestDir(t, registryManifest(host, host+"/acme/api"))
	var errs bytes.Buffer
	value, registry, code := secretValue(context.Background(), commandFlags{pos: []string{"set", "REGISTRY_TOKEN"}, dir: dir, env: "dev"}, &errs, func() ([]storedSecret, bool) { return []storedSecret{{Name: "REGISTRY_TOKEN", Registry: host}}, true }, "127.0.0.1")
	if code != 0 || value != "right" || registry != host || calls.Load() < 1 || strings.Contains(errs.String(), "127.0.0.1") || strings.Count(errs.String(), "[redacted]") < 2 {
		t.Error("inherited checker target changed or bearer was exposed")
	}
}

func TestR1NamedInvalidUTF8WireSecret(t *testing.T) {
	value := string([]byte{'x', 0xff, 0xfe, 'y'})
	wire := "x\ufffd\ufffdy"
	gotWire := false
	fakeTerminal(t, value)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			fmt.Fprint(w, `[]`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotWire = body["value"] == wire
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "registry": "registry.example", "services_error": wire})
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errs bytes.Buffer
	code := Run([]string{"secret", "set", "PASS", "-e", "dev", "--config", cfg, "--manifest-dir", t.TempDir(), "--registry", "registry.example", "--json"}, &out, &errs)
	if code != 0 || !gotWire || strings.Contains(out.String()+errs.String(), wire) || !strings.Contains(out.String(), "[redacted]") {
		t.Error("named secret leaked the submitted wire value")
	}
}

func TestR1KeptVersionRefusalStyle(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "")
	forceProgressVT(t)
	old := progressWidth
	progressWidth = func(io.Writer) (int, error) { return 80, nil }
	t.Cleanup(func() { progressWidth = old })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprint(w, `{"message":"denied"}`)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	var errs bytes.Buffer
	code := checkKeptVersions(context.Background(), server.Client(), &agentclient.Target{Base: base}, []deployStep{{app: "api", req: deploy.Request{KeepVersion: true}}}, &errs, false)
	if code != 1 || !strings.Contains(errs.String(), "\x1b[31m") || !strings.Contains(errs.String(), "(nothing was deployed)") {
		t.Error("kept version refusal was not styled at destination")
	}
}
