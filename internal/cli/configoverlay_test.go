package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// originBase is valid. The comments say the line each key is on, for the origins and warnings below.
const originBase = `project: demo
network: demo
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    env:
      plain:
        LOG_LEVEL: info
        LEGACY_MODE: "1"
      secret:
        - JWT_SIGNING_KEY
        - DATABASE_URL
    roles:
      web:
        port: 8080
`

// originLine is the line of config show --origin for path, or "" when there is none.
func originLine(out, path string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, path+": ") {
			return line
		}
	}
	return ""
}

// originPlace splits a line of config show --origin into what it says about the value and where it is written. The place is
// the last part, after the padding.
func originPlace(line string) (value, place string) {
	i := strings.LastIndex(line, "  ")
	return strings.TrimRight(line[:i], " "), line[i+2:]
}

func TestConfigShowOriginNamesTheFileAndLineOfEachValue(t *testing.T) {
	dir := validateDir(t, originBase, "apps:\n  api:\n    roles:\n      web:\n        replicas: 2\n")
	var out, errs bytes.Buffer
	code := Run([]string{"config", "show", "-e", "prod", "--manifest-dir", dir, "--origin"}, &out, &errs)
	if code != 0 || errs.Len() != 0 {
		t.Fatalf("exit %d, stderr %q", code, errs.String())
	}
	cases := []struct{ path, value, place string }{
		{"project", "project: demo", "berthy.yml:1"},
		{"apps.api.env.plain.LOG_LEVEL", "apps.api.env.plain.LOG_LEVEL: info", "berthy.yml:9"},
		{"apps.api.env.secret", `apps.api.env.secret: ["JWT_SIGNING_KEY","DATABASE_URL"]`, "berthy.yml:11"},
		{"apps.api.roles.web.replicas", "apps.api.roles.web.replicas: 2", "berthy.prod.yml:5"},
		{"apps.api.deploy.drain_timeout", "apps.api.deploy.drain_timeout: 30s", "default"},
	}
	for _, c := range cases {
		line := originLine(out.String(), c.path)
		if line == "" {
			t.Errorf("%s: no line in\n%s", c.path, out.String())
			continue
		}
		if value, place := originPlace(line); value != c.value || place != c.place {
			t.Errorf("%s: got %q at %q, want %q at %q", c.path, value, place, c.value, c.place)
		}
	}
	// Every place starts in the same column: the values are padded to the longest.
	column := -1
	for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		_, place := originPlace(line)
		if c := len(line) - len(place); column != -1 && c != column {
			t.Fatalf("places are not aligned at %q", line)
		} else {
			column = c
		}
	}
}

func TestConfigShowOriginJSONCarriesFileAndLine(t *testing.T) {
	dir := validateDir(t, originBase, "apps:\n  api:\n    roles:\n      web:\n        replicas: 2\n")
	var out, errs bytes.Buffer
	code := Run([]string{"config", "show", "-e", "prod", "--manifest-dir", dir, "--origin", "--json"}, &out, &errs)
	var got []map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); code != 0 || err != nil || errs.Len() != 0 {
		t.Fatalf("exit %d, %v\nstdout %q\nstderr %q", code, err, out.String(), errs.String())
	}
	byPath := map[string]map[string]any{}
	for _, o := range got {
		byPath[o["path"].(string)] = o
	}
	if o := byPath["apps.api.roles.web.replicas"]; o["file"] != "berthy.prod.yml" || o["line"] != float64(5) || o["value"] != float64(2) {
		t.Errorf("replicas: %v", o)
	}
	if o := byPath["apps.api.deploy.drain_timeout"]; o["value"] != "30s" || o["file"] != nil || o["line"] != nil {
		t.Errorf("a default has no file and no line: %v", o)
	}
	if o := byPath["apps.api.env.secret"]; o["file"] != "berthy.yml" || o["line"] != float64(11) {
		t.Errorf("secret: %v", o)
	}
}

func TestConfigShowShowsAnOverlayNullAsARemovedKey(t *testing.T) {
	dir := validateDir(t, originBase, "apps:\n  api:\n    env:\n      plain:\n        LEGACY_MODE: null\n")
	var out, errs bytes.Buffer
	code := Run([]string{"config", "show", "-e", "prod", "--manifest-dir", dir, "--json"}, &out, &errs)
	if code != 0 || errs.Len() != 0 || strings.Contains(out.String(), "LEGACY_MODE") || !strings.Contains(out.String(), "LOG_LEVEL") {
		t.Fatalf("exit %d, stderr %q\nstdout %s", code, errs.String(), out.String())
	}
}

func TestConfigValidateWarnsWhenAnOverlayListDropsAnEntry(t *testing.T) {
	dir := validateDir(t, originBase, "apps:\n  api:\n    env:\n      secret:\n        - JWT_SIGNING_KEY\n")
	var out, errs bytes.Buffer
	code := Run([]string{"config", "validate", "-e", "prod", "--manifest-dir", dir}, &out, &errs)
	want := "warning: berthy.prod.yml:4:7: apps.api.env.secret: replaces the base list and drops DATABASE_URL\n"
	if code != 0 || out.String() != "manifest valid\n" || errs.String() != want {
		t.Fatalf("exit %d\nstdout %q\nstderr %q\nwant stderr %q", code, out.String(), errs.String(), want)
	}
}
