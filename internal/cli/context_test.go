package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"berthy/internal/agentclient"
)

func TestContextAddWritesAnEntryThatTheOtherCommandsUse(t *testing.T) {
	isolatedUserConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(server.Close)
	cfg := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("CTX_ADD_TOKEN", "test-token")
	code, out, errs := runCLI("context", "add", "--project", "demo", "-e", "dev", "--url", server.URL, "--token-env", "CTX_ADD_TOKEN", "--config", cfg)
	if code != 0 {
		t.Fatalf("add: exit %d stderr %q", code, errs)
	}
	if !strings.Contains(out, "added demo/dev at "+server.URL+" to "+cfg) || !strings.Contains(out, "(token in $CTX_ADD_TOKEN)") {
		t.Fatalf("add output %q", out)
	}
	code, _, errs = runCLI("agent-info", "-e", "dev", "--config", cfg)
	if code != 0 {
		t.Fatalf("agent-info through the new entry: exit %d stderr %q", code, errs)
	}
}

func TestContextAddCreatesTheFileAndItsDirectory(t *testing.T) {
	isolatedUserConfig(t)
	dir := filepath.Join(t.TempDir(), "fresh")
	cfg := filepath.Join(dir, "config.json")
	t.Setenv(agentclient.ConfigEnv, cfg)
	t.Setenv("CTX_DIR_TOKEN", "tok")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the directory already exists: %v", err)
	}
	code, out, errs := runCLI("context", "add", "--project", "demo", "-e", "dev", "--url", "http://127.0.0.1:9", "--token-env", "CTX_DIR_TOKEN")
	if code != 0 {
		t.Fatalf("add: exit %d stderr %q stdout %q", code, errs, out)
	}
	st, err := os.Stat(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v", st.Mode())
	}
	dst, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && dst.Mode().Perm() != 0o700 {
		t.Errorf("directory mode = %v", dst.Mode())
	}
	got, err := agentclient.ReadConfig(cfg)
	if err != nil || got.Projects["demo"].Environments["dev"] != (agentclient.Environment{URL: "http://127.0.0.1:9", TokenEnv: "CTX_DIR_TOKEN"}) {
		t.Fatalf("file: %+v %v", got, err)
	}
}

