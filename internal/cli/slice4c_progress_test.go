package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/credstore/credtest"
	"berthy/internal/manifest"
)

// Start the process signal receiver outside any synctest bubble.
func init() {
	_, stop := contextStoreContext()
	stop()
}

func slice4cTerminal(t *testing.T) {
	t.Helper()
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "1")
	forceStderrTerminal(t, true)
	forceProgressVT(t)
	old := progressWidth
	progressWidth = func(io.Writer) (int, error) { return 100, nil }
	t.Cleanup(func() { progressWidth = old })
}

func TestSecretWaitStartsAfterHiddenRead(t *testing.T) {
	slice4cTerminal(t)
	synctest.Test(t, func(t *testing.T) {
		var errs progressTestBuffer
		input, check := make(chan struct{}), make(chan struct{})
		var inputOnce, checkOnce sync.Once
		done := make(chan askResult, 1)
		defer func() { inputOnce.Do(func() { close(input) }); checkOnce.Do(func() { close(check) }); <-done }()
		g := secretGuide{check: func(ctx context.Context, value string) checkResult { <-check; return checkResult{refusal: "refused"} }}
		go func() {
			_, result := askSecret(context.Background(), g, "prompt: ", func() ([]byte, error) { <-input; return []byte("fixture"), nil }, &errs, true)
			done <- result
		}()
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		if got := errs.String(); got != "prompt: " {
			t.Errorf("hidden input: %q", got)
		}
		inputOnce.Do(func() { close(input) })
		synctest.Wait()
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		if !strings.Contains(errs.String(), "checking secret") {
			t.Errorf("missing checker frame: %q", errs.String())
		}
		checkOnce.Do(func() { close(check) })
		synctest.Wait()
		if !strings.HasSuffix(errs.String(), "\r\x1b[2Knot stored: refused\n") {
			t.Errorf("final diagnostic: %q", errs.String())
		}
	})
}

func TestGuideRequestErrorAfterProgressClear(t *testing.T) {
	slice4cTerminal(t)
	dir := secretManifestDir(t, "project: demo\nnetwork: overlay\napps:\n  api:\n    kind: image\n    image: example.com/api\n    env:\n      secret: [API_KEY]\n    roles:\n      web:\n        port: 8080\n")
	m, err := manifest.Load(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("http://agent.invalid")
	for _, failure := range []string{"connection", "body"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var out, errs progressTestBuffer
				gate := make(chan struct{})
				var once sync.Once
				done := make(chan guideResult, 1)
				defer func() { once.Do(func() { close(gate) }); <-done }()
				client := &http.Client{Transport: progressRoundTrip(func(r *http.Request) (*http.Response, error) {
					if r.Method == http.MethodGet {
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("[]"))}, nil
					}
					if failure == "connection" {
						<-gate
						return nil, errors.New("fixture failure")
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &slice4cFailBody{gate}}, nil
				})}
				go func() {
					_, result := guideSecrets(context.Background(), client, &agentclient.Target{Base: base, Prefix: "/v1/", Token: "fixture-token"}, m, dir, "dev", func() ([]byte, error) { return []byte("fixture-value"), nil }, &out, &errs)
					done <- result
				}()
				synctest.Wait()
				time.Sleep(250 * time.Millisecond)
				synctest.Wait()
				if !strings.Contains(errs.String(), "storing secret") {
					t.Errorf("missing submission frame: %q", errs.String())
				}
				once.Do(func() { close(gate) })
				synctest.Wait()
				diagnostic := "agent connection failed\n"
				if failure == "body" {
					diagnostic = "invalid agent response\n"
				}
				got := errs.String()
				if !strings.HasSuffix(got, "\r\x1b[2K"+diagnostic) || strings.Count(got, diagnostic) != 1 {
					t.Errorf("final diagnostic: %q", got)
				}
				time.Sleep(time.Second)
				synctest.Wait()
				if errs.String() != got {
					t.Error("write after diagnostic")
				}
			})
		})
	}
}

type slice4cFailBody struct{ gate <-chan struct{} }

func (b *slice4cFailBody) Read([]byte) (int, error) {
	<-b.gate
	return 0, errors.New("fixture failure")
}
func (*slice4cFailBody) Close() error { return nil }

