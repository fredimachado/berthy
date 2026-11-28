package manifest

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// schemaOf returns the schema as generic JSON.
func schemaOf(t *testing.T, overlay bool) map[string]any {
	t.Helper()
	b, err := JSONSchema(overlay)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("the schema is not JSON: %v\n%s", err, b)
	}
	return doc
}

// withoutNull is the schema of a value of an overlay without its null branch, which is the value's own schema.
func withoutNull(node map[string]any) map[string]any {
	branches, _ := node["anyOf"].([]any)
	for _, b := range branches {
		if m, ok := b.(map[string]any); ok && m["type"] != "null" {
			return m
		}
	}
	return node
}

// acceptsNull says whether the schema of a value has a null branch.
func acceptsNull(node map[string]any) bool {
	branches, _ := node["anyOf"].([]any)
	for _, b := range branches {
		if m, ok := b.(map[string]any); ok && m["type"] == "null" {
			return true
		}
	}
	return false
}

// at follows names through the schema: a property, "*" for the values of a map, "[]" for the items of a list.
func at(t *testing.T, node map[string]any, path ...string) map[string]any {
	t.Helper()
	for _, p := range path {
		node = withoutNull(node)
		var next any
		switch p {
		case "*":
			next = node["additionalProperties"]
		case "[]":
			next = node["items"]
		default:
			props, _ := node["properties"].(map[string]any)
			next = props[p]
		}
		n, ok := next.(map[string]any)
		if !ok {
			t.Fatalf("the schema has nothing at %v (stopped at %q)", path, p)
		}
		node = n
	}
	return node
}

func stringsOf(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, x := range list {
		out = append(out, x.(string))
	}
	return out
}

func TestJSONSchemaIsDraft07WithTheRequiredKeys(t *testing.T) {
	s := schemaOf(t, false)
	if s["$schema"] != "http://json-schema.org/draft-07/schema#" {
		t.Errorf("$schema is %v", s["$schema"])
	}
	if s["type"] != "object" || s["additionalProperties"] != false {
		t.Errorf("the manifest is not a closed object: %v %v", s["type"], s["additionalProperties"])
	}
	req := stringsOf(s["required"])
	slices.Sort(req)
	if strings.Join(req, ",") != "apps,project" {
		t.Errorf("required is %v", req)
	}
}

func TestJSONSchemaClosesEveryObjectOfKeys(t *testing.T) {
	var walk func(path string, node any)
	walk = func(path string, node any) {
		switch n := node.(type) {
		case map[string]any:
			if _, hasProps := n["properties"]; hasProps && n["additionalProperties"] != false {
				t.Errorf("%s: an object of known keys lets any other key through", path)
			}
			for k, v := range n {
				if k != "if" { // a condition names the keys it tests, and lets the rest pass
					walk(path+"/"+k, v)
				}
			}
		case []any:
			for _, v := range n {
				walk(path+"/[]", v)
			}
		}
	}
	walk("", schemaOf(t, false))
}

func TestJSONSchemaDescribesKeys(t *testing.T) {
	s := schemaOf(t, false)
	kind := at(t, s, "apps", "*", "kind")
	if got := stringsOf(kind["enum"]); strings.Join(got, ",") != "image,compose" {
		t.Errorf("kind enum is %v", got)
	}
	desc, _ := kind["description"].(string)
	if desc == "" || strings.Contains(desc, "`") {
		t.Errorf("kind description is %q", desc)
	}
	deploys := at(t, s, "alerts", "deploys")
	if got := stringsOf(deploys["enum"]); strings.Join(got, ",") != "failed,all,none" || deploys["default"] != "failed" {
		t.Errorf("deploys: %v", deploys)
	}
	retain := at(t, s, "apps", "*", "deploy", "retain")
	if retain["type"] != "integer" || retain["default"] != float64(5) {
		t.Errorf("deploy.retain: %v", retain)
	}
	protected := at(t, s, "protected")
	if protected["type"] != "boolean" || protected["default"] != false {
		t.Errorf("protected: %v", protected)
	}
	names, _ := at(t, s, "apps")["propertyNames"].(map[string]any)
	if names["pattern"] != identifier.String() {
		t.Errorf("app names: %v", names)
	}
	// A default that is only prose is not a default.
	if _, ok := at(t, s, "apps", "*", "project_name")["default"]; ok {
		t.Error("project_name has a default that is a description")
	}
}

func TestJSONSchemaSecretEntries(t *testing.T) {
	secret := at(t, schemaOf(t, false), "apps", "*", "env", "secret")
	if secret["type"] != "array" {
		t.Fatalf("env.secret: %v", secret)
	}
	items, _ := secret["items"].(map[string]any)
	shapes, _ := items["oneOf"].([]any)
	if len(shapes) != 2 {
		t.Fatalf("an entry has %d shapes", len(shapes))
	}
	name, _ := shapes[0].(map[string]any)
	if name["type"] != "string" {
		t.Errorf("first shape: %v", name)
	}
	mapping, _ := shapes[1].(map[string]any)
	if mapping["type"] != "object" || mapping["minProperties"] != float64(1) || mapping["maxProperties"] != float64(1) {
		t.Errorf("second shape is not an object of exactly one key: %v", mapping)
	}
	if v, _ := mapping["additionalProperties"].(map[string]any); v["type"] != "string" {
		t.Errorf("the value of the one key is not a string: %v", mapping)
	}
}

