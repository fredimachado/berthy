package engine

import (
	"berthy/internal/api"
	"testing"
)

func TestSelect(t *testing.T) {
	a := []api.App{{Ref: api.AppRef{Kind: "swarm", Name: "web"}}, {Ref: api.AppRef{Kind: "compose", Name: "web"}}}
	for _, c := range []struct {
		s    string
		code int
	}{{"web", 409}, {"swarm:web", 200}, {"missing", 404}} {
		_, got := Select(a, c.s)
		if got != c.code {
			t.Errorf("%s: %d", c.s, got)
		}
	}
}
