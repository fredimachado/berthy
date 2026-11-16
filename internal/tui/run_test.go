package tui

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRefusesBadArguments(t *testing.T) {
	for name, args := range map[string][]string{
		"no env":        {"--config", "x"},
		"no config":     {"-e", "prod"},
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

func TestRunNeedsATerminal(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "cli.json")
	t.Setenv("BERTHY_TEST_UI_TOKEN", "bty.prod.secretsecretsecret")
	if err := os.WriteFile(cfg, []byte(`{"environments":{"prod":{"url":"http://127.0.0.1:1","token_env":"BERTHY_TEST_UI_TOKEN"}}}`), 0o600); err != nil {
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
