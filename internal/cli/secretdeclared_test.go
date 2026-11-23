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

// declaredManifest is a project with an image app, a Compose reader of DB_PASS, and a Compose app that reads no declared secret.
const declaredManifest = `project: demo
network: overlay
apps:
  web:
    kind: image
    image: example.com/web
    roles:
      main:
        health:
          test: ["CMD", "true"]
    env:
      secret: [GEN_PASS]
  db:
    kind: compose
    file: compose.yml
    env:
      secret: [DB_PASS]
  other:
    kind: compose
    file: other.yml
    env:
      secret: [OTHER]
secrets:
  GEN_PASS:
    generate:
      format: hex
  DB_PASS:
    generate:
      format: hex
`

const declaredReply = `{"action":"secret generate declared","secrets":[{"name":"DB_PASS","outcome":"generate","bytes":32,"format":"hex"},{"name":"GEN_PASS","outcome":"generate","bytes":32,"format":"hex"}]}`

func declaredManifestDir(t *testing.T) string {
	t.Helper()
	dir := secretManifestDir(t, declaredManifest)
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services: {db: {image: x}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other.yml"), []byte("services: {x: {image: y}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSecretGenerateWithoutANameSendsTheManifestAndTheComposeFiles(t *testing.T) {
	var bodies []string
	server := answeringAgent(&bodies, `[]`, declaredReply)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := declaredManifestDir(t)
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "generate", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	changes := secretChanges(bodies)
	if len(changes) != 1 || !strings.Contains(changes[0], "POST ") || !strings.Contains(changes[0], "/secrets/generate-declared ") {
		t.Fatalf("requests %q", bodies)
	}
	if !strings.Contains(changes[0], `"compose_files":{"db":"services: {db: {image: x}}\n"}`) {
		t.Fatalf("body must send only the Compose reader's file: %s", changes[0])
	}
	if !strings.Contains(changes[0], `"manifest":`) || !strings.Contains(changes[0], `"GEN_PASS"`) {
		t.Fatalf("body missing manifest: %s", changes[0])
	}
}

func TestSecretGenerateWithoutANamePrintsEachOutcome(t *testing.T) {
	reply := `{"action":"secret generate declared","secrets":[` +
		`{"name":"A","outcome":"generate","bytes":32,"format":"hex"},` +
		`{"name":"B","outcome":"keep","bytes":32,"format":"hex"},` +
		`{"name":"C","outcome":"refuse","bytes":32,"format":"hex","app":"db"},` +
		`{"name":"D","outcome":"refuse","bytes":32,"format":"hex","app":"cache","volume":"ext"}` +
		`]}`
	var bodies []string
	server := answeringAgent(&bodies, `[]`, reply)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := declaredManifestDir(t)
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "generate", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	want := "generated A (32 bytes, hex)\n" +
		"kept B: it is already stored\n" +
		"not generated C: app db was deployed here and may hold the old value; set the current one with berthy secret set C\n" +
		"not generated D: the volume ext of app cache exists and may hold the old value; set the current one with berthy secret set D\n"
	if code != 1 || out.String() != want {
		t.Fatalf("exit %d stdout %q stderr %q, want exit 1 and %q", code, out.String(), errout.String(), want)
	}
}

func TestSecretGenerateWithoutANameExitsOneWhenOneIsRefused(t *testing.T) {
	reply := `{"action":"secret generate declared","secrets":[{"name":"DB_PASS","outcome":"refuse","bytes":32,"format":"hex","app":"db"},{"name":"GEN_PASS","outcome":"generate","bytes":32,"format":"hex"}]}`
	var bodies []string
	server := answeringAgent(&bodies, `[]`, reply)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := declaredManifestDir(t)
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "generate", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if code != 1 || !strings.Contains(out.String(), "not generated DB_PASS") || !strings.Contains(out.String(), "generated GEN_PASS") {
		t.Fatalf("exit %d stdout %q stderr %q", code, out.String(), errout.String())
	}
}

func TestSecretGenerateWithoutANameDryRun(t *testing.T) {
	reply := `{"action":"secret generate declared","secrets":[` +
		`{"name":"A","outcome":"generate","bytes":32,"format":"hex"},` +
		`{"name":"B","outcome":"keep","bytes":32,"format":"hex"},` +
		`{"name":"C","outcome":"refuse","bytes":32,"format":"hex","app":"db"},` +
		`{"name":"D","outcome":"refuse","bytes":32,"format":"hex","app":"cache","volume":"ext"}` +
		`]}`
	var bodies []string
	server := answeringAgent(&bodies, `[]`, reply)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := declaredManifestDir(t)
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "generate", "--dry-run", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	want := "would generate A (32 bytes, hex)\n" +
		"would keep B: it is already stored\n" +
		"would not generate C: app db was deployed here and may hold the old value\n" +
		"would not generate D: the volume ext of app cache exists and may hold the old value\n"
	if code != 0 || out.String() != want {
		t.Fatalf("exit %d stdout %q stderr %q, want exit 0 and %q", code, out.String(), errout.String(), want)
	}
	if len(secretChanges(bodies)) != 1 || !strings.Contains(secretChanges(bodies)[0], `"dry_run":true`) {
		t.Fatalf("requests %q", bodies)
	}
}

func TestSecretGenerateWithoutANameOnAnOlderAgentSaysToUpgrade(t *testing.T) {
	var bodies []string
	server := replyingAgent(&bodies, http.StatusMethodNotAllowed, `{"code":"method_not_allowed","message":"method not allowed"}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := declaredManifestDir(t)
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "generate", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if code != 1 || out.Len() != 0 || strings.TrimSpace(errout.String()) != "the agent does not know this command: upgrade it with berthy setup" {
		t.Fatalf("exit %d stdout %q stderr %q", code, out.String(), errout.String())
	}
}

func TestSecretGenerateWithoutANameHeldExitsHeld(t *testing.T) {
	var bodies []string
	server := replyingAgent(&bodies, http.StatusAccepted, `{"approval_id":"ap1","expires":"2026-11-18T12:00:00Z","plan":{"action":"secret generate declared","secrets":[{"name":"GEN_PASS","outcome":"generate","bytes":32,"format":"hex"}]}}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := declaredManifestDir(t)
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "generate", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if code != exitHeld {
		t.Fatalf("exit %d, want %d: %s", code, exitHeld, errout.String())
	}
	if !strings.Contains(out.String(), "held for approval: ap1") || !strings.Contains(out.String(), "GEN_PASS") {
		t.Fatalf("stdout %q", out.String())
	}
}

func TestSecretGenerateWithoutANameRefusedByTheAgentExitsOne(t *testing.T) {
	var bodies []string
	server := replyingAgent(&bodies, http.StatusBadRequest, `{"code":"bad_request","message":"the state of app cache cannot be established, so nothing is generated"}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := declaredManifestDir(t)
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "generate", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if code != 1 || out.Len() != 0 || !strings.Contains(errout.String(), "the state of app cache cannot be established") {
		t.Fatalf("exit %d stdout %q stderr %q", code, out.String(), errout.String())
	}
}

func TestSecretGenerateWithoutANameAndNothingDeclaredSendsNothing(t *testing.T) {
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := secretManifestDir(t, "project: demo\nnetwork: overlay\napps:\n  web:\n    kind: image\n    image: example.com/web\n    roles:\n      main:\n        health:\n          test: [\"CMD\", \"true\"]\n")
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "generate", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if code != 0 || out.String() != "the manifest declares no generated secret\n" || calls.Load() != 0 {
		t.Fatalf("exit %d stdout %q stderr %q calls %d", code, out.String(), errout.String(), calls.Load())
	}
}

func TestSecretGenerateWithoutANameJSONExitsOneWhenOneIsRefused(t *testing.T) {
	reply := `{"action":"secret generate declared","secrets":[{"name":"DB_PASS","outcome":"refuse","bytes":32,"format":"hex","app":"db"},{"name":"GEN_PASS","outcome":"generate","bytes":32,"format":"hex"}]}`
	var bodies []string
	server := answeringAgent(&bodies, `[]`, reply)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := declaredManifestDir(t)
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "generate", "--json", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if code != 1 {
		t.Fatalf("exit %d, want 1: stderr %q", code, errout.String())
	}
	var v struct {
		Secrets []struct {
			Name, Outcome string
		} `json:"secrets"`
	}
	if json.Unmarshal(out.Bytes(), &v) != nil || len(v.Secrets) != 2 {
		t.Fatalf("stdout %q", out.String())
	}
}

func TestSecretGenerateWithoutANameAndNothingDeclaredPrintsJSON(t *testing.T) {
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := secretManifestDir(t, "project: demo\nnetwork: overlay\napps:\n  web:\n    kind: image\n    image: example.com/web\n    roles:\n      main:\n        health:\n          test: [\"CMD\", \"true\"]\n")
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "generate", "--json", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if code != 0 || calls.Load() != 0 {
		t.Fatalf("exit %d stderr %q calls %d", code, errout.String(), calls.Load())
	}
	var v struct {
		Action  string `json:"action"`
		Secrets []any  `json:"secrets"`
	}
	if json.Unmarshal(out.Bytes(), &v) != nil || v.Action != "secret generate declared" || v.Secrets == nil || len(v.Secrets) != 0 {
		t.Fatalf("stdout %q", out.String())
	}
	if strings.Contains(out.String(), "the manifest declares no generated secret") {
		t.Fatalf("json printed the sentence: %q", out.String())
	}
}
