package agentclient

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPutAddsAnEntryAndKeepsTheOthers(t *testing.T) {
	c := mustParseConfig(t, twoProjects)
	changed, err := c.Put("sliceup", "staging", Environment{URL: "http://10.0.0.4:7710", TokenEnv: "SU_STAGING"})
	if err != nil || !changed {
		t.Fatalf("Put: changed=%v err=%v", changed, err)
	}
	if c.Projects["sliceup"].Environments["staging"] != (Environment{URL: "http://10.0.0.4:7710", TokenEnv: "SU_STAGING"}) {
		t.Fatalf("new entry: %+v", c.Projects["sliceup"].Environments["staging"])
	}
	if c.Projects["sliceup"].Environments["prod"] != (Environment{URL: "http://10.0.0.2:7710", TokenEnv: "SU_TOKEN"}) ||
		c.Projects["sliceup"].Environments["dev"] != (Environment{URL: "http://10.0.0.3:7710", TokenEnv: "SU_DEV_TOKEN"}) ||
		c.Projects["cardiotennis"].Environments["prod"] != (Environment{URL: "http://10.0.0.1:7710", TokenEnv: "CT_TOKEN"}) {
		t.Fatalf("other entries: %+v", c)
	}
}

func TestPutRefuses(t *testing.T) {
	good := Environment{URL: "http://127.0.0.1:9000", TokenEnv: "BERTHY_TEST_TOKEN"}
	for _, tc := range []struct {
		name, project, env string
		e                  Environment
	}{
		{"invalid project", "Bad Project", "dev", good},
		{"invalid environment", "demo", "Prod", good},
		{"URL with a user", "demo", "dev", Environment{URL: "http://user:pw@127.0.0.1:9000", TokenEnv: "BERTHY_TEST_TOKEN"}},
		{"URL with a query", "demo", "dev", Environment{URL: "http://127.0.0.1:9000?x=1", TokenEnv: "BERTHY_TEST_TOKEN"}},
		{"https", "demo", "dev", Environment{URL: "https://127.0.0.1:9000", TokenEnv: "BERTHY_TEST_TOKEN"}},
		{"URL without a host", "demo", "dev", Environment{URL: "http://", TokenEnv: "BERTHY_TEST_TOKEN"}},
		{"variable with a dash", "demo", "dev", Environment{URL: "http://127.0.0.1:9000", TokenEnv: "BERTHY-TEST"}},
		{"variable with a leading digit", "demo", "dev", Environment{URL: "http://127.0.0.1:9000", TokenEnv: "1TOKEN"}},
	} {
		c := Config{}
		changed, err := c.Put(tc.project, tc.env, tc.e)
		if err == nil || changed {
			t.Errorf("%s: changed=%v err=%v", tc.name, changed, err)
			continue
		}
		for _, leaked := range []string{"user:pw", "pw", "https://", "?x=1", "BERTHY-TEST", "1TOKEN", "Bad Project"} {
			if strings.Contains(err.Error(), leaked) {
				t.Errorf("%s: the refusal repeats %q: %v", tc.name, leaked, err)
			}
		}
		if len(c.Projects) != 0 {
			t.Errorf("%s: wrote an entry: %+v", tc.name, c)
		}
	}
}

func TestPutOfTheSameEntryChangesNothing(t *testing.T) {
	c := Config{}
	e := Environment{URL: "http://127.0.0.1:9000", TokenEnv: "BERTHY_TEST_TOKEN"}
	if _, err := c.Put("demo", "dev", e); err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := c.Put("demo", "dev", e)
	if err != nil || changed {
		t.Fatalf("second Put: changed=%v err=%v", changed, err)
	}
	after, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("the config changed: %s -> %s", before, after)
	}
}

func TestRemoveDropsAProjectWithNoEnvironmentLeft(t *testing.T) {
	c := mustParseConfig(t, twoProjects)
	if !c.Remove("sliceup", "dev") {
		t.Fatal("Remove of an entry that is there returned false")
	}
	if _, ok := c.Projects["sliceup"].Environments["dev"]; ok {
		t.Fatal("dev is still there")
	}
	if _, ok := c.Projects["sliceup"].Environments["prod"]; !ok {
		t.Fatal("prod of sliceup was dropped")
	}
	if !c.Remove("sliceup", "prod") {
		t.Fatal("Remove of the last environment returned false")
	}
	if _, ok := c.Projects["sliceup"]; ok {
		t.Fatalf("sliceup is still there: %+v", c.Projects["sliceup"])
	}
	if _, ok := c.Projects["cardiotennis"]; !ok {
		t.Fatal("cardiotennis was dropped")
	}
	if c.Remove("sliceup", "prod") {
		t.Fatal("Remove of a missing entry returned true")
	}
}

