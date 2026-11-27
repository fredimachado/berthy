package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/manifest"
)

func TestHumanDiagnosticsStyleAfterRedaction(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "")
	forceProgressVT(t)
	old := progressWidth
	var terminal io.Writer
	progressWidth = func(w io.Writer) (int, error) {
		if w == terminal {
			return 80, nil
		}
		return 0, fmt.Errorf("pipe")
	}
	t.Cleanup(func() { progressWidth = old })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"active-credential promotion-credential"}`)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	for _, tc := range []struct{ asJSON, outTerminal, errTerminal bool }{{false, false, true}, {false, true, false}, {true, true, true}, {false, false, false}} {
		var out, errs bytes.Buffer
		terminal = nil
		if tc.outTerminal {
			terminal = &out
		}
		if tc.errTerminal {
			terminal = &errs
		}
		code := sendCommand(context.Background(), server.Client(), base, "/", "active-credential", "POST", "deploys", nil, commandFlags{json: tc.asJSON, redactToken: "promotion-credential"}, "deploy", &out, &errs)
		if code != 1 || out.Len() != 0 || strings.Contains(errs.String(), "credential") || !strings.Contains(errs.String(), "[redacted] [redacted]") || strings.Contains(errs.String(), "\x1b[31m") != (tc.errTerminal && !tc.asJSON) {
			t.Error("refusal did not preserve exit and apply redaction and writer mode")
		}
	}
	t.Run("held secret", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"approval_id":"ap1","expires":"2026-11-18T12:00:00Z","plan":{"action":"secret set","detail":"entered-credential active-credential"}}`)
		}))
		defer server.Close()
		base, _ := url.Parse(server.URL)
		for _, jsonOutput := range []bool{false, true} {
			var out, errs bytes.Buffer
			code := sendCommand(context.Background(), server.Client(), base, "/", "active-credential", "POST", "secrets", nil, commandFlags{pos: []string{"set", "name"}, json: jsonOutput}, "secret", &out, &errs, "entered-credential")
			if code != exitHeld || strings.Contains(out.String()+errs.String(), "credential") || !strings.Contains(out.String(), "[redacted]") {
				t.Error("held diagnostic exposed a credential or changed exit")
			}
		}
	})
}

