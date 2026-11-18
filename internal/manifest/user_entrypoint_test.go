package manifest

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func loadRoleManifest(t *testing.T, role string) (*Manifest, error) {
	t.Helper()
	dir := t.TempDir()
	input := "project: demo\nnetwork: demo\napps:\n  web:\n    kind: image\n    image: ghcr.io/acme/web\n    roles:\n      main:\n" + role
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	return Load(dir, "dev")
}

func TestRoleUserAndEntrypointLoad(t *testing.T) {
	m, err := loadRoleManifest(t, "        user: \"1000:1000\"\n        entrypoint: [\"/bin/app\", \"--serve\"]\n        command: [\"--port\", \"80\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	r := m.Apps["web"].Roles["main"]
	if r.User != "1000:1000" || !reflect.DeepEqual(r.Entrypoint, []string{"/bin/app", "--serve"}) || !reflect.DeepEqual(r.Command, []string{"--port", "80"}) {
		t.Fatalf("role %+v", r)
	}
}

func TestNameAndNumericUsersLoad(t *testing.T) {
	for _, user := range []string{"app", "1000", "app:staff", "1000:1000"} {
		if _, err := loadRoleManifest(t, "        user: \""+user+"\"\n"); err != nil {
			t.Errorf("user %q refused: %v", user, err)
		}
	}
}

func TestInvalidUserOrEntrypointIsRefused(t *testing.T) {
	for name, role := range map[string]string{
		"user with a space":    "        user: \"a b\"\n",
		"user with empty gid":  "        user: \"1000:\"\n",
		"user with three ids":  "        user: \"a:b:c\"\n",
		"entrypoint empty exe": "        entrypoint: [\"\"]\n",
	} {
		if _, err := loadRoleManifest(t, role); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
