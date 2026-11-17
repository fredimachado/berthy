package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"berthy/internal/manifest"
)

// runCLI runs the CLI with args and returns its exit code and what it printed to each stream.
func runCLI(args ...string) (code int, out, errs string) {
	var o, e bytes.Buffer
	code = Run(args, &o, &e)
	return code, o.String(), e.String()
}

func TestDocsPrintsTheWholeReference(t *testing.T) {
	// It runs where there is no manifest, no agent and no CLI configuration.
	t.Chdir(t.TempDir())
	t.Setenv("BERTHY_CONFIG", t.TempDir()+"/missing.json")
	code, out, errs := runCLI("docs")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	for _, want := range []string{"project", "network", "protected", "apps.<name>", "retention.keep", "alerts"} {
		if !strings.Contains(out, want) {
			t.Errorf("berthy docs does not mention %s", want)
		}
	}
}

func TestDocsPrintsOneSection(t *testing.T) {
	code, out, errs := runCLI("docs", "backups")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	if !strings.Contains(out, "retention.keep") || !strings.Contains(out, "destination") {
		t.Errorf("berthy docs backups lacks the backups keys:\n%s", out)
	}
	for _, other := range []string{"bot_token_secret", "deploy_timeout", "access_key_secret"} {
		if strings.Contains(out, other) {
			t.Errorf("berthy docs backups prints %s, a key of another section", other)
		}
	}
}

func TestDocsRefusesAnUnknownSectionAndListsTheSections(t *testing.T) {
	code, out, errs := runCLI("docs", "nope")
	if code != 2 || out != "" {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	for _, name := range manifest.SectionNames() {
		if !strings.Contains(errs, name) {
			t.Errorf("the error does not list %s: %q", name, errs)
		}
	}
}

func TestDocsRefusesWhatItDoesNotTake(t *testing.T) {
	for _, args := range [][]string{{"docs", "--json"}, {"docs", "-e", "dev"}, {"docs", "apps", "jobs"}} {
		code, out, errs := runCLI(args...)
		if code != 2 || out != "" || errs == "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, out, errs)
		}
	}
}

func TestConfigSchemaNeedsNoEnvironment(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("BERTHY_CONFIG", t.TempDir()+"/missing.json")
	code, out, errs := runCLI("config", "schema")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if doc["$schema"] != "http://json-schema.org/draft-07/schema#" {
		t.Errorf("$schema is %v", doc["$schema"])
	}
	if _, ok := doc["required"]; !ok {
		t.Error("the manifest schema requires nothing")
	}
}

func TestConfigSchemaOverlayRequiresNothing(t *testing.T) {
	code, out, errs := runCLI("config", "schema", "--overlay")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if _, ok := doc["required"]; ok {
		t.Error("the overlay schema has required keys")
	}
	_, whole, _ := runCLI("config", "schema")
	if whole == out {
		t.Error("--overlay changed nothing")
	}
}

func TestConfigSchemaRefusesWhatItDoesNotTake(t *testing.T) {
	for _, args := range [][]string{{"config", "schema", "-e", "dev"}, {"config", "schema", "--json"}, {"config", "schema", "extra"}} {
		code, out, errs := runCLI(args...)
		if code != 2 || out != "" || errs == "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, out, errs)
		}
	}
}
