package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/credstore/credtest"
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

func TestContextAddRefusesAnothersTokenVariable(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "cli.json")
	original := `{"projects":{"my-shop":{"environments":{"prod":{"url":"http://127.0.0.1:7710","token_env":"SHARED_TOKEN"}}}}}`
	if err := os.WriteFile(cfg, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runCLI("context", "add", "--project", "my_shop", "-e", "prod", "--url", "http://127.0.0.1:7711", "--token-env", "SHARED_TOKEN", "--config", cfg)
	if code == 0 || !strings.Contains(out+errs, "my-shop/prod") || !strings.Contains(out+errs, "my_shop/prod") || !strings.Contains(out+errs, "--token-env") {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	b, _ := os.ReadFile(cfg)
	if string(b) != original {
		t.Fatal("config changed")
	}
}

func tokenFixture(t *testing.T) (*credtest.Memory, string, string, *string) {
	t.Helper()
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		fmt.Fprint(w, `[]`)
	}))
	t.Cleanup(server.Close)
	cfg := filepath.Join(t.TempDir(), "config.json")
	body := fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"CTX_STORE_TOKEN"}}}}}`, server.URL)
	if err := os.WriteFile(cfg, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CTX_STORE_TOKEN", "")
	s := credtest.NewMemory()
	if err := s.Set(agentclient.StoreKey("demo", "dev", agentclient.Environment{URL: server.URL, TokenEnv: "CTX_STORE_TOKEN"}), "stored"); err != nil {
		t.Fatal(err)
	}
	old := agentclient.TokenStore
	agentclient.TokenStore = s
	t.Cleanup(func() { agentclient.TokenStore = old })
	return s, cfg, server.URL, &auth
}
func TestAnEntryWithAnotherVariableDoesNotGetTheStoredToken(t *testing.T) {
	_, _, url, auth := tokenFixture(t)
	t.Setenv("CTX_SCRIPT_TOKEN", "")
	cfg := filepath.Join(t.TempDir(), "config.json")
	body := fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"CTX_SCRIPT_TOKEN"}}}}}`, url)
	_ = os.WriteFile(cfg, []byte(body), 0600)
	code, _, err := runCLI("ps", "-e", "dev", "--config", cfg)
	if code != 1 || !strings.Contains(err, "the test store has none saved for it") || *auth != "" {
		t.Fatalf("%d %q auth %q", code, err, *auth)
	}
}
func TestRemoteCommandSendsTheStoredToken(t *testing.T) {
	_, cfg, _, auth := tokenFixture(t)
	code, _, err := runCLI("ps", "-e", "dev", "--config", cfg)
	if code != 0 || *auth != "Bearer stored" {
		t.Fatalf("%d %q auth %q", code, err, *auth)
	}
}
func TestTheVariableWinsOverTheStoredToken(t *testing.T) {
	_, cfg, _, auth := tokenFixture(t)
	t.Setenv("CTX_STORE_TOKEN", "variable")
	code, _, err := runCLI("ps", "-e", "dev", "--config", cfg)
	if code != 0 || *auth != "Bearer variable" {
		t.Fatalf("%d %q auth %q", code, err, *auth)
	}
}
func TestContextListSaysWhetherATokenIsStored(t *testing.T) {
	store, cfg, url, _ := tokenFixture(t)
	code, out, err := runCLI("context", "list", "--config", cfg)
	if code != 0 || !strings.Contains(out, "stored") {
		t.Fatalf("%d %q %q", code, out, err)
	}
	code, out, err = runCLI("context", "list", "--config", cfg, "--json")
	var rows []struct {
		Stored bool `json:"token_stored"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || !rows[0].Stored {
		t.Fatalf("%d %q %q", code, out, err)
	}
	_, _ = store.Delete(agentclient.StoreKey("demo", "dev", agentclient.Environment{URL: url, TokenEnv: "CTX_STORE_TOKEN"}))
	code, out, err = runCLI("context", "list", "--config", cfg)
	if code != 0 || !strings.Contains(out, "not stored") {
		t.Fatalf("%d %q %q", code, out, err)
	}
	code, out, err = runCLI("context", "list", "--config", cfg, "--json")
	if code != 0 || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0].Stored {
		t.Fatalf("%d %q %q", code, out, err)
	}
}
func blockedTokenFixture(t *testing.T) (*credtest.Memory, string) {
	t.Helper()
	s, cfg, _, _ := tokenFixture(t)
	ch := make(chan struct{})
	s.Block = ch
	old := agentclient.StoreWait
	agentclient.StoreWait = time.Millisecond
	t.Cleanup(func() { close(ch); s.Wait(); agentclient.StoreWait = old })
	return s, cfg
}
func TestContextListDoesNotHangOnAStoreThatNeverAnswers(t *testing.T) {
	s, cfg := blockedTokenFixture(t)
	code, out, err := runCLI("context", "list", "--config", cfg)
	if code != 0 || !strings.Contains(out, "unknown") || s.Calls() <= 1 {
		t.Fatalf("%d %q %q", code, out, err)
	}
}
func TestARemoteCommandDoesNotHangOnAStoreThatNeverAnswers(t *testing.T) {
	s, cfg := blockedTokenFixture(t)
	code, _, err := runCLI("ps", "-e", "dev", "--config", cfg)
	if code != 1 || !strings.Contains(err, "this machine has no credential store") || s.Calls() <= 1 {
		t.Fatalf("%d %q", code, err)
	}
}
func TestContextRemoveKeepsTheStoredToken(t *testing.T) {
	s, cfg, _, auth := tokenFixture(t)
	b, _ := os.ReadFile(cfg)
	cfg2 := filepath.Join(t.TempDir(), "config.json")
	_ = os.WriteFile(cfg2, b, 0600)
	calls := s.Calls()
	code, _, err := runCLI("context", "remove", "--project", "demo", "-e", "dev", "--config", cfg)
	if code != 0 || s.Calls() != calls {
		t.Fatalf("%d %q calls %d", code, err, s.Calls()-calls)
	}
	code, _, err = runCLI("ps", "-e", "dev", "--config", cfg2)
	if code != 0 || *auth != "Bearer stored" {
		t.Fatalf("%d %q %q", code, err, *auth)
	}
}
func TestContextRemoveDeleteTokenDeletesItFirst(t *testing.T) {
	s, cfg, _, _ := tokenFixture(t)
	original, readErr := os.ReadFile(cfg)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var during []byte
	s.OnCall = func() { during, _ = os.ReadFile(cfg) }
	code, out, err := runCLI("context", "remove", "--project", "demo", "-e", "dev", "--config", cfg, "--delete-token")
	if code != 0 || s.Len() != 0 || string(during) != string(original) || !strings.Contains(out, "deleted the token of demo/dev from the test store") {
		t.Fatalf("%d %q %q len %d", code, out, err, s.Len())
	}
}
func TestContextRemoveDeleteTokenThatFailsKeepsTheEntry(t *testing.T) {
	s, cfg, _, _ := tokenFixture(t)
	before, _ := os.ReadFile(cfg)
	s.DeleteErr = errors.New("refused")
	code, _, err := runCLI("context", "remove", "--project", "demo", "-e", "dev", "--config", cfg, "--delete-token")
	after, _ := os.ReadFile(cfg)
	if code != 1 || string(before) != string(after) || !strings.Contains(err, "the stored token was not deleted (refused): the entry is kept, so the command can be run again") {
		t.Fatalf("%d %q", code, err)
	}
}

func TestContextCommandsRefuseAmbiguousTokenVariableBeforeStore(t *testing.T) {
	for _, args := range [][]string{{"remove", "--project", "demo", "-e", "dev", "--delete-token"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			s, cfg, _, _ := tokenFixture(t)
			good := agentclient.Environment{URL: "http://host#X", TokenEnv: "Y"}
			bad := agentclient.Environment{URL: "http://host", TokenEnv: "X#Y"}
			if agentclient.StoreKey("demo", "dev", good).Account() != agentclient.StoreKey("demo", "dev", bad).Account() {
				t.Fatal("fixture keys do not collide")
			}
			if err := s.Set(agentclient.StoreKey("demo", "dev", good), "admin"); err != nil {
				t.Fatal(err)
			}
			original := []byte(`{"projects":{"demo":{"environments":{"dev":{"url":"http://host","token_env":"X#Y"}}}}}`)
			if err := os.WriteFile(cfg, original, 0600); err != nil {
				t.Fatal(err)
			}
			calls := s.Calls()
			code, out, err := runCLI(append(append([]string{"context"}, args...), "--config", cfg)...)
			after, readErr := os.ReadFile(cfg)
			if code != 1 || out != "" || err != "unknown environment or missing token_env\n" || s.Calls() != calls || readErr != nil || string(after) != string(original) {
				t.Fatalf("exit %d stdout %q stderr %q store calls %d file error %v", code, out, err, s.Calls()-calls, readErr)
			}
		})
	}
}

func TestContextRemoveDeleteTokenGivenUpKeepsTheEntry(t *testing.T) {
	s, cfg := blockedTokenFixture(t)
	before, readErr := os.ReadFile(cfg)
	if readErr != nil {
		t.Fatal(readErr)
	}
	code, out, err := runCLI("context", "remove", "--project", "demo", "-e", "dev", "--config", cfg, "--delete-token")
	after, readErr := os.ReadFile(cfg)
	want := "the stored token could not be confirmed deleted (no credential store on this machine: the store did not answer): the entry is kept, so the command can be run again\n"
	if code != 1 || out != "" || err != want || readErr != nil || string(before) != string(after) || s.Calls() <= 1 {
		t.Fatalf("exit %d stdout %q stderr %q file error %v", code, out, err, readErr)
	}
}

func TestBlockedTokenFixtureCanCleanUpWithoutAStoreCall(t *testing.T) {
	t.Run("no lookup", func(t *testing.T) { _, _ = blockedTokenFixture(t) })
}

func TestContextListSkipsUnsafeStoreKeys(t *testing.T) {
	for _, badProject := range []string{"bad@name", "bad/name", "invalid-var"} {
		t.Run(badProject, func(t *testing.T) {
			badVar := "CTX_STORE_TOKEN"
			if badProject == "invalid-var" {
				badVar = "X#Y"
			}
			s, cfg, _, _ := tokenFixture(t)
			body := fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":"http://host","token_env":"CTX_STORE_TOKEN"}}},%q:{"environments":{"dev":{"url":"http://host","token_env":%q}}}}}`, badProject, badVar)
			if err := os.WriteFile(cfg, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			calls := s.Calls()
			code, out, err := runCLI("context", "list", "--config", cfg)
			if code != 0 || err != "" || !strings.Contains(out, badProject) || !strings.Contains(out, "demo") || !strings.Contains(out, "unknown") || s.Calls() != calls+1 {
				t.Fatalf("exit %d out %q err %q calls %d", code, out, err, s.Calls()-calls)
			}
			calls = s.Calls()
			code, out, err = runCLI("context", "list", "--config", cfg, "--json")
			var rows []struct {
				Project string `json:"project"`
				Stored  bool   `json:"token_stored"`
			}
			if code != 0 || err != "" || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 2 || s.Calls() != calls+1 {
				t.Fatalf("exit %d out %q err %q calls %d", code, out, err, s.Calls()-calls)
			}
			for _, r := range rows {
				if r.Project == badProject && r.Stored {
					t.Fatalf("unsafe entry marked stored: %q", out)
				}
			}
		})
	}
}
func TestContextRemoveRejectsInvalidScopeBeforeStore(t *testing.T) {
	for _, project := range []string{"bad@name", "bad/name"} {
		t.Run(project, func(t *testing.T) {
			s, cfg, _, _ := tokenFixture(t)
			body := fmt.Sprintf(`{"projects":{%q:{"environments":{"dev":{"url":"http://host","token_env":"CTX_STORE_TOKEN"}}}}}`, project)
			if err := os.WriteFile(cfg, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			calls := s.Calls()
			code, out, err := runCLI("context", "remove", "--project", project, "-e", "dev", "--delete-token", "--config", cfg)
			after, _ := os.ReadFile(cfg)
			if code != 1 || out != "" || err != "unknown environment or missing token_env\n" || s.Calls() != calls || string(after) != body {
				t.Fatalf("exit %d out %q err %q calls %d", code, out, err, s.Calls()-calls)
			}
		})
	}
}
