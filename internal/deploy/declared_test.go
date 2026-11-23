package deploy

import (
	"berthy/internal/api"
	"berthy/internal/manifest"
	"testing"
)

func TestDecideDeclared(t *testing.T) {
	m := manifest.Manifest{Secrets: map[string]manifest.DeclaredSecret{
		"A": {Generate: &manifest.GenerateRule{Format: "hex"}},
		"B": {Generate: &manifest.GenerateRule{Format: "base64"}},
		"C": {Generate: &manifest.GenerateRule{Format: "alnum"}},
	}}
	got := decideDeclared(m, map[string]bool{"A": true}, map[string]GuardFinding{"B": {App: "db", Volume: "data"}})
	if len(got) != 3 || got[0].Name != "A" || got[0].Outcome != api.DeclaredKeep || got[1].Outcome != api.DeclaredRefuse || got[1].Volume != "data" || got[2].Outcome != api.DeclaredGenerate {
		t.Fatalf("outcomes: %+v", got)
	}
}
