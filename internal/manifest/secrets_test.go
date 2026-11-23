package manifest

import (
	"reflect"
	"strings"
	"testing"
)

// declaredYAML is a minimal image app that can read a secret, plus an optional secrets block.
func declaredYAML(envSecret, secretsBlock string) string {
	var b strings.Builder
	b.WriteString("project: demo\nnetwork: demo\napps:\n  app:\n    kind: image\n    image: alpine\n    roles:\n      main: {}\n")
	if envSecret != "" {
		b.WriteString("    env:\n      " + envSecret + "\n")
	}
	if secretsBlock != "" {
		b.WriteString(secretsBlock)
		if !strings.HasSuffix(secretsBlock, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func TestDeclaredSecretProblems(t *testing.T) {
	valid := declaredYAML("secret: [GEN]", `secrets:
  GEN:
    generate:
      format: hex
      bytes: 32
`)
	for _, tc := range []struct {
		name string
		body string
		want string // empty: valid
	}{
		{"valid", valid, ""},
		{"invalid name", declaredYAML("secret: [GEN]", "secrets:\n  \"1BAD\":\n    generate:\n      format: hex\n"), "secrets.1BAD: invalid secret name"},
		{"reserved name", declaredYAML("", "secrets:\n  TUNNEL_TOKEN:\n    generate:\n      format: hex\n"), "secrets.TUNNEL_TOKEN: TUNNEL_TOKEN is reserved for managed ingress"},
		{"no generate", declaredYAML("secret: [GEN]", "secrets:\n  GEN: {}\n"), "secrets.GEN: generate is required"},
		{"bad format", declaredYAML("secret: [GEN]", "secrets:\n  GEN:\n    generate:\n      format: uuid\n"), "secrets.GEN.generate.format: must be hex, base64 or alnum"},
		{"bytes too small", declaredYAML("secret: [GEN]", "secrets:\n  GEN:\n    generate:\n      format: hex\n      bytes: 15\n"), "secrets.GEN.generate.bytes: must be from 16 to 256"},
		{"bytes too large", declaredYAML("secret: [GEN]", "secrets:\n  GEN:\n    generate:\n      format: hex\n      bytes: 257\n"), "secrets.GEN.generate.bytes: must be from 16 to 256"},
		{"needed by registry", `project: demo
network: demo
registries:
  ghcr.io:
    username: bot
    password_secret: REG_PASS
apps:
  app:
    kind: image
    image: alpine
    roles:
      main: {}
    env:
      secret: [REG_PASS]
secrets:
  REG_PASS:
    generate:
      format: hex
`, "secrets.REG_PASS.generate: REG_PASS is needed by registry ghcr.io and cannot be generated"},
		{"no reader", declaredYAML("secret: [OTHER]", `secrets:
  GEN:
    generate:
      format: hex
`), "secrets.GEN: no app reads GEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeFiles(t, map[string]string{"berthy.yml": tc.body})
			m, err := Load(dir, "dev")
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid manifest refused: %v", err)
				}
				if m.Secrets["GEN"].Generate == nil || m.Secrets["GEN"].Generate.Format != "hex" {
					t.Fatalf("secrets not loaded: %+v", m.Secrets)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSecretReaders(t *testing.T) {
	m := Manifest{
		Apps: map[string]App{
			"plain":    {Env: Env{Secret: []SecretVar{{Var: "A", Secret: "A"}}}},
			"mapped":   {Env: Env{Secret: []SecretVar{{Var: "DB_URL", Secret: "DATABASE_URL"}}}},
			"template": {Env: Env{Template: map[string]string{"DSN": "postgres://${DATABASE_URL}@db/app"}}},
			"none":     {Env: Env{Plain: map[string]string{"LOG": "info"}}},
		},
	}
	if got := m.SecretReaders("A"); !reflect.DeepEqual(got, []string{"plain"}) {
		t.Fatalf("plain entry: %v", got)
	}
	if got := m.SecretReaders("DATABASE_URL"); !reflect.DeepEqual(got, []string{"mapped", "template"}) {
		t.Fatalf("mapping and template: %v", got)
	}
	if got := m.SecretReaders("MISSING"); got == nil || len(got) != 0 {
		t.Fatalf("unread secret: %#v", got)
	}
	if got := m.SecretReaders("A"); reflect.DeepEqual(got, []string{"none"}) {
		t.Fatal("an app that reads nothing was listed")
	}
}

func TestGenerateRuleLength(t *testing.T) {
	if got := (GenerateRule{}).Length(); got != GenerateDefaultBytes {
		t.Fatalf("default length %d, want %d", got, GenerateDefaultBytes)
	}
	n := 48
	if got := (GenerateRule{Bytes: &n}).Length(); got != 48 {
		t.Fatalf("explicit length %d", got)
	}
}