func TestContextAddUpdatesAnEntryAndKeepsTheOthers(t *testing.T) {
	isolatedUserConfig(t)
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, []byte(`{"projects":{"demo":{"environments":{"dev":{"url":"http://10.0.0.1:7710","token_env":"OLD_TOKEN"},"prod":{"url":"http://10.0.0.2:7710","token_env":"PROD_TOKEN"}}},"other":{"environments":{"dev":{"url":"http://10.1.1.1:7710","token_env":"OTHER_TOKEN"}}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEW_TOKEN", "n")
	code, out, errs := runCLI("context", "add", "--project", "demo", "-e", "dev", "--url", "http://10.9.9.9:7710", "--token-env", "NEW_TOKEN", "--config", cfg)
	if code != 0 || !strings.Contains(out, "updated demo/dev at http://10.9.9.9:7710 to "+cfg) {
		t.Fatalf("update: exit %d stdout %q stderr %q", code, out, errs)
	}
	got, err := agentclient.ReadConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Projects["demo"].Environments["dev"] != (agentclient.Environment{URL: "http://10.9.9.9:7710", TokenEnv: "NEW_TOKEN"}) {
		t.Fatalf("updated entry: %+v", got.Projects["demo"].Environments["dev"])
	}
	if got.Projects["demo"].Environments["prod"] != (agentclient.Environment{URL: "http://10.0.0.2:7710", TokenEnv: "PROD_TOKEN"}) ||
		got.Projects["other"].Environments["dev"] != (agentclient.Environment{URL: "http://10.1.1.1:7710", TokenEnv: "OTHER_TOKEN"}) {
		t.Fatalf("other entries: %+v", got)
	}
	code, out, errs = runCLI("context", "add", "--project", "demo", "-e", "dev", "--url", "http://10.9.9.9:7710", "--token-env", "NEW_TOKEN", "--config", cfg)
	if code != 0 || !strings.Contains(out, "demo/dev is already at http://10.9.9.9:7710 in "+cfg) {
		t.Fatalf("already: exit %d stdout %q stderr %q", code, out, errs)
	}
}

func TestContextAddNeedsEveryFlag(t *testing.T) {
	isolatedUserConfig(t)
	cfg := filepath.Join(t.TempDir(), "config.json")
	base := []string{"--project", "demo", "-e", "dev", "--url", "http://127.0.0.1:9", "--token-env", "TOK", "--config", cfg}
	for _, drop := range []string{"--project", "-e", "--url", "--token-env"} {
		var args []string
		for i := 0; i < len(base); {
			if base[i] == drop {
				i += 2
				continue
			}
			args = append(args, base[i])
			i++
		}
		code, out, errs := runCLI(append([]string{"context", "add"}, args...)...)
		if code != 2 || out != "" || !strings.Contains(errs, "usage:") {
			t.Errorf("without %s: exit %d stdout %q stderr %q", drop, code, out, errs)
		}
		if _, err := os.Stat(cfg); err == nil {
			t.Errorf("without %s: wrote the file", drop)
		}
	}
}

func TestContextListSaysWhetherTheVariableIsSetAndNeverItsValue(t *testing.T) {
	isolatedUserConfig(t)
	const marker = "marker-token-value-that-must-not-appear"
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, []byte(`{"projects":{"zoo":{"environments":{"prod":{"url":"http://10.0.0.2:7710","token_env":"CTX_LIST_UNSET"}}},"demo":{"environments":{"dev":{"url":"http://10.0.0.1:7710","token_env":"CTX_LIST_SET"}}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CTX_LIST_SET", marker)
	t.Setenv("CTX_LIST_UNSET", "")
	code, out, errs := runCLI("context", "list", "--config", cfg)
	if code != 0 {
		t.Fatalf("list: exit %d stderr %q", code, errs)
	}
	if strings.Contains(out, marker) || strings.Contains(errs, marker) {
		t.Fatalf("the token value leaked:\nstdout %q\nstderr %q", out, errs)
	}
	if !strings.Contains(out, "CTX_LIST_SET") || !strings.Contains(out, "CTX_LIST_UNSET") {
		t.Fatalf("list does not name the variables: %q", out)
	}
	if !strings.Contains(out, "yes") || !strings.Contains(out, "no") {
		t.Fatalf("list does not say whether the variable is set: %q", out)
	}
	// Sorted by project then environment: demo/dev before zoo/prod.
	if i, j := strings.Index(out, "demo"), strings.Index(out, "zoo"); i < 0 || j < 0 || i > j {
		t.Fatalf("not sorted by project: %q", out)
	}
	code, js, errs := runCLI("context", "list", "--config", cfg, "--json")
	if code != 0 {
		t.Fatalf("list --json: exit %d stderr %q", code, errs)
	}
	if strings.Contains(js, marker) {
		t.Fatalf("json leaked the token: %q", js)
	}
	var rows []struct {
		Project     string `json:"project"`
		Environment string `json:"environment"`
		URL         string `json:"url"`
		TokenEnv    string `json:"token_env"`
		TokenSet    bool   `json:"token_set"`
	}
	if err := json.Unmarshal([]byte(js), &rows); err != nil {
		t.Fatalf("json: %v in %q", err, js)
	}
	if len(rows) != 2 || rows[0].Project != "demo" || !rows[0].TokenSet || rows[1].Project != "zoo" || rows[1].TokenSet {
		t.Fatalf("json rows: %+v", rows)
	}
}

func TestContextRemove(t *testing.T) {
	isolatedUserConfig(t)
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, []byte(`{"projects":{"demo":{"environments":{"dev":{"url":"http://127.0.0.1:9","token_env":"TOK"},"prod":{"url":"http://10.0.0.1:7710","token_env":"PROD"}}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runCLI("context", "remove", "--project", "demo", "-e", "dev", "--config", cfg)
	if code != 0 || !strings.Contains(out, "removed demo/dev from "+cfg) {
		t.Fatalf("remove: exit %d stdout %q stderr %q", code, out, errs)
	}
	got, err := agentclient.ReadConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Projects["demo"].Environments["dev"]; ok {
		t.Fatal("dev is still there")
	}
	if got.Projects["demo"].Environments["prod"].TokenEnv != "PROD" {
		t.Fatalf("prod was dropped: %+v", got)
	}
	code, out, errs = runCLI("context", "remove", "--project", "demo", "-e", "dev", "--config", cfg)
	if code != 1 || out != "" || errs != "no environment dev for project demo in the CLI configuration\n" {
		t.Fatalf("missing: exit %d stdout %q stderr %q", code, out, errs)
	}
}

func TestContextCommandsContactNoAgent(t *testing.T) {
	isolatedUserConfig(t)
	cfg := filepath.Join(t.TempDir(), "config.json")
	url := "http://127.0.0.1:1"
	t.Setenv("CTX_DEAD_TOKEN", "tok")
	code, _, errs := runCLI("context", "add", "--project", "demo", "-e", "dev", "--url", url, "--token-env", "CTX_DEAD_TOKEN", "--config", cfg)
	if code != 0 {
		t.Fatalf("add: exit %d stderr %q", code, errs)
	}
	code, _, errs = runCLI("context", "list", "--config", cfg)
	if code != 0 {
		t.Fatalf("list: exit %d stderr %q", code, errs)
	}
	code, _, errs = runCLI("context", "remove", "--project", "demo", "-e", "dev", "--config", cfg)
	if code != 0 {
		t.Fatalf("remove: exit %d stderr %q", code, errs)
	}
}

func TestContextListOfAMissingFilePrintsNothing(t *testing.T) {
	isolatedUserConfig(t)
	cfg := filepath.Join(t.TempDir(), "missing.json")
	code, out, errs := runCLI("context", "list", "--config", cfg)
	if code != 0 || out != "" || errs != "" {
		t.Fatalf("missing: exit %d stdout %q stderr %q", code, out, errs)
	}
	code, out, errs = runCLI("context", "list", "--config", cfg, "--json")
	if code != 0 || out != "" || errs != "" {
		t.Fatalf("missing json: exit %d stdout %q stderr %q", code, out, errs)
	}
}
