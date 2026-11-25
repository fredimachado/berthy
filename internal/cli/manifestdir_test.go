package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestDirectoryDiscovery(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("BERTHY_CONFIG", filepath.Join(root, "config.json"))
	if err := os.Mkdir(".berthy", 0o700); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, ".berthy", "hidden-network")
	overlay := []byte("aliases:\n  inspect-config: config show\n")
	if err := os.WriteFile(filepath.Join(".berthy", "berthy.prod.yml"), overlay, 0o600); err != nil {
		t.Fatal(err)
	}
	check := func(want string, args ...string) {
		t.Helper()
		code, out, errs := runCLI(append([]string{"config", "show", "-e", "prod"}, args...)...)
		if want == "" {
			if code == 0 {
				t.Fatalf("unexpected manifest: %s", out)
			}
		} else if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("want %s, exit %d: %s %s", want, code, out, errs)
		}
	}
	check("hidden-network")
	if code, out, errs := runCLI("inspect-config", "-e", "prod"); code != 0 || !strings.Contains(out, "hidden-network") {
		t.Fatalf("nested alias: exit %d: %s %s", code, out, errs)
	}
	check("", "--manifest-dir", ".")
	writeManifest(t, ".", "root-network")
	check("root-network")
	if code, _, _ := runCLI("inspect-config", "-e", "prod"); code != 2 {
		t.Fatalf("root manifest used the hidden overlay: exit %d", code)
	}
	check("hidden-network", "--manifest-dir", ".berthy")
	if err := os.Mkdir("child", 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir("child")
	check("")
	t.Chdir(root)
	if err := os.WriteFile("berthy.yml", []byte("bad: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	check("")
}
