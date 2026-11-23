package manifest

import (
	"slices"
	"strings"
)

// DeclaredSecret is what the manifest says about one secret, by its name. Today it says one thing: Berthy may generate it.
type DeclaredSecret struct {
	Generate *GenerateRule `json:"generate,omitempty" yaml:"generate,omitempty"`
}

// GenerateRule is how a declared secret is generated.
type GenerateRule struct {
	Format string `json:"format" yaml:"format"`
	Bytes  *int   `json:"bytes,omitempty" yaml:"bytes,omitempty"`
}

// The length and the formats of a generated secret. Bytes is the number of random bytes for hex and base64, and the number of
// characters for alnum.
const (
	GenerateDefaultBytes  = 32
	GenerateMinBytes      = 16
	GenerateMaxBytes      = 256
	GenerateDefaultFormat = "hex"
)

// GenerateFormats are the formats a generated secret may take: hex and URL-safe base64 without padding, which are safe inside a
// URL, and letters and digits.
var GenerateFormats = []string{"hex", "base64", "alnum"}

// Length is the rule's bytes, or GenerateDefaultBytes.
func (g GenerateRule) Length() int {
	if g.Bytes != nil {
		return *g.Bytes
	}
	return GenerateDefaultBytes
}

// SecretReaders returns the apps that read the secret name through env.secret or env.template, sorted.
func (m *Manifest) SecretReaders(name string) []string {
	out := []string{}
	for _, app := range sortedKeys(m.Apps) {
		if slices.Contains(m.Apps[app].Env.SecretNames(), name) {
			out = append(out, app)
		}
	}
	return out
}

// checkSecrets checks each declared secret: its name, that it may be generated, and that an app reads it.
func (m *Manifest) checkSecrets(p *Problems) {
	if len(m.Secrets) == 0 {
		return
	}
	// One SecretNeeds pass for every declared name: NeededBy empty means no reader; a non-app entry refuses generation.
	neededBy := map[string][]string{}
	for _, need := range m.SecretNeeds() {
		neededBy[need.Name] = need.NeededBy
	}
	for _, name := range sortedKeys(m.Secrets) {
		path := join("secrets", name)
		s := m.Secrets[name]
		switch {
		case !envName.MatchString(name):
			p.add(path, "secrets.%s: invalid secret name", name)
			continue
		case name == TunnelTokenSecret:
			p.add(path, "secrets.%s: %s is reserved for managed ingress", name, TunnelTokenSecret)
			continue
		case s.Generate == nil:
			p.add(path, "secrets.%s: generate is required", name)
			continue
		}
		gpath := join(path, "generate")
		g := s.Generate
		if !slices.Contains(GenerateFormats, g.Format) {
			p.add(join(gpath, "format"), "secrets.%s.generate.format: must be hex, base64 or alnum", name)
			continue
		}
		if g.Bytes != nil && (*g.Bytes < GenerateMinBytes || *g.Bytes > GenerateMaxBytes) {
			p.add(join(gpath, "bytes"), "secrets.%s.generate.bytes: must be from %d to %d", name, GenerateMinBytes, GenerateMaxBytes)
			continue
		}
		who := neededBy[name]
		hasApp, refused := false, false
		for _, w := range who {
			if strings.HasPrefix(w, "app ") {
				hasApp = true
				continue
			}
			p.add(gpath, "secrets.%s.generate: %s is needed by %s and cannot be generated", name, name, w)
			refused = true
			break
		}
		if !refused && !hasApp {
			p.add(path, "secrets.%s: no app reads %s", name, name)
		}
	}
}
