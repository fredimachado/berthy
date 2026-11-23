package manifest

import (
	"errors"
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

func problemMessages(t *testing.T, err error) []string {
	t.Helper()
	var ps Problems
	if !errors.As(err, &ps) {
		t.Fatalf("want Problems, got %#v", err)
	}
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Message
	}
	return out
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
		want []string // nil: valid; otherwise every diagnostic, in order
	}{
		{"valid", valid, nil},
		{"invalid name", declaredYAML("secret: [GEN]", "secrets:\n  \"1BAD\":\n    generate:\n      format: hex\n"), []string{"secrets.1BAD: invalid secret name"}},
		{"reserved name", declaredYAML("", "secrets:\n  TUNNEL_TOKEN:\n    generate:\n      format: hex\n"), []string{"secrets.TUNNEL_TOKEN: TUNNEL_TOKEN is reserved for managed ingress"}},
		{"no generate", declaredYAML("secret: [GEN]", "secrets:\n  GEN: {}\n"), []string{"secrets.GEN: generate is required"}},
		{"bad format", declaredYAML("secret: [GEN]", "secrets:\n  GEN:\n    generate:\n      format: uuid\n"), []string{"secrets.GEN.generate.format: must be hex, base64 or alnum"}},
		{"bytes too small", declaredYAML("secret: [GEN]", "secrets:\n  GEN:\n    generate:\n      format: hex\n      bytes: 15\n"), []string{"secrets.GEN.generate.bytes: must be from 16 to 256"}},
		{"bytes too large", declaredYAML("secret: [GEN]", "secrets:\n  GEN:\n    generate:\n      format: hex\n      bytes: 257\n"), []string{"secrets.GEN.generate.bytes: must be from 16 to 256"}},
		// Format is checked before length: one diagnostic, the format one.
		{"bad format and bytes", declaredYAML("secret: [GEN]", "secrets:\n  GEN:\n    generate:\n      format: uuid\n      bytes: 15\n"), []string{"secrets.GEN.generate.format: must be hex, base64 or alnum"}},
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
`, []string{"secrets.REG_PASS.generate: REG_PASS is needed by registry ghcr.io and cannot be generated"}},
		// Non-app use beats "no app reads": registry-only, no env.secret reader.
		{"registry only no reader", `project: demo
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
secrets:
  REG_PASS:
    generate:
      format: hex
`, []string{"secrets.REG_PASS.generate: REG_PASS is needed by registry ghcr.io and cannot be generated"}},
		{"no reader", declaredYAML("secret: [OTHER]", `secrets:
  GEN:
    generate:
      format: hex
`), []string{"secrets.GEN: no app reads GEN"}},
		// One diagnostic per invalid declaration, in name order.
		{"two invalid declarations", declaredYAML("secret: [OTHER]", `secrets:
  A_BAD:
    generate:
      format: uuid
  Z_MISS:
    generate:
      format: hex
`), []string{
			"secrets.A_BAD.generate.format: must be hex, base64 or alnum",
			"secrets.Z_MISS: no app reads Z_MISS",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeFiles(t, map[string]string{"berthy.yml": tc.body})
			m, err := Load(dir, "dev")
			if tc.want == nil {
				if err != nil {
					t.Fatalf("valid manifest refused: %v", err)
				}
				if m.Secrets["GEN"].Generate == nil || m.Secrets["GEN"].Generate.Format != "hex" {
					t.Fatalf("secrets not loaded: %+v", m.Secrets)
				}
				return
			}
			if err == nil {
				t.Fatal("accepted a bad manifest")
			}
			if got := problemMessages(t, err); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("problems\n got %v\nwant %v", got, tc.want)
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
