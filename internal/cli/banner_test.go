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

// expectedReadOnlyCommands is the approved set that must print no banner. It is independent of production's
// readOnlyCommands so a mistaken edit there cannot hide a gap.
var expectedReadOnlyCommands = []string{
	"versions", "wait", "history", "diagnostics", "audit", "lock status", "ingress status", "secret list",
	"job logs", "backup list", "approvals", "token list", "config show", "config validate",
}

// bannerTokenCommands are the token subcommands, which changeCommands does not list. token list is read-only; create and
// revoke change things.
var bannerTokenCommands = []changeCommand{
	{name: "token list", pos: []string{"token", "list"}},
	{name: "token create", pos: []string{"token", "create", "ci"}, fixed: []string{"--scope", "deploy"}},
	{name: "token revoke", pos: []string{"token", "revoke", "ci"}},
}

// bannerCoverageCommands is every change command the banner tests exercise, including the token ones.
func bannerCoverageCommands() []changeCommand {
	return append(slices.Clone(changeCommands), bannerTokenCommands...)
}

// forceStderrTerminal makes stderrTerminal report yes or no for the rest of the test.
func forceStderrTerminal(t *testing.T, yes bool) {
	t.Helper()
	old := stderrTerminal
	t.Cleanup(func() { stderrTerminal = old })
	stderrTerminal = func(io.Writer) bool { return yes }
}

// TestBannerNamesProjectEnvironmentAndAgentBeforeAChange: a mutating dry-run prints the banner before any request, then
// exits 0 with exactly one lock/acquire call.
func TestBannerNamesProjectEnvironmentAndAgentBeforeAChange(t *testing.T) {
	var calls atomic.Int32
	var askedAtBanner int32 = -1
	var route string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		route = r.Method + " " + r.URL.Path
		fmt.Fprint(w, `{"action":"lock acquire","message":"banner","currently_locked":false}`)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	forceStderrTerminal(t, true)
	var out, errout bytes.Buffer
	w := writeCatch{Writer: &errout, onWrite: func(p []byte) {
		if strings.Contains(string(p), "berthy:") {
			askedAtBanner = calls.Load()
		}
	}}
	code := Run([]string{"lock", "acquire", "-m", "banner", "--dry-run", "-e", "dev", "--config", cfg}, &out, w)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	want := "berthy: demo/dev at " + server.URL
	if !strings.HasPrefix(errout.String(), want+"\n") {
		t.Fatalf("stderr %q, want prefix %q", errout.String(), want)
	}
	if askedAtBanner != 0 {
		t.Fatalf("agent was already called %d times when the banner printed", askedAtBanner)
	}
	if n := calls.Load(); n != 1 || route != "POST /v1/projects/demo/environments/dev/lock/acquire" {
		t.Fatalf("after the run: %d calls, route %q", n, route)
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

// TestReadOnlyCommandsPrintNoBanner: every approved read-only command prints no banner.
func TestReadOnlyCommandsPrintNoBanner(t *testing.T) {
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := projectDirOf(t, "demo")
	forceStderrTerminal(t, true)
	withStdin(t, "postgres://db\n")
	byName := map[string]changeCommand{}
	for _, cc := range bannerCoverageCommands() {
		byName[cc.name] = cc
	}
	for _, name := range expectedReadOnlyCommands {
		cc, ok := byName[name]
		if !ok {
			t.Fatalf("%s is expected read-only but has no banner coverage command", name)
		}
		var out, errout bytes.Buffer
		args := cc.argv(cfg)
		if cc.local {
			args = append(args, "--manifest-dir", dir)
		}
		Run(args, &out, &errout)
		if strings.Contains(errout.String(), "berthy:") {
			t.Errorf("%s printed a banner: %q", name, errout.String())
		}
	}
}

// TestEveryOtherChangeCommandPrintsOne: every covered change command outside the approved read-only set prints one banner.
func TestEveryOtherChangeCommandPrintsOne(t *testing.T) {
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	forceStderrTerminal(t, true)
	withStdin(t, "postgres://db\n")
	wantPrefix := "berthy: demo/dev at " + server.URL
	for _, cc := range bannerCoverageCommands() {
		if slices.Contains(expectedReadOnlyCommands, cc.name) {
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

// TestEveryReadOnlyCommandIsAKeyOfChangeFlags keeps the approved read-only set in step with changeFlags, and pins
// production's list to the same expectation.
func TestEveryReadOnlyCommandIsAKeyOfChangeFlags(t *testing.T) {
	for _, name := range expectedReadOnlyCommands {
		if _, ok := changeFlags[name]; !ok {
			t.Errorf("%q is expected read-only but not a key of changeFlags", name)
		}
	}
	if !slices.Equal(readOnlyCommands, expectedReadOnlyCommands) {
		t.Errorf("production readOnlyCommands %v, want %v", readOnlyCommands, expectedReadOnlyCommands)
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
