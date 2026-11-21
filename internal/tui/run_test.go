package tui

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/agentclient"
)

// isolatedUserConfig points the user configuration directory at a new directory for the rest of the test, clears
// BERTHY_CONFIG, and returns that directory as os.UserConfigDir reports it.
func isolatedUserConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv(agentclient.ConfigEnv, "")
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("HOME", home)
	t.Setenv("AppData", home)
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunRefusesBadArguments(t *testing.T) {
	for name, args := range map[string][]string{
		"no env":        {"--config", "x"},
		"unknown flag":  {"-e", "prod", "--config", "x", "--json"},
		"missing value": {"-e"},
		"bad env":       {"-e", "Prod!", "--config", "x"},
	} {
		var out, errs bytes.Buffer
		if code := Run(args, strings.NewReader(""), &out, &errs); code != 2 {
			t.Errorf("%s: code %d, stderr %q", name, code, errs.String())
		}
	}
}

func TestRunFindsTheDefaultConfigFile(t *testing.T) {
	dir := isolatedUserConfig(t)
	t.Setenv("BERTHY_TEST_UI_TOKEN", "bty.prod.secretsecretsecret")
	if err := os.MkdirAll(filepath.Join(dir, "berthy"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"projects":{"demo":{"environments":{"prod":{"url":"http://127.0.0.1:1","token_env":"BERTHY_TEST_UI_TOKEN"}}}}}`
	if err := os.WriteFile(filepath.Join(dir, "berthy", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	// Without a terminal the UI stops after the configuration is read, so the message shows that the file was found.
	var out, errs bytes.Buffer
	if code := Run([]string{"-e", "prod"}, strings.NewReader(""), &out, &errs); code != 1 || !strings.Contains(errs.String(), "berthy ui needs a terminal") {
		t.Fatalf("code %d, stderr %q", code, errs.String())
	}
}

func TestRunNamesTheMissingDefaultFile(t *testing.T) {
	dir := isolatedUserConfig(t)
	var out, errs bytes.Buffer
	want := filepath.Join(dir, "berthy", "config.json")
	if code := Run([]string{"-e", "prod"}, strings.NewReader(""), &out, &errs); code != 1 || !strings.Contains(errs.String(), want) {
		t.Fatalf("code %d, stderr %q", code, errs.String())
	}
}

func TestRunNeedsATerminal(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "cli.json")
	t.Setenv("BERTHY_TEST_UI_TOKEN", "bty.prod.secretsecretsecret")
	if err := os.WriteFile(cfg, []byte(`{"projects":{"demo":{"environments":{"prod":{"url":"http://127.0.0.1:1","token_env":"BERTHY_TEST_UI_TOKEN"}}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	code := Run([]string{"-e", "prod", "--config", cfg}, strings.NewReader(""), &out, &errs)
	if code != 1 || !strings.Contains(errs.String(), "berthy ui needs a terminal") {
		t.Fatalf("code %d, stderr %q", code, errs.String())
	}
	if out.Len() != 0 || strings.Contains(errs.String(), "secretsecret") {
		t.Fatal("output holds something it should not")
	}
}

func TestRunLooksForTheProjectInTheCurrentDirectory(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "cli.json")
	t.Setenv("BERTHY_TEST_UI_TOKEN", "bty.prod.secretsecretsecret")
	if err := os.WriteFile(cfg, []byte(`{"projects":{"demo":{"environments":{"prod":{"url":"http://127.0.0.1:1","token_env":"BERTHY_TEST_UI_TOKEN"}}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte("project: other\nnetwork: demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	var out, errs bytes.Buffer
	if code := Run([]string{"-e", "prod", "--config", cfg}, strings.NewReader(""), &out, &errs); code != 1 || !strings.Contains(errs.String(), "no environment prod for project other in the CLI configuration") {
		t.Fatalf("code %d, stderr %q", code, errs.String())
	}
}
