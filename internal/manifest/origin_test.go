package manifest

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// originsOf loads prod from files and returns the origins of its values by path.
func originsOf(t *testing.T, files map[string]string) map[string]Origin {
	t.Helper()
	l, err := LoadDetailed(writeFiles(t, files), "prod")
	if err != nil {
		t.Fatal(err)
	}
	origins, err := l.Origins()
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]Origin{}
	for _, o := range origins {
		byPath[o.Path] = o
	}
	return byPath
}

func TestOriginsNameTheFileAndLineOfEachValue(t *testing.T) {
	origins := originsOf(t, map[string]string{
		"berthy.yml":      overlayBase,
		"berthy.prod.yml": "apps:\n  api:\n    roles:\n      web:\n        replicas: 2\n        port: 9090\n",
	})
	cases := []struct {
		path  string
		value any
		file  string
		line  int
	}{
		{"project", "demo", "berthy.yml", 1},
		{"apps.api.env.plain.LOG_LEVEL", "info", "berthy.yml", 9},
		{"apps.api.roles.web.replicas", json.Number("2"), "berthy.prod.yml", 5},
		{"apps.api.roles.web.port", json.Number("9090"), "berthy.prod.yml", 6},
		{"apps.api.deploy.drain_timeout", "30s", "", 0},
		{"apps.api.deploy.retain", json.Number("5"), "", 0},
	}
	for _, c := range cases {
		o, ok := origins[c.path]
		if !ok || !reflect.DeepEqual(o.Value, c.value) || o.File != c.file || o.Line != c.line {
			t.Errorf("%s: got %+v, want value %v at %q:%d", c.path, o, c.value, c.file, c.line)
		}
	}
}

func TestOriginsOfAListIsOneValue(t *testing.T) {
	origins := originsOf(t, map[string]string{"berthy.yml": overlayBase})
	secret, ok := origins["apps.api.env.secret"]
	if !ok || !reflect.DeepEqual(secret.Value, []any{"JWT_SIGNING_KEY", map[string]any{"DATABASE_URL": "DB"}}) ||
		secret.File != "berthy.yml" || secret.Line != 11 {
		t.Fatalf("got %+v", secret)
	}
	for path := range origins {
		if strings.HasPrefix(path, "apps.api.env.secret.") {
			t.Fatalf("a list's items are not values of their own: %s", path)
		}
	}
}

func TestOriginsOfAKeyWithDotsAreLocated(t *testing.T) {
	origins := originsOf(t, map[string]string{
		"berthy.yml": overlayBase + "registries:\n  ghcr.io:\n    username: bot\n    password_secret: P\n",
	})
	if o := origins["registries.ghcr.io.username"]; o.File != "berthy.yml" || o.Line != 19 {
		t.Fatalf("got %+v", o)
	}
}

func TestOriginsDoNotListARemovedKey(t *testing.T) {
	origins := originsOf(t, map[string]string{
		"berthy.yml":      overlayBase,
		"berthy.prod.yml": "apps:\n  api:\n    env:\n      plain:\n        LEGACY_MODE: null\n",
	})
	if _, ok := origins["apps.api.env.plain.LEGACY_MODE"]; ok {
		t.Fatal("a removed key is still listed")
	}
	if o := origins["apps.api.env.plain.LOG_LEVEL"]; o.File != "berthy.yml" || o.Line != 9 {
		t.Fatalf("got %+v", o)
	}
}

func TestOriginsAreSortedByPath(t *testing.T) {
	l, err := LoadDetailed(writeFiles(t, map[string]string{"berthy.yml": overlayBase}), "prod")
	if err != nil {
		t.Fatal(err)
	}
	origins, err := l.Origins()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(origins); i++ {
		if origins[i-1].Path >= origins[i].Path {
			t.Fatalf("%s is not before %s", origins[i-1].Path, origins[i].Path)
		}
	}
}
