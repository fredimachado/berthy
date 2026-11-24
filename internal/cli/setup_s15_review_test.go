package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"berthy/internal/scope"
)

func TestSetupKeyCopyPassesOneRemoteArgumentAndKeepsBackupOnFailure(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh is not available: %v", err)
	}
	old := keyCopyNow
	keyCopyNow = func() time.Time { return time.Date(2026, 11, 22, 15, 30, 12, 0, time.UTC) }
	t.Cleanup(func() { keyCopyNow = old })
	id := scope.ID{Project: "shop", Environment: "prod"}
	step := setupNextSteps(nextSteps{id: id, keyFile: "/srv/keys/my key", config: "", local: false, target: "ops@s1", tokenEnv: "T", storeToken: true, ingress: false, manifestDir: ""})[1]
	_, command, ok := strings.Cut(step, ": ")
	if !ok {
		t.Fatal(step)
	}
	dir := t.TempDir()
	backup := filepath.Join(dir, "shop.prod.key.20261122T153012Z")
	run := func() error {
		c := exec.Command(sh, "-c", "ssh() { printf '%s\\n' \"$@\" > args; printf 01234567890123456789012345678901; }; "+command)
		c.Dir = dir
		return c.Run()
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil || string(args) != "ops@s1\nsudo cat '/srv/keys/my key'\n" {
		t.Fatalf("ssh arguments: %q %v", args, err)
	}
	b, err := os.ReadFile(backup)
	if err != nil || string(b) != "01234567890123456789012345678901" {
		t.Fatalf("backup: %q %v", b, err)
	}
	if err := run(); err == nil {
		t.Fatal("overwrote existing backup")
	}
	after, _ := os.ReadFile(backup)
	if string(after) != string(b) {
		t.Fatalf("backup changed: %q", after)
	}
}

func twoProjectsSharingProdConfig(t *testing.T) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "cli.json")
	content := `{"projects":{"shop":{"environments":{"prod":{"url":"http://127.0.0.1:7710","token_env":"SHOP_PROD"}}},"other":{"environments":{"prod":{"url":"http://127.0.0.1:7711","token_env":"OTHER_PROD"}}}}}`
	if err := os.WriteFile(cfg, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func installedShopProd(t *testing.T) *setupServer {
	t.Helper()
	server := installedServer(t, "ops@s1")
	server.config = strings.ReplaceAll(server.config, `"environment": "p5"`, `"environment": "prod"`)
	server.config = strings.ReplaceAll(server.config, "/shop/p5/", "/shop/prod/")
	return server
}

func TestSetupFollowUpsLeaveOutTheProjectTheManifestGives(t *testing.T) {
	dir := projectDir(t)
	cfg := twoProjectsSharingProdConfig(t)
	withSetupRunner(t, installedShopProd(t))
	code, out, errout := runSetupCmd("-e", "prod", "--host", "ops@s1", "--manifest-dir", dir, "--config", cfg)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errout)
	}
	if strings.Contains(out, " --project ") {
		t.Fatalf("follow-ups still name --project: %s", out)
	}
	if !strings.Contains(out, "berthy secret set NAME -e prod --config "+shellWord(cfg)+" --manifest-dir "+shellWord(dir)) ||
		!strings.Contains(out, "berthy deploy -e prod --config "+shellWord(cfg)+" --manifest-dir "+shellWord(dir)) {
		t.Fatalf("follow-ups: %s", out)
	}
}

func TestSetupFollowUpsNameTheProjectWithoutAManifest(t *testing.T) {
	empty := t.TempDir()
	cfg := twoProjectsSharingProdConfig(t)
	withSetupRunner(t, installedShopProd(t))
	code, out, errout := runSetupCmd("-e", "prod", "--project", "shop", "--host", "ops@s1", "--manifest-dir", empty, "--config", cfg)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errout)
	}
	if !strings.Contains(out, "berthy secret set NAME -e prod --project shop --config "+shellWord(cfg)) ||
		!strings.Contains(out, "berthy deploy -e prod --project shop --config "+shellWord(cfg)) {
		t.Fatalf("follow-ups: %s", out)
	}
	_, after, ok := strings.Cut(out, "\nnext steps:\n")
	if !ok {
		t.Fatal(out)
	}
	for _, line := range strings.Split(after, "\n") {
		if !strings.Contains(line, "berthy secret ") && !strings.Contains(line, "berthy deploy") && !strings.Contains(line, "berthy ingress ") {
			continue
		}
		if !strings.Contains(line, " --project shop") {
			t.Fatalf("follow-up lacks --project shop: %s", line)
		}
	}
}

