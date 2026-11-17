package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// aliasBase is a valid manifest with one image app, to which a test adds its aliases.
const aliasBase = `project: demo
network: demo
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    roles:
      web: {}
`

// loadAliasManifest writes berthy.yml with text into a new directory and loads it for the environment prod.
func loadAliasManifest(t *testing.T, text string) (*Manifest, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return Load(dir, "prod")
}

func TestAliasesLoadFromTheTopLevel(t *testing.T) {
	m, err := loadAliasManifest(t, aliasBase+"aliases:\n  console: exec api --role web -- bin/console\n  weblogs: logs api --role web --follow\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Aliases) != 2 || m.Aliases["console"] != "exec api --role web -- bin/console" || m.Aliases["weblogs"] != "logs api --role web --follow" {
		t.Fatalf("aliases %v", m.Aliases)
	}
}

func TestAliasShapeIsRefused(t *testing.T) {
	many := "aliases:\n"
	for i := 0; i <= 64; i++ {
		many += fmt.Sprintf("  a%d: exec api -- x\n", i)
	}
	for name, text := range map[string]string{
		"upper-case name": "aliases:\n  Console: exec api -- x\n",
		"dotted name":     "aliases:\n  con.sole: exec api -- x\n",
		"empty value":     "aliases:\n  console: \"   \"\n",
		"double quotes":   "aliases:\n  console: 'exec api -- echo \"hi\"'\n",
		"single quotes":   "aliases:\n  console: \"exec api -- echo 'hi'\"\n",
		"NUL byte":        "aliases:\n  console: \"exec api\\0\"\n",
		"65 aliases":      many,
	} {
		if _, err := loadAliasManifest(t, aliasBase+text); err == nil || !strings.Contains(err.Error(), "alias") {
			t.Errorf("%s: err %v", name, err)
		}
	}
	// Sixty-four aliases are the most a manifest may hold.
	sixtyFour := aliasBase + "aliases:\n"
	for i := 0; i < 64; i++ {
		sixtyFour += fmt.Sprintf("  a%d: exec api -- x\n", i)
	}
	if _, err := loadAliasManifest(t, sixtyFour); err != nil {
		t.Errorf("64 aliases: %v", err)
	}
}

func TestManifestWithoutAliasesHasNoAliasesKey(t *testing.T) {
	m, err := loadAliasManifest(t, aliasBase)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "aliases") {
		t.Fatalf("a manifest without aliases marshals with the key: %s", b)
	}
}

// An overlay's alias replaces the base alias of the same name; the other base aliases stay, as the merge of mappings does.
func TestOverlayAliasReplacesTheBaseAliasOfTheSameName(t *testing.T) {
	dir := t.TempDir()
	base := aliasBase + "aliases:\n  console: exec api -- true\n  weblogs: logs api --follow\n"
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "berthy.prod.yml"), []byte("aliases:\n  console: exec api -- date\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(dir, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Aliases) != 2 || m.Aliases["console"] != "exec api -- date" || m.Aliases["weblogs"] != "logs api --follow" {
		t.Fatalf("aliases %v", m.Aliases)
	}
}