func TestBackupAdmissionRefusalRedactsToken(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "")
	forceProgressVT(t)
	old := progressWidth
	progressWidth = func(io.Writer) (int, error) { return 80, nil }
	t.Cleanup(func() { progressWidth = old })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"backup-credential"}`)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	for _, asJSON := range []bool{false, true} {
		var out, errs bytes.Buffer
		code := runBackupCommand(context.Background(), commandFlags{pos: []string{"run", "task"}, json: asJSON}, base, "/", "backup-credential", &out, &errs)
		if code != 1 || out.Len() != 0 || strings.Contains(errs.String(), "backup-credential") || !strings.Contains(errs.String(), "[redacted]") || strings.Contains(errs.String(), "\x1b[31m") == asJSON {
			t.Error("backup admission exposed its credential, changed its exit or writer mode")
		}
	}
}

func TestSecretCheckAndRegistryDiagnosticsRedactBothValues(t *testing.T) {
	var errs bytes.Buffer
	g := secretGuide{check: func(context.Context, string) checkResult {
		return checkResult{notes: []string{"entered-credential test-token\rnote"}, refusal: "entered-credential test-token"}
	}}
	_, result := askSecret(context.Background(), g, "prompt: ", func() ([]byte, error) { return []byte("entered-credential"), nil }, &errs, false, "test-token")
	if result != askRefused || strings.Contains(errs.String(), "entered-credential") || strings.Contains(errs.String(), "test-token") || strings.Contains(errs.String(), "\r") || !strings.Contains(errs.String(), "[redacted] [redacted]") {
		t.Error("checker diagnostic exposed its entered value or control")
	}
	t.Run("redact before sanitizing", func(t *testing.T) {
		var out bytes.Buffer
		secret := "entered\rcredential"
		g := secretGuide{check: func(context.Context, string) checkResult { return checkResult{refusal: secret + " token\x1b"} }}
		_, result := askSecret(context.Background(), g, "", func() ([]byte, error) { return []byte(secret), nil }, &out, false, "token")
		if result != askRefused || out.String() != "\nnot stored: [redacted] [redacted]\\x1b\n" {
			t.Error("checker did not redact before sanitizing")
		}
	})

	t.Run("registry", func(t *testing.T) {
		for _, tc := range []struct {
			name, reply string
			asJSON      bool
			status      int
		}{
			{"services error", `{"ok":true,"registry":"registry.example","services_error":"entered-credential test-token\rproblem","extra":42}`, false, 200},
			{"service error", `{"ok":true,"registry":"registry.example","services":[{"name":"entered-credential","updated":false,"error":"test-token entered-credential"}],"extra":42}`, false, 200},
			{"json", `{"ok":true,"registry":"registry.example","services_error":"entered-credential test-token","extra":42}`, true, 200},
			{"json case variants", `{"ok":true,"Registry":"registry.example","Services":[{"Name":"entered-credential","Updated":false,"Error":"test-token"}],"extra":42}`, true, 200},
			{"invalid service shape", `{"ok":true,"registry":"registry.example","services_error":"entered-credential test-token","services":"invalid","extra":42}`, false, 200},
			{"refusal", `{"message":"entered-credential test-token"}`, false, 403},
		} {
			t.Run(tc.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == "GET" {
						fmt.Fprint(w, `[]`)
						return
					}
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.reply)
				}))
				defer server.Close()
				extra := []string{"--registry", "registry.example"}
				if tc.asJSON {
					extra = append(extra, "--json")
				}
				code, out, errout := secretSetAtTerminal(t, "entered-credential", server.URL, t.TempDir(), "REGISTRY_TOKEN", extra...)
				if (tc.status == 200 && code != 0) || (tc.status != 200 && code != 1) || strings.Contains(out+errout, "entered-credential") || strings.Contains(out+errout, "test-token") || !strings.Contains(out+errout, "[redacted]") {
					t.Error("registry diagnostic exposed credentials or changed exit")
				}
				if tc.status == 200 && !tc.asJSON && tc.name != "invalid service shape" && !strings.Contains(out, "retry the services") {
					t.Error("missing retry advice")
				}
				if tc.asJSON && !strings.Contains(out, `"extra": 42`) {
					t.Error("extra field was lost")
				}
			})
		}
	})
	t.Run("guided registry", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" {
				fmt.Fprint(w, `[]`)
				return
			}
			fmt.Fprint(w, `{"ok":true,"registry":"registry.example","services":[{"name":"entered-credential","updated":false,"error":"test-token entered-credential"}]}`)
		}))
		defer server.Close()
		dir := secretManifestDir(t, registryManifest("registry.example", "example.com/api"))
		m, err := manifest.Load(dir, "dev")
		if err != nil {
			t.Fatal(err)
		}
		base, _ := url.Parse(server.URL)
		var out, errout bytes.Buffer
		_, result := guideSecrets(context.Background(), server.Client(), &agentclient.Target{Base: base, Prefix: "/", Token: "test-token"}, m, dir, "dev", func() ([]byte, error) { return []byte("entered-credential"), nil }, &out, &errout)
		if result != guideDone || strings.Contains(out.String()+errout.String(), "entered-credential") || strings.Contains(out.String()+errout.String(), "test-token") || !strings.Contains(out.String(), "[redacted]") || !strings.Contains(out.String(), "retry the services") {
			t.Error("guided registry diagnostic was not safely reported")
		}
	})
	t.Run("wait clear", func(t *testing.T) {
		slice4cTerminal(t)
		synctest.Test(t, func(t *testing.T) {
			var output progressTestBuffer
			gate := make(chan struct{})
			var once sync.Once
			done := make(chan askResult, 1)
			defer func() { once.Do(func() { close(gate) }); <-done }()
			g := secretGuide{check: func(context.Context, string) checkResult {
				<-gate
				return checkResult{notes: []string{"entered-credential\rnote"}, refusal: "entered-credential"}
			}}
			go func() {
				_, result := askSecret(context.Background(), g, "prompt: ", func() ([]byte, error) { return []byte("entered-credential"), nil }, &output, true, "test-token")
				done <- result
			}()
			synctest.Wait()
			time.Sleep(250 * time.Millisecond)
			synctest.Wait()
			if !strings.Contains(output.String(), "checking secret") {
				t.Error("missing wait frame")
			}
			once.Do(func() { close(gate) })
			synctest.Wait()
			got := output.String()
			if !strings.Contains(got, "\r\x1b[2K[redacted]") || strings.Contains(got, "entered-credential") {
				t.Error("diagnostic was not cleared and redacted")
			}
		})
	})
	t.Run("json suppresses checker frame", func(t *testing.T) {
		slice4cTerminal(t)
		synctest.Test(t, func(t *testing.T) {
			var output progressTestBuffer
			gate := make(chan struct{})
			var once sync.Once
			done := make(chan askResult, 1)
			defer func() { once.Do(func() { close(gate) }); <-done }()
			g := secretGuide{check: func(context.Context, string) checkResult {
				<-gate
				return checkResult{refusal: "test-token entered-credential"}
			}}
			go func() {
				_, result := askSecret(context.Background(), g, "prompt: ", func() ([]byte, error) { return []byte("entered-credential"), nil }, &output, false, "test-token")
				done <- result
			}()
			synctest.Wait()
			time.Sleep(time.Second)
			synctest.Wait()
			if output.String() != "prompt: \n" {
				t.Error("JSON checker emitted progress")
			}
			once.Do(func() { close(gate) })
			synctest.Wait()
			if output.String() != "prompt: \nnot stored: [redacted] [redacted]\n" {
				t.Error("JSON checker refusal was not plain and redacted")
			}
		})
	})
	t.Run("safe registry bytes", func(t *testing.T) {
		var out bytes.Buffer
		if !printSecretSet(&out, []byte(`{"ok":true,"registry":"registry.example","services_error":"offline"}`), "test-token", "entered-credential") || out.String() != "ok: true\nservices not updated: offline\nRun the command again to retry the services that were not updated.\n" {
			t.Error("safe registry transcript changed")
		}
	})
}