func TestJSONSchemaRetentionKeep(t *testing.T) {
	keep := at(t, schemaOf(t, false), "backups", "*", "retention", "keep")
	shapes, _ := keep["oneOf"].([]any)
	if len(shapes) != 2 {
		t.Fatalf("keep: %v", keep)
	}
	n, _ := shapes[0].(map[string]any)
	if n["type"] != "integer" || n["minimum"] != float64(1) {
		t.Errorf("first shape: %v", n)
	}
	if all, _ := shapes[1].(map[string]any); all["const"] != "all" {
		t.Errorf("second shape: %v", shapes[1])
	}
}

func TestJSONSchemaAppKindsRequireTheirKeys(t *testing.T) {
	app := at(t, schemaOf(t, false), "apps", "*")
	if got := stringsOf(app["required"]); strings.Join(got, ",") != "kind" {
		t.Errorf("an app always requires %v", got)
	}
	required := map[string]string{}
	all, _ := app["allOf"].([]any)
	for _, c := range all {
		c := c.(map[string]any)
		cond := c["if"].(map[string]any)["properties"].(map[string]any)["kind"].(map[string]any)["const"].(string)
		required[cond] = strings.Join(stringsOf(c["then"].(map[string]any)["required"]), ",")
	}
	if required["image"] != "image,roles" || required["compose"] != "file" {
		t.Errorf("by kind: %v", required)
	}
}

func TestJSONSchemaListsAndMaps(t *testing.T) {
	s := schemaOf(t, false)
	if at(t, s, "apps", "*", "hooks", "pre_deploy")["type"] != "array" {
		t.Error("pre_deploy is not a list")
	}
	if at(t, s, "apps", "*", "hooks", "pre_deploy", "[]", "run")["type"] != "array" {
		t.Error("a run hook is not a list")
	}
	opts := at(t, s, "apps", "*", "roles", "*", "logging", "options")
	if v, _ := opts["additionalProperties"].(map[string]any); v["type"] != "string" {
		t.Errorf("logging options: %v", opts)
	}
	if at(t, s, "peers", "*")["type"] != "string" {
		t.Error("peers are not strings")
	}
}

func TestJSONSchemaOverlayRequiresNothing(t *testing.T) {
	var walk func(path string, node any)
	walk = func(path string, node any) {
		switch n := node.(type) {
		case map[string]any:
			props, _ := n["properties"].(map[string]any)
			for k, v := range n {
				// A key of the manifest may be called required; a keyword may not appear.
				if _, isKey := props[k]; (k == "required" || k == "allOf" || k == "if") && !isKey {
					t.Errorf("%s: the overlay schema holds %q", path, k)
				}
				walk(path+"/"+k, v)
			}
		case []any:
			for _, v := range n {
				walk(path+"/[]", v)
			}
		}
	}
	overlay := schemaOf(t, true)
	walk("", overlay)
	if withoutNull(at(t, overlay, "project"))["type"] != "string" {
		t.Error("the overlay schema lost project")
	}
}

// A null in an overlay removes its key, so every value there is its own schema or null. The manifest's values are not.
func TestJSONSchemaOverlayAcceptsNullForEveryKey(t *testing.T) {
	overlay := schemaOf(t, true)
	for _, path := range [][]string{{"project"}, {"apps"}, {"apps", "*", "env", "plain", "*"}, {"apps", "*", "roles", "*", "replicas"}} {
		if !acceptsNull(at(t, overlay, path...)) {
			t.Errorf("%v: the overlay schema refuses null", path)
		}
	}
	if acceptsNull(at(t, schemaOf(t, false), "project")) {
		t.Error("the manifest schema accepts null")
	}
}

func TestUptimeHealthSchemaAlternatives(t *testing.T) {
	path := []string{"apps", "*", "roles", "*", "health"}
	health := at(t, schemaOf(t, false), path...)
	if len(stringsOf(health["required"])) != 0 {
		t.Errorf("health requires a specific form: %v", health["required"])
	}
	forms, _ := health["oneOf"].([]any)
	var required []string
	for _, form := range forms {
		branch := stringsOf(form.(map[string]any)["required"])
		if len(branch) != 1 {
			t.Fatalf("health branch should require exactly one field: %v", form)
		}
		required = append(required, branch[0])
	}
	slices.Sort(required)
	forms := health["oneOf"].([]any)
	if got := forms[1].(map[string]any)["maxProperties"]; got != float64(1) {
		t.Errorf("uptime maxProperties = %v, want 1", got)
	}
	command := forms[0].(map[string]any)
	constraint, _ := command["not"].(map[string]any)
	if excluded, _ := constraint["required"].([]any); len(excluded) != 1 || excluded[0] != "uptime" {
		t.Errorf("command branch accepts uptime: %v", command)
	}
	if !slices.Equal(required, []string{"test", "uptime"}) {
		t.Fatalf("health alternatives = %v, want test and uptime", required)
	}
	if test := at(t, health, "test"); test["type"] != "array" || test["minItems"] != float64(1) {
		t.Errorf("health.test should require a nonempty array: %v", test)
	}
	if uptime := at(t, health, "uptime"); uptime["type"] != "string" || uptime["minLength"] != float64(1) {
		t.Errorf("health.uptime should require a nonempty string: %v", uptime)
	}
	overlay := at(t, schemaOf(t, true), path...)
	if withoutNull(overlay)["oneOf"] != nil || !acceptsNull(at(t, overlay, "test")) || withoutNull(at(t, overlay, "uptime"))["type"] != "string" {
		t.Errorf("overlay should accept deleting test and setting uptime together: %v", overlay)
	}
}

func TestJSONSchemaIsStable(t *testing.T) {
	a, _ := JSONSchema(false)
	b, _ := JSONSchema(false)
	if string(a) != string(b) {
		t.Error("two runs differ")
	}
	if !strings.Contains(string(a), "\n  \"") || !strings.HasSuffix(string(a), "\n") {
		t.Error("the schema is not indented and newline-terminated")
	}
}
