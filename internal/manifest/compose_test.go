package manifest

import (
	"strings"
	"testing"
)

func TestComposeProjectNameDefaultsAndOverride(t *testing.T) {
	m := Manifest{Project: "shop", Apps: map[string]App{
		"db":    {Kind: "compose", File: "compose.yaml"},
		"cache": {Kind: "compose", File: "cache.yaml", ProjectName: "legacy-cache"},
	}}
	if got := m.ComposeProject("db"); got != "shop-db" {
		t.Fatalf("default project %q", got)
	}
	if got := m.ComposeProject("cache"); got != "legacy-cache" {
		t.Fatalf("override project %q", got)
	}
}

func TestCheckComposeDeployNeedsComposeApp(t *testing.T) {
	m := Manifest{Project: "shop", Apps: map[string]App{
		"db":  {Kind: "compose", File: "compose.yaml"},
		"web": {Kind: "image", Image: "example/web", Roles: map[string]Role{"web": {}}},
	}}
	if err := m.CheckComposeDeploy("db"); err != nil {
		t.Fatalf("compose app refused: %v", err)
	}
	if err := m.CheckComposeDeploy("web"); err == nil {
		t.Fatal("image app accepted as compose")
	}
	if err := m.CheckComposeDeploy("missing"); err == nil {
		t.Fatal("missing app accepted")
	}
}

func TestCheckComposeFileLimits(t *testing.T) {
	exact := []byte(strings.Repeat("x", MaxComposeFile))
	if err := CheckComposeFile(exact); err != nil {
		t.Fatalf("file at the limit refused: %v", err)
	}
	for name, b := range map[string][]byte{
		"empty":      {},
		"too large":  []byte(strings.Repeat("x", MaxComposeFile+1)),
		"not UTF-8":  []byte("services: \xff\n"),
		"NUL byte":   []byte("services:\x00\n"),
		"nul at end": []byte("x\x00"),
	} {
		if err := CheckComposeFile(b); err != ErrComposeFile {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}
