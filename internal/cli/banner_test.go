package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"berthy/internal/agentclient"
)

// forceStderrTerminal makes stderrTerminal report yes or no for the rest of the test.
func forceStderrTerminal(t *testing.T, yes bool) {
	t.Helper()
	old := stderrTerminal
	t.Cleanup(func() { stderrTerminal = old })
	stderrTerminal = func(io.Writer) bool { return yes }
}

// TestBannerNamesProjectEnvironmentAndAgentBeforeAChange: a mutating command prints the banner naming the resolved
// project, environment and agent URL before any request reaches the agent.
func TestBannerNamesProjectEnvironmentAndAgentBeforeAChange(t *testing.T) {
	var calls atomic.Int32
	var askedAtBanner int32 = -1
	server := countingAgent(&calls)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	forceStderrTerminal(t, true)
	var out, errout bytes.Buffer
	w := writeCatch{Writer: &errout, onWrite: func(p []byte) {
		if strings.Contains(string(p), "berthy:") {
			askedAtBanner = calls.Load()
		}
	}}
	if code := Run([]string{"lock", "acquire", "-m", "banner", "-e", "dev", "--config", cfg}, &out, w); code != 0 && calls.Load() == 0 {
		t.Fatalf("exit %d with no agent call: %s", code, errout.String())
	}
	want := "berthy: demo/dev at " + server.URL
	if !strings.HasPrefix(errout.String(), want+"\n") {
		t.Fatalf("stderr %q, want prefix %q", errout.String(), want)
	}
	if askedAtBanner != 0 {
		t.Fatalf("agent was already called %d times when the banner printed", askedAtBanner)
	}
}

// TestBannerSaysWhenBerthyEnvChoseTheEnvironment: the banner notes when BERTHY_ENV chose the environment.
func TestBannerSaysWhenBerthyEnvChoseTheEnvironment(t *testing.T) {
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	t.Setenv(agentclient.EnvironmentEnv, "dev")
	forceStderrTerminal(t, true)
	var out, errout bytes.Buffer
	Run([]string{"lock", "acquire", "-m", "from-env", "--config", cfg}, &out, &errout)
	want := "berthy: demo/dev at " + server.URL + " (environment from BERTHY_ENV)"
	if !strings.HasPrefix(errout.String(), want+"\n") {
		t.Fatalf("stderr %q, want prefix %q", errout.String(), want)
	}
}

// TestBannerStaysOffStdoutWithJSON: under --json the banner is on stderr only, so stdout stays one JSON object.
func TestBannerStaysOffStdoutWithJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"held":false}`)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	forceStderrTerminal(t, true)
	var out, errout bytes.Buffer
	if code := Run([]string{"lock", "release", "-e", "dev", "--config", cfg, "--json"}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if !strings.HasPrefix(errout.String(), "berthy: demo/dev at "+server.URL+"\n") {
		t.Fatalf("stderr %q", errout.String())
	}
	if strings.Contains(out.String(), "berthy:") {
		t.Fatalf("banner leaked to stdout: %q", out.String())
	}
	var obj map[string]any
	if err := json.Unmarshal(out.Bytes(), &obj); err != nil {
		t.Fatalf("stdout is not one JSON object: %v (%q)", err, out.String())
	}
}

// TestNoBannerWhenStderrIsNotATerminal: redirected stderr prints nothing.
func TestNoBannerWhenStderrIsNotATerminal(t *testing.T) {
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	forceStderrTerminal(t, false)
	var out, errout bytes.Buffer
	Run([]string{"lock", "acquire", "-m", "quiet", "-e", "dev", "--config", cfg}, &out, &errout)
	if strings.Contains(errout.String(), "berthy:") || strings.Contains(out.String(), "berthy:") {
		t.Fatalf("banner appeared without a terminal: out=%q err=%q", out.String(), errout.String())
	}
}

// TestReadOnlyCommandsPrintNoBanner: every command in readOnlyCommands that changeCommands covers prints no banner.
func TestReadOnlyCommandsPrintNoBanner(t *testing.T) {
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := projectDirOf(t, "demo")
	forceStderrTerminal(t, true)
	withStdin(t, "postgres://db\n")
	for _, cc := range changeCommands {
		if !slices.Contains(readOnlyCommands, cc.name) {
			continue
		}
		var out, errout bytes.Buffer
		args := cc.argv(cfg)
		if cc.local {
			args = append(args, "--manifest-dir", dir)
		}
		Run(args, &out, &errout)
		if strings.Contains(errout.String(), "berthy:") {
			t.Errorf("%s printed a banner: %q", cc.name, errout.String())
		}
	}
}

// TestEveryOtherChangeCommandPrintsOne: every changeCommands entry outside readOnlyCommands prints exactly one banner line.
func TestEveryOtherChangeCommandPrintsOne(t *testing.T) {
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	forceStderrTerminal(t, true)
	withStdin(t, "postgres://db\n")
	wantPrefix := "berthy: demo/dev at " + server.URL
	for _, cc := range changeCommands {
		if slices.Contains(readOnlyCommands, cc.name) {
			continue
		}
		var out, errout bytes.Buffer
		Run(cc.argv(cfg), &out, &errout)
		lines := strings.Split(strings.TrimSuffix(errout.String(), "\n"), "\n")
		bannerLines := 0
		for _, line := range lines {
			if strings.HasPrefix(line, "berthy:") {
				bannerLines++
				if line != wantPrefix {
					t.Errorf("%s banner %q, want %q", cc.name, line, wantPrefix)
				}
			}
		}
		if bannerLines != 1 {
			t.Errorf("%s: %d banner lines in %q, want 1", cc.name, bannerLines, errout.String())
		}
	}
}

// TestEveryReadOnlyCommandIsAKeyOfChangeFlags keeps readOnlyCommands in step with changeFlags.
func TestEveryReadOnlyCommandIsAKeyOfChangeFlags(t *testing.T) {
	for _, name := range readOnlyCommands {
		if _, ok := changeFlags[name]; !ok {
			t.Errorf("%q is in readOnlyCommands but not a key of changeFlags", name)
		}
	}
}

// writeCatch calls onWrite for each Write, then forwards to Writer.
type writeCatch struct {
	io.Writer
	onWrite func([]byte)
}

func (w writeCatch) Write(p []byte) (int, error) {
	w.onWrite(p)
	return w.Writer.Write(p)
}

// Ensure *os.File stays the type stderrTerminal recognises in production (compile-time reminder for tests).
var _ = os.Stderr
