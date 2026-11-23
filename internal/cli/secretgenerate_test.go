package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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

func TestSecretGenerateByNameDryRunPrintsTheFinding(t *testing.T) {
	var out, errout bytes.Buffer
	data := []byte(`{"action":"secret generate","name":"GEN_PASS","exists":false,"bytes":32,"format":"hex","app":"db","volume":"shared"}`)
	if code := printSecretGenerate(&out, &errout, data, true, false); code != 0 || out.String() != "GEN_PASS is not stored, but the volume shared of app db exists: a real run is refused unless --replace is given\n" {
		t.Fatalf("exit %d stdout %q stderr %q", code, out.String(), errout.String())
	}
}

func TestSecretGenerateByNameSendsTheManifestWhenItHasAReader(t *testing.T) {
	var bodies []string
	server := replyingAgent(&bodies, http.StatusOK, `{"name":"GEN_PASS","bytes":32,"format":"hex","replaced":false}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := t.TempDir()
	content := "project: demo\nnetwork: overlay\napps:\n  db:\n    kind: compose\n    file: compose.yml\n    env:\n      secret: [GEN_PASS]\n"
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "generate", "GEN_PASS", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if code != 0 || len(bodies) != 1 || !strings.Contains(bodies[0], `"manifest":`) || !strings.Contains(bodies[0], `"compose_files":{"db":`) {
		t.Fatalf("exit %d requests %v stderr %s", code, bodies, errout.String())
	}
}

func TestSecretGenerateByNameSendsManifestWithMissingComposeFile(t *testing.T) {
	var bodies []string
	server := replyingAgent(&bodies, http.StatusConflict, `{"code":"conflict","message":"refused"}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := t.TempDir()
	content := "project: demo\nnetwork: overlay\napps:\n  db:\n    kind: compose\n    file: missing.yml\n    env:\n      secret: [GEN_PASS]\n  cache:\n    kind: compose\n    file: compose.yml\n    env:\n      secret: [GEN_PASS]\n"
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "generate", "GEN_PASS", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if code != 1 || len(bodies) != 1 || !strings.Contains(bodies[0], `"manifest":`) || !strings.Contains(bodies[0], `"compose_files":{"cache":"services: {}\n"}`) || !strings.Contains(errout.String(), "refused") {
		t.Fatalf("exit %d requests %v stderr %q", code, bodies, errout.String())
	}
}

func TestSecretGenerateReplaceSkipsMissingComposeFile(t *testing.T) {
	var bodies []string
	server := replyingAgent(&bodies, http.StatusOK, `{"name":"GEN_PASS","bytes":32,"format":"hex","replaced":true}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := t.TempDir()
	content := "project: demo\nnetwork: overlay\napps:\n  db:\n    kind: compose\n    file: missing.yml\n    env:\n      secret: [GEN_PASS]\n"
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "generate", "GEN_PASS", "--replace", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if code != 0 || len(bodies) != 1 || !strings.HasSuffix(bodies[0], `{"name":"GEN_PASS","replace":true}`) {
		t.Fatalf("exit %d requests %v stderr %q", code, bodies, errout.String())
	}
}