func TestNextStepsFlagsOrder(t *testing.T) {
	id := scope.ID{Project: "shop", Environment: "prod"}
	withProject := setupNextSteps(nextSteps{id: id, keyFile: "/key", config: "/etc/cli.json", local: false, target: "ops@s1", tokenEnv: "T", storeToken: false, ingress: false, manifestDir: "/repo", inferProject: false})
	without := setupNextSteps(nextSteps{id: id, keyFile: "/key", config: "/etc/cli.json", local: false, target: "ops@s1", tokenEnv: "T", storeToken: false, ingress: false, manifestDir: "/repo", inferProject: true})
	wantWith := "-e prod --project shop --config /etc/cli.json --manifest-dir /repo"
	wantWithout := "-e prod --config /etc/cli.json --manifest-dir /repo"
	for _, step := range withProject[2:] {
		if !strings.Contains(step, wantWith) {
			t.Errorf("with project: %s", step)
		}
	}
	for _, step := range without[2:] {
		if !strings.Contains(step, wantWithout) || strings.Contains(step, " --project ") {
			t.Errorf("inferred project: %s", step)
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
	if !strings.Contains(g, "--out /var/lib/berthy/environments/todo/prod/state --key-file ./todo.prod.key.20261122T153012Z") || !strings.Contains(g, "--key-file ./todo.prod.key.20261122T153012Z --config") || strings.Contains(g, "`prod.key`") {
		t.Fatal("guide rebuild uses inconsistent key or state path")
	}
	s := string(spec)
	if !strings.Contains(s, "--out /var/lib/berthy/environments/todo/prod/state --key-file ./todo.prod.key.20261122T153012Z") {
		t.Fatal("spec restore uses wrong state or key")
	}
}

func TestKeyCopyShellTestSkipsWithoutSh(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestSetupKeyCopyPassesOneRemoteArgumentAndKeepsBackupOnFailure$", "-test.v")
	cmd.Env = append(os.Environ(), "PATH="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- SKIP:") || !strings.Contains(string(out), "sh is not available") {
		t.Fatalf("result %v: %s", err, out)
	}
}

func TestTimestampedKeyCopyText(t *testing.T) {
	old := keyCopyNow
	keyCopyNow = func() time.Time { return time.Date(2026, 11, 22, 15, 30, 12, 0, time.UTC) }
	t.Cleanup(func() { keyCopyNow = old })
	for _, tc := range []struct{ key, remote string }{
		{"/srv/key", `'sudo cat /srv/key'`},
		{"/srv/keys/my key", `'sudo cat '\''/srv/keys/my key'\'''`},
	} {
		t.Run(tc.key, func(t *testing.T) {
			steps := setupNextSteps(nextSteps{id: scope.ID{Project: "shop", Environment: "prod"}, keyFile: tc.key, config: "", local: false, target: "ops@s1", tokenEnv: "T", storeToken: false, ingress: false, manifestDir: ""})
			if steps[0] != "run the command from a private folder, such as your home folder (cd ~), keep the file private, then return to the setup manifest directory before the other commands" {
				t.Fatalf("instruction: %q", steps[0])
			}
			want := `in a POSIX shell (macOS, Linux, or Git Bash/WSL on Windows): (umask 077; set -C; ssh 'ops@s1' ` + tc.remote + ` > shop.prod.key.20261122T153012Z)`
			if steps[1] != want {
				t.Fatalf("got %q\nwant %q", steps[1], want)
			}
		})
	}
}

func TestTimestampedKeyCopyDoesNotOverwrite(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh is not available: %v", err)
	}
	old := keyCopyNow
	keyCopyNow = func() time.Time { return time.Date(2026, 11, 22, 15, 30, 12, 0, time.UTC) }
	t.Cleanup(func() { keyCopyNow = old })
	steps := setupNextSteps(nextSteps{id: scope.ID{Project: "shop", Environment: "prod"}, keyFile: "/srv/keys/my key", config: "", local: false, target: "ops@s1", tokenEnv: "T", storeToken: false, ingress: false, manifestDir: ""})
	_, command, ok := strings.Cut(steps[1], ": ")
	if !ok {
		t.Fatal(steps)
	}
	dir := t.TempDir()
	backup := filepath.Join(dir, "shop.prod.key.20261122T153012Z")
	if err := os.WriteFile(backup, []byte("existing backup"), 0600); err != nil {
		t.Fatal(err)
	}
	c := exec.Command(sh, "-c", "ssh() { printf 01234567890123456789012345678901; }; "+command)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err == nil || !(strings.Contains(string(out), "exists") || strings.Contains(string(out), "overwrite")) {
		t.Fatalf("expected noclobber refusal, got %v: %s", err, out)
	}
	b, err := os.ReadFile(backup)
	if err != nil || string(b) != "existing backup" {
		t.Fatalf("backup %q: %v", b, err)
	}
}