func TestContextListShowsSlowStoreWait(t *testing.T) {
	slice4cTerminal(t)
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, []byte(`{"projects":{"demo":{"environments":{"dev":{"url":"http://localhost","token_env":"SLICE4C_TOKEN"}}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SLICE4C_TOKEN", "")
	s := credtest.NewMemory()
	entry := agentclient.Environment{URL: "http://localhost", TokenEnv: "SLICE4C_TOKEN"}
	if err := s.Set(agentclient.StoreKey("demo", "dev", entry), "fixture"); err != nil {
		t.Fatal(err)
	}
	oldStore, oldWait := agentclient.TokenStore, agentclient.StoreWait
	agentclient.TokenStore, agentclient.StoreWait = s, 5*time.Second
	t.Cleanup(func() { agentclient.TokenStore, agentclient.StoreWait = oldStore, oldWait })
	synctest.Test(t, func(t *testing.T) {
		var out, errs progressTestBuffer
		gate, entered := make(chan struct{}), make(chan struct{}, 1)
		var once sync.Once
		s.Block = gate
		s.OnCall = func() { entered <- struct{}{} }
		done := make(chan int, 1)
		returned := false
		defer func() {
			once.Do(func() { close(gate) })
			if !returned {
				<-done
			}
			s.Wait()
			s.Block = nil
			s.OnCall = nil
		}()
		go func() { done <- runContextList([]string{"--config", cfg}, &out, &errs) }()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("credential lookup did not enter the store")
		}
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		if !strings.Contains(errs.String(), "checking credentials") {
			t.Errorf("missing store frame: %q", errs.String())
		}
		once.Do(func() { close(gate) })
		code := <-done
		returned = true
		if code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
		if got, want := out.String(), "demo  dev  http://localhost  SLICE4C_TOKEN  no  stored\n"; got != want {
			t.Errorf("row = %q, want %q", got, want)
		}
		if !strings.HasSuffix(errs.String(), "\r\x1b[2K") {
			t.Errorf("missing final clear: %q", errs.String())
		}
	})
}

func TestContextListWaitModes(t *testing.T) {
	slice4cTerminal(t)
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, []byte(`{"projects":{"demo":{"environments":{"dev":{"url":"http://localhost","token_env":"SLICE4C_TOKEN"}}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SLICE4C_TOKEN", "")
	for _, mode := range []string{"quick", "timeout", "json"} {
		t.Run(mode, func(t *testing.T) {
			s := credtest.NewMemory()
			oldStore, oldWait := agentclient.TokenStore, agentclient.StoreWait
			agentclient.TokenStore, agentclient.StoreWait = s, time.Second
			defer func() { agentclient.TokenStore, agentclient.StoreWait = oldStore, oldWait }()
			synctest.Test(t, func(t *testing.T) {
				var out, errs progressTestBuffer
				gate := make(chan struct{})
				var once sync.Once
				if mode != "quick" {
					s.Block = gate
				}
				done := make(chan int, 1)
				returned := false
				defer func() {
					once.Do(func() { close(gate) })
					if !returned {
						<-done
					}
					s.Wait()
				}()
				args := []string{"--config", cfg}
				if mode == "json" {
					args = append(args, "--json")
				}
				go func() { done <- runContextList(args, &out, &errs) }()
				synctest.Wait()
				if mode != "quick" {
					time.Sleep(250 * time.Millisecond)
					synctest.Wait()
				}
				if mode == "timeout" {
					if !strings.Contains(errs.String(), "checking credentials") {
						t.Errorf("missing frame: %q", errs.String())
					}
					time.Sleep(time.Second)
					synctest.Wait()
				}
				if mode == "json" {
					if errs.String() != "" {
						t.Errorf("JSON wait: %q", errs.String())
					}
					once.Do(func() { close(gate) })
				}
				if got := <-done; got != 0 {
					t.Errorf("exit %d", got)
				}
				returned = true
				if mode == "quick" && errs.String() != "" {
					t.Errorf("quick wait: %q", errs.String())
				}
				if mode == "timeout" && (!strings.HasSuffix(errs.String(), "\r\x1b[2K") || !strings.Contains(out.String(), "unknown")) {
					t.Errorf("timeout stdout %q stderr %q", out.String(), errs.String())
				}
				if mode == "json" && (strings.Contains(out.String(), "\x1b") || !strings.Contains(out.String(), `"token_stored": false`)) {
					t.Errorf("JSON output: %q", out.String())
				}
			})
		})
	}
}

func TestContextCredentialWritesClearBeforeResult(t *testing.T) {
	slice4cTerminal(t)
	oldHidden := hiddenInput
	hiddenInput = func() (func() ([]byte, error), bool) {
		return func() ([]byte, error) { return []byte("fixture-value"), nil }, true
	}
	defer func() { hiddenInput = oldHidden }()
	for _, mode := range []string{"set", "delete"} {
		t.Run(mode, func(t *testing.T) {
			s := credtest.NewMemory()
			entry := agentclient.Environment{URL: "http://localhost", TokenEnv: "SLICE4C_TOKEN"}
			if mode == "delete" {
				if err := s.Set(agentclient.StoreKey("demo", "dev", entry), "fixture-value"); err != nil {
					t.Fatal(err)
				}
			}
			cfg := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(cfg, []byte(`{"projects":{"demo":{"environments":{"dev":{"url":"http://localhost","token_env":"SLICE4C_TOKEN"}}}}}`), 0600); err != nil {
				t.Fatal(err)
			}
			oldStore, oldWait := agentclient.TokenStore, agentclient.StoreWait
			agentclient.TokenStore, agentclient.StoreWait = s, 5*time.Second
			defer func() { agentclient.TokenStore, agentclient.StoreWait = oldStore, oldWait }()
			synctest.Test(t, func(t *testing.T) {
				var combined progressTestBuffer
				gate, entered := make(chan struct{}), make(chan struct{}, 1)
				var once sync.Once
				s.Block = gate
				s.OnCall = func() { entered <- struct{}{} }
				done := make(chan int, 1)
				returned := false
				defer func() {
					once.Do(func() { close(gate) })
					if !returned {
						<-done
					}
					s.Wait()
				}()
				go func() {
					if mode == "set" {
						done <- saveContextToken("demo", "dev", entry, &combined, &combined)
					} else {
						done <- runContextRemove([]string{"--project", "demo", "-e", "dev", "--config", cfg, "--delete-token"}, &combined, &combined)
					}
				}()
				synctest.Wait()
				select {
				case <-entered:
				default:
					t.Fatal("credential write did not enter the store")
				}
				time.Sleep(250 * time.Millisecond)
				synctest.Wait()
				label := "saving credential"
				if mode == "delete" {
					label = "deleting credential"
				}
				if !strings.Contains(combined.String(), label) {
					t.Errorf("missing frame: %q", combined.String())
				}
				once.Do(func() { close(gate) })
				if got := <-done; got != 0 {
					t.Errorf("exit %d", got)
				}
				returned = true
				result := "saved the token of demo/dev"
				if mode == "delete" {
					result = "deleted the token of demo/dev"
				}
				if got := combined.String(); !strings.Contains(got, "\r\x1b[2K"+result) || strings.Contains(got, "fixture-value") {
					t.Errorf("combined output %q", got)
				}
			})
		})
	}
}

func TestContextStoreInterruptClearsAndStops(t *testing.T) {
	slice4cTerminal(t)
	oldHidden, oldContext := hiddenInput, contextStoreContext
	hiddenInput = func() (func() ([]byte, error), bool) {
		return func() ([]byte, error) { return []byte("fixture-value"), nil }, true
	}
	defer func() { hiddenInput, contextStoreContext = oldHidden, oldContext }()
	for _, mode := range []string{"add", "list", "remove"} {
		t.Run(mode, func(t *testing.T) {
			s := credtest.NewMemory()
			entry := agentclient.Environment{URL: "http://localhost", TokenEnv: "SLICE4C_TOKEN"}
			if err := s.Set(agentclient.StoreKey("demo", "dev", entry), "fixture-value"); err != nil {
				t.Fatal(err)
			}
			cfg := filepath.Join(t.TempDir(), "config.json")
			original := []byte(`{"projects":{"demo":{"environments":{"dev":{"url":"http://localhost","token_env":"SLICE4C_TOKEN"}}}}}`)
			if mode != "add" {
				if err := os.WriteFile(cfg, original, 0600); err != nil {
					t.Fatal(err)
				}
			}
			oldStore, oldWait := agentclient.TokenStore, agentclient.StoreWait
			agentclient.TokenStore, agentclient.StoreWait = s, 5*time.Second
			defer func() { agentclient.TokenStore, agentclient.StoreWait = oldStore, oldWait }()
			synctest.Test(t, func(t *testing.T) {
				var out, errs progressTestBuffer
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				contextStoreContext = func() (context.Context, func() bool) { return ctx, func() bool { return ctx.Err() != nil } }
				gate, entered := make(chan struct{}), make(chan struct{}, 1)
				var once sync.Once
				s.Block = gate
				s.OnCall = func() { entered <- struct{}{} }
				done := make(chan int, 1)
				returned := false
				defer func() {
					cancel()
					once.Do(func() { close(gate) })
					if !returned {
						<-done
					}
					s.Wait()
				}()
				go func() {
					switch mode {
					case "add":
						done <- runContextAdd([]string{"--project", "demo", "-e", "dev", "--url", "http://localhost", "--token-env", "SLICE4C_TOKEN", "--config", cfg, "--save-token"}, &out, &errs)
					case "list":
						done <- runContextList([]string{"--config", cfg}, &out, &errs)
					case "remove":
						done <- runContextRemove([]string{"--project", "demo", "-e", "dev", "--config", cfg, "--delete-token"}, &out, &errs)
					}
				}()
				synctest.Wait()
				select {
				case <-entered:
				default:
					t.Fatal("store call not entered")
				}
				time.Sleep(250 * time.Millisecond)
				synctest.Wait()
				if !strings.Contains(errs.String(), "credential") {
					t.Errorf("no frame: %q", errs.String())
				}
				cancel()
				synctest.Wait()
				select {
				case code := <-done:
					returned = true
					if code != 130 {
						t.Errorf("exit = %d, want 130", code)
					}
				default:
					t.Fatal("interrupted command did not return")
				}
				if !strings.HasSuffix(errs.String(), "\r\x1b[2K") {
					t.Errorf("missing final clear: %q", errs.String())
				}
				if strings.Contains(out.String(), "saved the token") || strings.Contains(out.String(), "deleted the token") || (mode == "list" && out.String() != "") {
					t.Errorf("unexpected success: %q", out.String())
				}
				data, err := os.ReadFile(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "remove" && string(data) != string(original) {
					t.Error("interrupted removal changed config")
				}
				if mode == "add" && !strings.Contains(string(data), "demo") {
					t.Error("added config entry lost")
				}
			})
		})
	}
}

func TestSecretCheckerParentCancellationClearsWait(t *testing.T) {
	slice4cTerminal(t)
	synctest.Test(t, func(t *testing.T) {
		var errs progressTestBuffer
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan askResult, 1)
		release := make(chan struct{})
		returned := false
		defer func() {
			close(release)
			if !returned {
				<-done
			}
		}()
		g := secretGuide{check: func(ctx context.Context, _ string) checkResult {
			select {
			case <-ctx.Done():
			case <-release:
			}
			return checkResult{refusal: "cancelled"}
		}}
		go func() {
			_, res := askSecret(ctx, g, "prompt: ", func() ([]byte, error) { return []byte("fixture-value"), nil }, &errs, true)
			done <- res
		}()
		synctest.Wait()
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		if !strings.Contains(errs.String(), "checking secret") {
			t.Errorf("missing frame: %q", errs.String())
		}
		cancel()
		synctest.Wait()
		select {
		case got := <-done:
			returned = true
			if got != askRefused {
				t.Errorf("result %v", got)
			}
		default:
			t.Fatal("checker did not observe parent cancellation")
		}
		if !strings.HasSuffix(errs.String(), "\r\x1b[2Knot stored: cancelled\n") {
			t.Errorf("clear and diagnostic: %q", errs.String())
		}
	})
}

func TestContextStoreStopsBeforeResult(t *testing.T) {
	oldContext, oldHidden, oldStore := contextStoreContext, hiddenInput, agentclient.TokenStore
	defer func() { contextStoreContext, hiddenInput, agentclient.TokenStore = oldContext, oldHidden, oldStore }()
	hiddenInput = func() (func() ([]byte, error), bool) {
		return func() ([]byte, error) { return []byte("fixture"), nil }, true
	}
	for _, mode := range []string{"set", "list", "remove"} {
		t.Run(mode, func(t *testing.T) {
			cfg := filepath.Join(t.TempDir(), "config.json")
			original := []byte(`{"projects":{"demo":{"environments":{"dev":{"url":"http://localhost","token_env":"SLICE4C_TOKEN"}}}}}`)
			if err := os.WriteFile(cfg, original, 0600); err != nil {
				t.Fatal(err)
			}
			s := credtest.NewMemory()
			entry := agentclient.Environment{URL: "http://localhost", TokenEnv: "SLICE4C_TOKEN"}
			if err := s.Set(agentclient.StoreKey("demo", "dev", entry), "fixture"); err != nil {
				t.Fatal(err)
			}
			agentclient.TokenStore = s
			stopped := false
			contextStoreContext = func() (context.Context, func() bool) {
				ctx, cancel := context.WithCancel(context.Background())
				var once sync.Once
				return ctx, func() bool {
					once.Do(func() {
						if mode == "remove" {
							data, err := os.ReadFile(cfg)
							if err != nil || string(data) != string(original) {
								t.Error("config changed before signal teardown")
							}
						}
						stopped = true
						cancel()
					})
					return false
				}
			}
			var output, errs progressTestBuffer
			out := promotionTestWriter{write: func(p []byte) (int, error) {
				if !stopped {
					t.Error("result written before signal teardown")
				}
				return output.Write(p)
			}}
			var code int
			switch mode {
			case "set":
				code = saveContextToken("demo", "dev", entry, out, &errs)
			case "list":
				code = runContextList([]string{"--config", cfg}, out, &errs)
			case "remove":
				code = runContextRemove([]string{"--project", "demo", "-e", "dev", "--config", cfg, "--delete-token"}, out, &errs)
			}
			if code != 0 || !stopped || output.Len() == 0 {
				t.Errorf("exit %d stopped %v output %q stderr %q", code, stopped, output.String(), errs.String())
			}
		})
	}
}

func TestContextInterruptDuringFinalClear(t *testing.T) {
	slice4cTerminal(t)
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, []byte(`{"projects":{"demo":{"environments":{"dev":{"url":"http://localhost","token_env":"SLICE4C_TOKEN"}}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	oldContext, oldStore, oldWait := contextStoreContext, agentclient.TokenStore, agentclient.StoreWait
	s := credtest.NewMemory()
	agentclient.TokenStore, agentclient.StoreWait = s, 5*time.Second
	defer func() {
		contextStoreContext, agentclient.TokenStore, agentclient.StoreWait = oldContext, oldStore, oldWait
	}()
	synctest.Test(t, func(t *testing.T) {
		var out, errs progressTestBuffer
		ctx, cancel := context.WithCancel(context.Background())
		contextStoreContext = func() (context.Context, func() bool) { return ctx, func() bool { return ctx.Err() != nil } }
		gate, clearGate, clearEntered := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
		var storeOnce, clearOnce sync.Once
		s.Block = gate
		done := make(chan int, 1)
		returned := false
		defer func() {
			cancel()
			storeOnce.Do(func() { close(gate) })
			clearOnce.Do(func() { close(clearGate) })
			if !returned {
				<-done
			}
			s.Wait()
		}()
		writer := promotionTestWriter{write: func(p []byte) (int, error) {
			if string(p) == "\r\x1b[2K" {
				clearEntered <- struct{}{}
				<-clearGate
			}
			return errs.Write(p)
		}}
		go func() { done <- runContextList([]string{"--config", cfg}, &out, writer) }()
		synctest.Wait()
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		if !strings.Contains(errs.String(), "checking credentials") {
			t.Fatalf("missing frame: %q", errs.String())
		}
		storeOnce.Do(func() { close(gate) })
		synctest.Wait()
		select {
		case <-clearEntered:
		default:
			t.Fatal("final clear not entered")
		}
		cancel()
		clearOnce.Do(func() { close(clearGate) })
		if code := <-done; code != 130 {
			t.Errorf("exit %d, want 130", code)
		}
		returned = true
		if !strings.HasSuffix(errs.String(), "\r\x1b[2K") || out.String() != "" {
			t.Errorf("stdout %q stderr %q", out.String(), errs.String())
		}
	})
}
