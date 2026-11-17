package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loggingManifest is a one-role image app whose role "main" has the given logging block, spliced in as YAML.
func loggingManifest(logging string) string {
	return `project: demo
network: demo
apps:
  web:
    kind: image
    image: ghcr.io/acme/web
    roles:
      main:
` + logging
}

func TestLoggingBlockLoadsFromManifest(t *testing.T) {
	dir := t.TempDir()
	input := loggingManifest(`        logging:
          driver: json-file
          options:
            max-size: 50m
            max-file: "5"
`)
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	lg := m.Apps["web"].Roles["main"].Logging
	if lg == nil || lg.Driver != "json-file" || len(lg.Options) != 2 || lg.Options["max-size"] != "50m" || lg.Options["max-file"] != "5" {
		t.Fatalf("logging %+v", lg)
	}
}

func TestLoggingWithoutOptionsAndPluginDriverAreValid(t *testing.T) {
	for _, logging := range []string{
		"        logging:\n          driver: json-file\n",
		"        logging:\n          driver: grafana/loki-docker-driver:latest\n          options: {}\n",
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(loggingManifest(logging)), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir, "dev"); err != nil {
			t.Fatalf("refused %q: %v", logging, err)
		}
	}
}

func TestUnquotedLoggingOptionNumberIsRefused(t *testing.T) {
	dir := t.TempDir()
	input := loggingManifest("        logging:\n          driver: json-file\n          options:\n            max-file: 5\n")
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "dev"); err == nil {
		t.Fatal("an unquoted option number was accepted")
	}
}

func TestInvalidLoggingIsRefused(t *testing.T) {
	long := strings.Repeat("a", 1025)
	cases := []struct {
		name, json, want string
	}{
		{"uppercase driver", `{"driver":"JSON-File"}`, "app web role main: invalid logging"},
		{"missing driver", `{"options":{"max-size":"10m"}}`, "app web role main: invalid logging"},
		{"uppercase option key", `{"driver":"json-file","options":{"Max-Size":"10m"}}`, "app web role main: invalid logging"},
		{"option key with a slash", `{"driver":"json-file","options":{"max/size":"10m"}}`, "app web role main: invalid logging"},
		{"NUL in option value", `{"driver":"json-file","options":{"max-size":"10\u0000m"}}`, "app web role main: invalid logging"},
		{"option value over 1024 bytes", `{"driver":"json-file","options":{"max-size":"` + long + `"}}`, "app web role main: invalid logging"},
		{"logging on a Compose app", "", "app web role main: logging is for image apps"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var data string
			if tc.json == "" {
				data = `{"project":"demo","network":"demo","apps":{"web":{"kind":"compose","file":"compose.yml","roles":{"main":{"logging":{"driver":"json-file"}}}}}}`
			} else {
				data = `{"project":"demo","network":"demo","apps":{"web":{"kind":"image","image":"ghcr.io/acme/web","roles":{"main":{"logging":` + tc.json + `}}}}}`
			}
			_, err := DecodeJSON([]byte(data))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}
