package manifest

import (
	"strings"
	"testing"
)

// TestBerthyPrefixIsReservedInEveryEnvironmentSlot: a name starting with BERTHY_ belongs to Berthy, in env.plain and in
// env.secret alike. A name that only contains BERTHY_ is an ordinary name.
func TestBerthyPrefixIsReservedInEveryEnvironmentSlot(t *testing.T) {
	const message = "names starting with BERTHY_ are reserved for Berthy"
	const app = `"apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}},"env":%s}}`
	reserved := map[string]string{
		"plain":  `{"project":"demo","network":"demo",` + strings.Replace(app, "%s", `{"plain":{"BERTHY_X":"1"}}`, 1) + `}`,
		"secret": `{"project":"demo","network":"demo",` + strings.Replace(app, "%s", `{"secret":["BERTHY_Y"]}`, 1) + `}`,
	}
	for name, input := range reserved {
		if _, err := DecodeJSON([]byte(input)); err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("%s: %v", name, err)
		}
	}
	ordinary := `{"project":"demo","network":"demo",` + strings.Replace(app, "%s", `{"plain":{"MY_BERTHY_X":"1"}}`, 1) + `}`
	if _, err := DecodeJSON([]byte(ordinary)); err != nil {
		t.Errorf("ordinary name refused: %v", err)
	}
}
