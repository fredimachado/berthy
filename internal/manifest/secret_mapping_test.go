package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// secretApp is a manifest with one image app whose env.secret is the JSON array secret.
func secretApp(secret string) string {
	return `{"project":"demo","network":"demo","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}},"env":{"secret":` + secret + `}}}}`
}

// TestSecretEntryIsANameOrAOneKeyMapping: a name is both the variable and the secret; a one-key map names the variable and
// the secret apart. Both kinds of entry come in the order the manifest lists them.
func TestSecretEntryIsANameOrAOneKeyMapping(t *testing.T) {
	input := `project: demo
network: demo
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    roles:
      web: {}
    env:
      secret:
        - JWT_SIGNING_KEY
        - DATABASE_URL: API_DATABASE_URL
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	want := []SecretVar{{Var: "JWT_SIGNING_KEY", Secret: "JWT_SIGNING_KEY"}, {Var: "DATABASE_URL", Secret: "API_DATABASE_URL"}}
	if got := m.Apps["api"].Env.Secret; !reflect.DeepEqual(got, want) {
		t.Fatalf("secret entries %+v, want %+v", got, want)
	}
}

// TestSecretEntryJSONKeepsBothForms: a name is written as the bare name, so a manifest without a mapping keeps the stored
// form it had before mappings existed. A mapping is written as its one-key object. Both decode back to the same entries.
func TestSecretEntryJSONKeepsBothForms(t *testing.T) {
	for _, in := range []string{`["JWT_SIGNING_KEY"]`, `[{"DATABASE_URL":"API_DATABASE_URL"}]`, `["A",{"B":"C"}]`} {
		var got []SecretVar
		if err := json.Unmarshal([]byte(in), &got); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		out, err := json.Marshal(got)
		if err != nil || string(out) != in {
			t.Errorf("%s came back as %s (%v)", in, out, err)
		}
	}
	if out, _ := json.Marshal(SecretVar{Var: "A", Secret: "A"}); string(out) != `"A"` {
		t.Errorf("a variable of its own secret marshals as %s", out)
	}
}

// TestStoredSecretEntriesDecode: a release stored before mappings existed still decodes, and so does one stored with a
// mapping.
func TestStoredSecretEntriesDecode(t *testing.T) {
	for _, tc := range []struct {
		secret string
		want   []SecretVar
	}{
		{`["DATABASE_URL"]`, []SecretVar{{Var: "DATABASE_URL", Secret: "DATABASE_URL"}}},
		{`[{"DATABASE_URL":"API_DATABASE_URL"}]`, []SecretVar{{Var: "DATABASE_URL", Secret: "API_DATABASE_URL"}}},
	} {
		m, err := DecodeRelease([]byte(secretApp(tc.secret)), false)
		if err != nil {
			t.Fatalf("%s: %v", tc.secret, err)
		}
		if got := m.Apps["api"].Env.Secret; !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s decoded as %+v, want %+v", tc.secret, got, tc.want)
		}
	}
}

// TestSecretEntryRefusesOtherShapes: an entry is a name or a one-key object of a name to a name. Anything else is refused
// with the rule that says what an entry is.
func TestSecretEntryRefusesOtherShapes(t *testing.T) {
	const message = "env.secret entry must be a name or VARIABLE: SECRET"
	for _, secret := range []string{`[5]`, `[true]`, `[["A"]]`, `[{}]`, `[{"A":"B","C":"D"}]`, `[{"A":1}]`, `[{"A":["B"]}]`, `[{"":"B"}]`} {
		if _, err := DecodeJSON([]byte(secretApp(secret))); err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("%s: %v", secret, err)
		}
	}
	yamlInput := `project: demo
network: demo
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    roles:
      web: {}
    env:
      secret:
        - DATABASE_URL: [API_DATABASE_URL]
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(yamlInput), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "dev"); err == nil || !strings.Contains(err.Error(), message) {
		t.Errorf("YAML list as the secret: %v", err)
	}
}

// TestSecretVariableIsListedOnce: a variable may not be listed twice, whether as a name and a mapping or as two mappings.
func TestSecretVariableIsListedOnce(t *testing.T) {
	for _, secret := range []string{`["DATABASE_URL",{"DATABASE_URL":"API_DB"}]`, `[{"DATABASE_URL":"WEB_DB"},{"DATABASE_URL":"API_DB"}]`} {
		if _, err := DecodeJSON([]byte(secretApp(secret))); err == nil || !strings.Contains(err.Error(), "variable DATABASE_URL is listed twice") {
			t.Errorf("%s: %v", secret, err)
		}
	}
}

// TestTwoVariablesMayReadOneSecret: two variables can take the value of the same secret.
func TestTwoVariablesMayReadOneSecret(t *testing.T) {
	if _, err := DecodeJSON([]byte(secretApp(`[{"DATABASE_URL":"DB"},{"DB_HOST":"DB"}]`))); err != nil {
		t.Fatalf("refused: %v", err)
	}
}

// TestSecretMappingNamesFollowTheirRules: the variable follows the rules of a name in env.plain, the secret those of a secret
// name, and a variable of env.plain cannot also be a secret variable. The reserved names stay reserved as variables.
func TestSecretMappingNamesFollowTheirRules(t *testing.T) {
	for _, tc := range []struct {
		secret, message string
	}{
		{`[{"DATABASE_URL":"bad-name"}]`, "invalid secret"},
		{`[{"1BAD":"DB"}]`, "invalid secret"},
		{`[{"BERTHY_X":"DB"}]`, "names starting with BERTHY_ are reserved for Berthy"},
		{`[{"DATABASE_URL":"TUNNEL_TOKEN"}]`, "TUNNEL_TOKEN is reserved for managed ingress"},
		{`[{"TUNNEL_TOKEN":"DB"}]`, "TUNNEL_TOKEN is reserved for managed ingress"},
	} {
		if _, err := DecodeJSON([]byte(secretApp(tc.secret))); err == nil || !strings.Contains(err.Error(), tc.message) {
			t.Errorf("%s: %v, want %q", tc.secret, err, tc.message)
		}
	}
	plain := `{"project":"demo","network":"demo","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}},"env":{"plain":{"DATABASE_URL":"x"},"secret":[{"DATABASE_URL":"DB"}]}}}}`
	if _, err := DecodeJSON([]byte(plain)); err == nil || !strings.Contains(err.Error(), "duplicate environment name") {
		t.Errorf("variable also in plain: %v", err)
	}
}
