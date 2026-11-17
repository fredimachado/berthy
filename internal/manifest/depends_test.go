package manifest

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// dependsYAML is a manifest of image apps, one per name in deps, each depending on the apps listed for it. A nil list writes no
// depends_on key.
func dependsYAML(deps map[string][]string) string {
	var b strings.Builder
	b.WriteString("project: demo\nnetwork: demo\napps:\n")
	for _, name := range sortedKeys(deps) {
		b.WriteString("  " + name + ":\n    kind: image\n    image: ghcr.io/acme/" + name + "\n")
		if deps[name] != nil {
			b.WriteString("    depends_on: [" + strings.Join(deps[name], ", ") + "]\n")
		}
		b.WriteString("    roles:\n      web: {}\n")
	}
	return b.String()
}

func loadDepends(t *testing.T, base, overlay string) (*Manifest, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	if overlay != "" {
		if err := os.WriteFile(filepath.Join(dir, "berthy.prod.yml"), []byte(overlay), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return Load(dir, "prod")
}

func TestDeployOrder(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps map[string][]string
		want []string
	}{
		{"no dependencies runs by name", map[string][]string{"redis": nil, "api": nil, "postgres": nil}, []string{"api", "postgres", "redis"}},
		{"an app waits for what it depends on", map[string][]string{"redis": nil, "postgres": nil, "api": {"postgres", "redis"}}, []string{"postgres", "redis", "api"}},
		{"a chain", map[string][]string{"a": nil, "b": {"a"}, "c": {"b"}}, []string{"a", "b", "c"}},
		{"a chain named against the alphabet", map[string][]string{"a": {"b"}, "b": {"c"}, "c": nil}, []string{"c", "b", "a"}},
		{"a diamond", map[string][]string{"top": {"left", "right"}, "left": {"base"}, "right": {"base"}, "base": nil}, []string{"base", "left", "right", "top"}},
		{"an app with no dependencies does not wait for a later name", map[string][]string{"a": {"z"}, "m": nil, "z": nil}, []string{"m", "z", "a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := loadDepends(t, dependsYAML(tc.deps), "")
			if err != nil {
				t.Fatal(err)
			}
			if got := m.DeployOrder(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("DeployOrder() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDependsOnIsValidated(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps map[string][]string
		want string
	}{
		{"unknown app", map[string][]string{"api": {"cache"}}, `apps.api.depends_on: unknown app "cache"`},
		{"itself", map[string][]string{"api": {"api"}}, "apps.api.depends_on: an app cannot depend on itself"},
		{"duplicate", map[string][]string{"api": {"db", "db"}, "db": nil}, `apps.api.depends_on: duplicate app "db"`},
		{"cycle of two", map[string][]string{"api": {"worker"}, "worker": {"api"}}, "depends_on cycle: api -> worker -> api"},
		{"cycle that starts after a chain", map[string][]string{"a": {"c"}, "b": {"c"}, "c": {"d"}, "d": {"b"}}, "depends_on cycle: b -> c -> d -> b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadDepends(t, dependsYAML(tc.deps), "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}

func TestDependsOnAcceptsAComposeApp(t *testing.T) {
	base := dependsYAML(map[string][]string{"api": {"stack"}}) + "  stack:\n    kind: compose\n    file: compose.yml\n"
	m, err := loadDepends(t, base, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := m.DeployOrder(); !reflect.DeepEqual(got, []string{"stack", "api"}) {
		t.Fatalf("DeployOrder() = %v", got)
	}
}

func TestOverlayReplacesDependsOn(t *testing.T) {
	base := dependsYAML(map[string][]string{"api": {"db"}, "db": nil, "cache": nil})
	m, err := loadDepends(t, base, "apps:\n  api:\n    depends_on: [cache]\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Apps["api"].DependsOn; !reflect.DeepEqual(got, []string{"cache"}) {
		t.Fatalf("overlay did not replace the list: %v", got)
	}
	if got := m.DeployOrder(); !reflect.DeepEqual(got, []string{"cache", "api", "db"}) {
		t.Fatalf("DeployOrder() = %v", got)
	}
}

func TestOverlayCanMakeADependsOnCycle(t *testing.T) {
	base := dependsYAML(map[string][]string{"api": {"db"}, "db": nil})
	if _, err := loadDepends(t, base, "apps:\n  db:\n    depends_on: [api]\n"); err == nil || !strings.Contains(err.Error(), "depends_on cycle: api -> db -> api") {
		t.Fatalf("error %v", err)
	}
}

// TestTodoExampleDeploysItsDatabaseAndCacheBeforeTheAPI: the example the guide walks through is valid and ordered.
func TestTodoExampleDeploysItsDatabaseAndCacheBeforeTheAPI(t *testing.T) {
	for _, env := range []string{"dev", "prod"} {
		m, err := Load(filepath.Join("..", "..", "examples", "todo"), env)
		if err != nil {
			t.Fatalf("%s: %v", env, err)
		}
		if got := m.DeployOrder(); !reflect.DeepEqual(got, []string{"postgres", "redis", "api"}) {
			t.Fatalf("%s: DeployOrder() = %v", env, got)
		}
	}
}