func TestReadConfigOfAMissingFileIsEmpty(t *testing.T) {
	c, err := ReadConfig(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil || len(c.Projects) != 0 {
		t.Fatalf("ReadConfig of a missing file: %+v %v", c, err)
	}
}

func TestWriteConfigCreatesAPrivateFileAndReplacesAnExistingOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := Config{Projects: map[string]Project{"demo": {Environments: map[string]Environment{
		"dev": {URL: "http://127.0.0.1:9000", TokenEnv: "BERTHY_TEST_TOKEN"},
	}}}}
	if err := WriteConfig(path, c); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fileModeIs(st, 0o600) {
		t.Errorf("new file mode = %v", st.Mode())
	}
	got, err := ReadConfig(path)
	if err != nil || got.Projects["demo"].Environments["dev"] != c.Projects["demo"].Environments["dev"] {
		t.Fatalf("read back: %+v %v", got, err)
	}

	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"projects":{}}`), 0o640); err != nil {
		t.Fatal(err)
	}
	next := Config{Projects: map[string]Project{"shop": {Environments: map[string]Environment{
		"prod": {URL: "http://10.0.0.5:7710", TokenEnv: "SHOP_TOKEN"},
	}}}}
	if err := WriteConfig(path, next); err != nil {
		t.Fatal(err)
	}
	st, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fileModeIs(st, 0o640) {
		t.Errorf("existing file mode = %v", st.Mode())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "demo") || !strings.Contains(string(body), "shop") {
		t.Fatalf("old content still there: %s", body)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Fatalf("directory after write: %v", namesOf(entries))
	}
}

func TestResolveAndPutShareTheURLRule(t *testing.T) {
	t.Setenv("BERTHY_TEST_TOKEN", "secret-token")
	good := []string{"http://127.0.0.1:9000", "http://127.0.0.1:9000/base/", "http://localhost", "http://[::1]:7710"}
	for _, raw := range good {
		c := Config{}
		if _, err := c.Put("demo", "dev", Environment{URL: raw, TokenEnv: "BERTHY_TEST_TOKEN"}); err != nil {
			t.Errorf("Put accepted no %s: %v", raw, err)
			continue
		}
		path := writeConfig(t, mustJSON(t, c))
		if _, err := Resolve(path, Selector{Project: "demo", Environment: "dev"}); err != nil {
			t.Errorf("Resolve refused a URL Put accepted (%s): %v", raw, err)
		}
	}
	bad := []string{
		"https://127.0.0.1:9000",
		"http://user:pw@127.0.0.1:9000",
		"http://127.0.0.1:9000?x=1",
		"http://127.0.0.1:9000#frag",
		"http://",
		"ftp://127.0.0.1:9000",
	}
	for _, raw := range bad {
		c := Config{}
		if _, err := c.Put("demo", "dev", Environment{URL: raw, TokenEnv: "BERTHY_TEST_TOKEN"}); err == nil {
			t.Errorf("Put accepted %s", raw)
		}
		path := writeConfig(t, `{"projects":{"demo":{"environments":{"dev":{"url":`+mustJSON(t, raw)+`,"token_env":"BERTHY_TEST_TOKEN"}}}}}`)
		if _, err := Resolve(path, Selector{Project: "demo", Environment: "dev"}); err == nil || !strings.Contains(err.Error(), "invalid agent URL") {
			t.Errorf("Resolve of a URL Put refuses (%s): %v", raw, err)
		}
	}
}

func mustParseConfig(t *testing.T, body string) Config {
	t.Helper()
	c, err := ParseConfig([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func namesOf(entries []fs.DirEntry) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

// fileModeIs reports whether st has the permission bits want. Windows has no such bits, so it always does there.
func fileModeIs(st os.FileInfo, want fs.FileMode) bool {
	return runtime.GOOS == "windows" || st.Mode().Perm() == want
}
