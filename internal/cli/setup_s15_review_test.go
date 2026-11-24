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

func TestKeyRotationCopyKeepsBackupOnFailure(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh is not available: %v", err)
	}
	old := keyCopyNow
	keyCopyNow = func() time.Time { return time.Date(2026, 11, 22, 15, 30, 12, 0, time.UTC) }
	t.Cleanup(func() { keyCopyNow = old })
	var out, errout strings.Builder
	if printKeyRotation(&out, &errout, []byte(`{"old_key_id":"old","new_key_id":"new","key_file":"/srv/key"}`), scope.ID{Project: "shop", Environment: "prod"}) != 0 {
		t.Fatal("rotation output failed")
	}
	_, cmd, ok := strings.Cut(out.String(), "another machine: ")
	if !ok {
		t.Fatal("copy instruction missing")
	}
	dir := t.TempDir()
	run := func() error {
		c := exec.Command(sh, "-c", "sudo() { printf 01234567890123456789012345678901; }; "+cmd)
		c.Dir = dir
		c.Env = append(os.Environ(), "HOME="+dir)
		return c.Run()
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "shop.prod.key.20261122T153012Z")
	before, err := os.ReadFile(path)
	if err != nil || len(before) != 32 {
		t.Fatal("copy missing")
	}
	if run() == nil {
		t.Fatal("overwrote existing backup")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("backup changed")
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

func TestSetupFollowUpsKeepRelativeManifestAndConfigPaths(t *testing.T) {
	root := t.TempDir()
	todo := filepath.Join(root, "todo")
	if err := os.Mkdir(todo, 0o700); err != nil {
		t.Fatal(err)
	}
	text := "project: shop\nnetwork: p5net\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"
	if err := os.WriteFile(filepath.Join(todo, "berthy.yml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := `{"projects":{"shop":{"environments":{"prod":{"url":"http://127.0.0.1:7710","token_env":"SHOP_PROD"}}},"other":{"environments":{"prod":{"url":"http://127.0.0.1:7711","token_env":"OTHER_PROD"}}}}}`
	if err := os.WriteFile(filepath.Join(root, "cli.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	withSetupRunner(t, installedShopProd(t))
	code, out, errout := runSetupCmd("-e", "prod", "--host", "ops@s1", "--manifest-dir", "todo", "--config", "cli.json")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errout)
	}
	if !strings.Contains(out, "from this same directory") {
		t.Fatalf("instruction should name the setup cwd, not the manifest directory:\n%s", out)
	}
	if strings.Contains(out, "setup manifest directory") {
		t.Fatalf("instruction still names the manifest directory:\n%s", out)
	}
	flags := " -e prod --config cli.json --manifest-dir todo"
	for _, want := range []string{
		"berthy secret set NAME" + flags,
		"berthy deploy" + flags + " --version APP=REF",
		"berthy deploy APP" + flags + " --version REF",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
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
	flags := " -e prod --config " + shellWord(cfg) + " --manifest-dir " + shellWord(dir)
	for _, want := range []string{
		"berthy secret set NAME" + flags,
		"berthy deploy" + flags + " --version APP=REF",
		"berthy deploy APP" + flags + " --version REF",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
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
	flags := " -e prod --project shop --config " + shellWord(cfg) + " --manifest-dir " + shellWord(empty)
	for _, want := range []string{
		"berthy secret set NAME" + flags,
		"berthy deploy" + flags + " --version APP=REF",
		"berthy deploy APP" + flags + " --version REF",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestNextStepsFlagsOrder(t *testing.T) {
	id := scope.ID{Project: "shop", Environment: "prod"}
	withProject := setupNextSteps(nextSteps{id: id, config: "/etc/cli.json", local: false, tokenEnv: "T", storeToken: false, ingress: false, manifestDir: "/repo", inferProject: false})
	without := setupNextSteps(nextSteps{id: id, config: "/etc/cli.json", local: false, tokenEnv: "T", storeToken: false, ingress: false, manifestDir: "/repo", inferProject: true})
	wantWith := "-e prod --project shop --config /etc/cli.json --manifest-dir /repo"
	wantWithout := "-e prod --config /etc/cli.json --manifest-dir /repo"
	for _, step := range withProject[1:] {
		if !strings.Contains(step, wantWith) {
			t.Errorf("with project: %s", step)
		}
	}
	for _, step := range without[1:] {
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
	if !strings.Contains(g, "--out /var/lib/berthy/environments/todo/prod/state --key-file ./berthy.todo.prod.91c0aa11bb22cc33.key") || !strings.Contains(g, "--key-file ~/.berthy/keys/berthy.todo.prod.91c0aa11bb22cc33.key --config") || strings.Contains(g, "`prod.key`") {
		t.Fatal("guide rebuild uses inconsistent key or state path")
	}
	s := string(spec)
	if !strings.Contains(s, "--out /var/lib/berthy/environments/todo/prod/state --key-file ./berthy.todo.prod.91c0aa11bb22cc33.key") {
		t.Fatal("spec restore uses wrong state or key")
	}
}

func TestKeyCopyShellTestSkipsWithoutSh(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestKeyRotationCopyKeepsBackupOnFailure$", "-test.v")
	cmd.Env = append(os.Environ(), "PATH="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- SKIP:") || !strings.Contains(string(out), "sh is not available") {
		t.Fatalf("result %v: %s", err, out)
	}
}
