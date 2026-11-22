package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/scope"
)

func TestSetupKeyCopyPassesOneRemoteArgumentAndKeepsBackupOnFailure(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	id := scope.ID{Project: "shop", Environment: "prod"}
	step := setupNextSteps(id, "/srv/keys/my key", "", false, "ops@s1", "T", true, false, "")[0]
	_, command, ok := strings.Cut(step, ": ")
	if !ok {
		t.Fatal(step)
	}
	dir := t.TempDir()
	backup := filepath.Join(dir, "shop.prod.key")
	if err := os.WriteFile(backup, []byte("previous"), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(body string) error {
		c := exec.Command(sh, "-c", "ssh() { printf '%s\\n' \"$@\" > args; "+body+"; }; "+command)
		c.Dir = dir
		return c.Run()
	}
	if err := run("return 1"); err == nil {
		t.Fatal("failure accepted")
	}
	b, _ := os.ReadFile(backup)
	if string(b) != "previous" {
		t.Fatalf("backup changed: %q", b)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if string(args) != "ops@s1\nsudo cat '/srv/keys/my key'\n" {
		t.Fatalf("ssh arguments: %q", args)
	}
	if err := run("printf short"); err == nil {
		t.Fatal("short key accepted")
	}
	b, _ = os.ReadFile(backup)
	if string(b) != "previous" {
		t.Fatalf("short copy changed backup: %q", b)
	}
	if err := run("printf 01234567890123456789012345678901"); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(backup)
	if string(b) != "01234567890123456789012345678901" {
		t.Fatalf("new backup: %q", b)
	}
}

func TestSetupFollowUpsNameProjectAndManifestDirectory(t *testing.T) {
	dir := projectDir(t)
	cfg := filepath.Join(t.TempDir(), "cli.json")
	content := `{"projects":{"shop":{"environments":{"prod":{"url":"http://127.0.0.1:7710","token_env":"SHOP_PROD"}}},"other":{"environments":{"prod":{"url":"http://127.0.0.1:7711","token_env":"OTHER_PROD"}}}}}`
	if err := os.WriteFile(cfg, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	server := installedServer(t, "ops@s1")
	server.config = strings.ReplaceAll(server.config, `"environment": "p5"`, `"environment": "prod"`)
	server.config = strings.ReplaceAll(server.config, "/shop/p5/", "/shop/prod/")
	withSetupRunner(t, server)
	code, out, errout := runSetupCmd("-e", "prod", "--host", "ops@s1", "--manifest-dir", dir, "--config", cfg)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errout)
	}
	if !strings.Contains(out, "berthy secret set NAME -e prod --project shop") || !strings.Contains(out, "berthy deploy -e prod --project shop") || !strings.Contains(out, "--manifest-dir "+shellWord(dir)) {
		t.Fatalf("follow-ups: %s", out)
	}
	steps := setupNextSteps(scope.ID{Project: "sliceup", Environment: "prod"}, "/key", cfg, true, "this server", "TOKEN", true, true, dir)
	for _, step := range steps[2:] {
		if !strings.Contains(step, "--project sliceup") || !strings.Contains(step, "--manifest-dir "+shellWord(dir)) {
			t.Errorf("follow-up lacks project or manifest: %s", step)
		}
	}
}

func TestRebuildExamplesUseTheHostedStateAndSavedKey(t *testing.T) {
	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "user-guide.md"))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := os.ReadFile(filepath.Join("..", "..", "docs", "spec-v1.md"))
	if err != nil {
		t.Fatal(err)
	}
	g := string(guide)
	start := strings.Index(g, "### Step 15. Rebuild a server")
	if start < 0 {
		t.Fatal("rebuild section missing")
	}
	end := strings.Index(g[start+4:], "\n### ")
	if end < 0 {
		t.Fatal("next section missing")
	}
	g = g[start : start+4+end]
	if !strings.Contains(g, "--out /var/lib/berthy/environments/todo/prod/state --key-file ./todo.prod.key") || !strings.Contains(g, "--key-file ./todo.prod.key --config") || strings.Contains(g, "`prod.key`") {
		t.Fatal("guide rebuild uses inconsistent key or state path")
	}
	s := string(spec)
	if !strings.Contains(s, "--out /var/lib/berthy/environments/todo/prod/state --key-file ./todo.prod.key") {
		t.Fatal("spec restore uses wrong state or key")
	}
}
