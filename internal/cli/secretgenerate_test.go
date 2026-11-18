package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// TestSecretGeneratePrintsWhatWasGeneratedAndNoValue checks the request a bare generate sends (the name, and no length or format,
// so the agent's defaults apply) and the line it prints: the name and the length, never a value.
func TestSecretGeneratePrintsWhatWasGeneratedAndNoValue(t *testing.T) {
	var bodies []string
	server := replyingAgent(&bodies, http.StatusOK, `{"name":"GEN_PASS","bytes":32,"format":"hex","replaced":false}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	if code := Run([]string{"secret", "generate", "GEN_PASS", "-e", "dev", "--config", cfg}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if out.String() != "generated GEN_PASS (32 bytes, hex)\n" {
		t.Fatalf("stdout %q", out.String())
	}
	if len(bodies) != 1 || !strings.HasPrefix(bodies[0], "POST ") || !strings.Contains(bodies[0], "/secrets/generate ") || !strings.HasSuffix(bodies[0], `{"name":"GEN_PASS"}`) {
		t.Fatalf("requests %q", bodies)
	}
}

// TestSecretGenerateSendsTheLengthFormatAndReplace checks that the flags reach the agent, and that a replacement and an alnum
// length say so: an alnum secret is counted in characters.
func TestSecretGenerateSendsTheLengthFormatAndReplace(t *testing.T) {
	var bodies []string
	server := replyingAgent(&bodies, http.StatusOK, `{"name":"GEN_PASS","bytes":20,"format":"alnum","replaced":true}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	args := []string{"secret", "generate", "GEN_PASS", "--bytes", "20", "--format", "alnum", "--replace", "-e", "dev", "--config", cfg}
	if code := Run(args, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if out.String() != "replaced GEN_PASS (20 characters, alnum)\n" {
		t.Fatalf("stdout %q", out.String())
	}
	if len(bodies) != 1 || !strings.HasSuffix(bodies[0], `{"name":"GEN_PASS","bytes":20,"format":"alnum","replace":true}`) {
		t.Fatalf("requests %q", bodies)
	}
}

// TestSecretGenerateDryRunSaysWhatWouldHappen checks the dry run: it sends a dry run, prints what a real run would do, and says
// when a real run would be refused for want of --replace.
func TestSecretGenerateDryRunSaysWhatWouldHappen(t *testing.T) {
	for _, tc := range []struct {
		name, reply, flags, want string
	}{
		{"a new secret", `{"action":"secret generate","name":"GEN_PASS","exists":false,"bytes":32,"format":"hex"}`, "",
			"would generate GEN_PASS (32 bytes, hex)\n"},
		{"a stored secret without replace", `{"action":"secret generate","name":"GEN_PASS","exists":true,"bytes":32,"format":"hex"}`, "",
			"GEN_PASS exists: a real run is refused unless --replace is given\n"},
		{"a stored secret with replace", `{"action":"secret generate","name":"GEN_PASS","exists":true,"bytes":32,"format":"hex"}`, "--replace",
			"would replace GEN_PASS (32 bytes, hex)\n"},
	} {
		var bodies []string
		server := replyingAgent(&bodies, http.StatusOK, tc.reply)
		cfg := ingressCLIConfig(t, server.URL)
		var out, errout bytes.Buffer
		args := []string{"secret", "generate", "GEN_PASS", "--dry-run", "-e", "dev", "--config", cfg}
		if tc.flags != "" {
			args = append(args, tc.flags)
		}
		code := Run(args, &out, &errout)
		server.Close()
		if code != 0 || out.String() != tc.want {
			t.Fatalf("%s: exit %d stdout %q stderr %q, want %q", tc.name, code, out.String(), errout.String(), tc.want)
		}
		if len(bodies) != 1 || !strings.Contains(bodies[0], `"dry_run":true`) {
			t.Fatalf("%s: requests %q, want one dry run", tc.name, bodies)
		}
	}
}

// TestSecretGenerateBytesMustBeAWholeNumber checks that a --bytes that is not a number is refused before anything is sent.
func TestSecretGenerateBytesMustBeAWholeNumber(t *testing.T) {
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "generate", "GEN_PASS", "--bytes", "many", "-e", "dev", "--config", cfg}, &out, &errout)
	if code != 2 || strings.TrimSpace(errout.String()) != "invalid --bytes" || calls.Load() != 0 {
		t.Fatalf("exit %d stderr %q, agent calls %d", code, errout.String(), calls.Load())
	}
}

// TestSecretGenerateWithJSONPrintsTheAnswer checks that --json prints the agent's answer as it is.
func TestSecretGenerateWithJSONPrintsTheAnswer(t *testing.T) {
	var bodies []string
	server := replyingAgent(&bodies, http.StatusOK, `{"name":"GEN_PASS","bytes":32,"format":"hex","replaced":false}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	if code := Run([]string{"secret", "generate", "GEN_PASS", "--json", "-e", "dev", "--config", cfg}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	var v map[string]any
	if json.Unmarshal(out.Bytes(), &v) != nil || v["format"] != "hex" || strings.Contains(out.String(), "generated ") {
		t.Fatalf("stdout %q, want the JSON answer", out.String())
	}
}

// TestSecretGenerateHeldForApprovalExitsHeld checks that an agent admin's generate is held: the command exits with the held
// code and prints the plan, which names the secret and no value.
func TestSecretGenerateHeldForApprovalExitsHeld(t *testing.T) {
	var bodies []string
	server := replyingAgent(&bodies, http.StatusAccepted, `{"approval_id":"ap1","expires":"2026-11-18T12:00:00Z","plan":{"action":"secret generate","name":"GEN_PASS","exists":false,"bytes":32,"format":"hex"}}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	if code := Run([]string{"secret", "generate", "GEN_PASS", "-e", "dev", "--config", cfg}, &out, &errout); code != exitHeld {
		t.Fatalf("exit %d, want %d: %s", code, exitHeld, errout.String())
	}
	if !strings.Contains(out.String(), "held for approval: ap1") || !strings.Contains(out.String(), "GEN_PASS") {
		t.Fatalf("stdout %q", out.String())
	}
}

// TestSecretGenerateRefusedByTheAgentShowsItsMessage checks that a refusal, such as a stored secret without --replace, is shown
// with the agent's message and exits 1.
func TestSecretGenerateRefusedByTheAgentShowsItsMessage(t *testing.T) {
	var bodies []string
	server := replyingAgent(&bodies, http.StatusConflict, `{"code":"conflict","message":"secret GEN_PASS exists; --replace replaces it"}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "generate", "GEN_PASS", "-e", "dev", "--config", cfg}, &out, &errout)
	if code != 1 || !strings.Contains(errout.String(), "secret GEN_PASS exists; --replace replaces it") || out.Len() != 0 {
		t.Fatalf("exit %d stdout %q stderr %q", code, out.String(), errout.String())
	}
}
