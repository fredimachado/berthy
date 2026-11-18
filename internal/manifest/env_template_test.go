package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestTemplateRefsListsTheSecretsInOrderOfFirstUse: ${NAME} names a secret, $$ is one dollar sign and writes no reference.
func TestTemplateRefsListsTheSecretsInOrderOfFirstUse(t *testing.T) {
	for _, tc := range []struct {
		template string
		want     []string
	}{
		{"postgres://u:${PW}@db/x", []string{"PW"}},
		{"${B}-${A}-${B}", []string{"B", "A"}},
		{"cost: $$5 and ${_x1}", []string{"_x1"}},
		{"no references, $$${A}$$", []string{"A"}},
		{"plain text", nil},
		{"", nil},
	} {
		got, err := TemplateRefs(tc.template)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q gave %v, %v; want %v", tc.template, got, err, tc.want)
		}
	}
}

// TestTemplateRefsRefusesAMalformedReference: every other use of a dollar sign is an error, and the error says where it is.
func TestTemplateRefsRefusesAMalformedReference(t *testing.T) {
	for _, tc := range []struct{ template, offset string }{
		{"a$x", "offset 1"},
		{"abc$", "offset 3"},
		{"ab${}", "offset 2"},
		{"ab${a-b}", "offset 2"},
		{"ab${1a}", "offset 2"},
		{"x ${PW", "offset 2"},
		{"${A:-d}", "offset 0"},
		{"${A}$ ", "offset 4"},
	} {
		_, err := TemplateRefs(tc.template)
		if err == nil || !strings.Contains(err.Error(), tc.offset) {
			t.Errorf("%q gave %v, want an error at %s", tc.template, err, tc.offset)
		}
	}
}

// TestResolveBuildsTheContainersVariables: plain values stay, a secret gives its value, a template is filled in; secret and
// template values are sensitive. The variables come sorted by name.
func TestResolveBuildsTheContainersVariables(t *testing.T) {
	env := Env{
		Plain:    map[string]string{"LOG_LEVEL": "info"},
		Secret:   []SecretVar{{Var: "JWT", Secret: "JWT_KEY"}},
		Template: map[string]string{"DATABASE_URL": "postgres://u:${PW}@db/$$x"},
	}
	values := map[string]string{"JWT_KEY": "k", "PW": "p"}
	got, err := env.Resolve(func(name string) ([]byte, error) { return []byte(values[name]), nil })
	if err != nil {
		t.Fatal(err)
	}
	want := []EnvVar{
		{Name: "DATABASE_URL", Value: "postgres://u:p@db/$x", Sensitive: true},
		{Name: "JWT", Value: "k", Sensitive: true},
		{Name: "LOG_LEVEL", Value: "info"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// TestResolveNamesTheSecretItCannotRead: a secret that cannot be read, in env.secret or in a template, is an error that names
// it and keeps the reason.
func TestResolveNamesTheSecretItCannotRead(t *testing.T) {
	missing := errors.New("not stored")
	get := func(name string) ([]byte, error) {
		if name == "GONE" {
			return nil, missing
		}
		return []byte("v"), nil
	}
	for _, env := range []Env{
		{Secret: []SecretVar{{Var: "X", Secret: "GONE"}}},
		{Template: map[string]string{"X": "a${OK}${GONE}"}},
	} {
		if _, err := env.Resolve(get); err == nil || !strings.Contains(err.Error(), "GONE") || !errors.Is(err, missing) {
			t.Errorf("%+v: %v", env, err)
		}
	}
}

// TestSecretNamesIncludesTheTemplatesReferences: a secret read only by a template is still a secret the app needs.
func TestSecretNamesIncludesTheTemplatesReferences(t *testing.T) {
	env := Env{
		Secret:   []SecretVar{{Var: "A", Secret: "S1"}},
		Template: map[string]string{"X": "${S2}${S1}", "Y": "${S3}"},
	}
	if got, want := env.SecretNames(), []string{"S1", "S2", "S3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, want := env.SensitiveVars(), []string{"A", "X", "Y"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sensitive variables %v, want %v", got, want)
	}
}

// loadTemplateManifest loads a manifest whose app api has the given env block (indented under "env:").
func loadTemplateManifest(t *testing.T, env string) (*Manifest, error) {
	t.Helper()
	input := `project: demo
network: demo
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    roles:
      web: {}
    env:
` + env
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	return Load(dir, "dev")
}

// TestAVariableIsInOneSectionOnly: a name in two of plain, secret and template is refused, and the message names the app.
func TestAVariableIsInOneSectionOnly(t *testing.T) {
	for name, env := range map[string]string{
		"template and plain":  "      plain:\n        A: x\n      template:\n        A: \"${B}\"\n",
		"template and secret": "      secret:\n        - A\n      template:\n        A: \"${B}\"\n",
		"template and mapped": "      secret:\n        - A: C\n      template:\n        A: \"${B}\"\n",
	} {
		_, err := loadTemplateManifest(t, env)
		if err == nil || !strings.Contains(err.Error(), "app api: duplicate environment name") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := loadTemplateManifest(t, "      plain:\n        A: x\n      secret:\n        - B\n      template:\n        C: \"${B}\"\n"); err != nil {
		t.Errorf("distinct variables: %v", err)
	}
}

// TestAMalformedTemplateIsRefusedWithItsLocation: Load places the problem at the template's line.
func TestAMalformedTemplateIsRefusedWithItsLocation(t *testing.T) {
	_, err := loadTemplateManifest(t, "      template:\n        OK: \"${A}\"\n        BAD: \"x${A-b}\"\n")
	var ps Problems
	if !errors.As(err, &ps) || len(ps) != 1 {
		t.Fatalf("got %v", err)
	}
	p := ps[0]
	if p.Path != "apps.api.env.template.BAD" || p.File != "berthy.yml" || p.Line != 12 || !strings.Contains(p.Message, "offset 1") {
		t.Fatalf("problem %+v", p)
	}
}

// TestATemplateChecksItsVariableAndItsSecrets: the variable follows the rules of any variable, and a reference follows the
// rules of a secret name, including the reserved ones.
func TestATemplateChecksItsVariableAndItsSecrets(t *testing.T) {
	for name, env := range map[string]string{
		"reserved variable":   "      template:\n        BERTHY_X: \"${A}\"\n",
		"bad variable":        "      template:\n        1X: \"${A}\"\n",
		"tunnel token secret": "      template:\n        X: \"${TUNNEL_TOKEN}\"\n",
		"NUL":                 "      template:\n        X: \"a\\0${A}\"\n",
	} {
		if _, err := loadTemplateManifest(t, env); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestSecretNeedsNamesATemplatesSecrets: secret list --manifest-dir reports a secret only a template reads.
func TestSecretNeedsNamesATemplatesSecrets(t *testing.T) {
	m, err := loadTemplateManifest(t, "      template:\n        DATABASE_URL: \"postgres://u:${DB_PW}@db/x\"\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []SecretNeed{{Name: "DB_PW", NeededBy: []string{"app api"}}}
	if got := m.SecretNeeds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
