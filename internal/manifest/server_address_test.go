package manifest

import (
	"errors"
	"fmt"
	"testing"
)

func TestServerAddress(t *testing.T) {
	base := func(address string) string {
		return fmt.Sprintf(`{
 "project":"demo","network":"demo",
 "servers":{"pg":{"ssh":"ops@10.0.0.5","address":%q}},
 "apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}}
}`, address)
	}
	for _, ok := range []string{"10.0.0.5", "fd00::5"} {
		if _, err := DecodeJSON([]byte(base(ok))); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	want := `servers.pg.address: use one IP address of the server, such as 10.0.0.5`
	for _, bad := range []string{"10.0.0", "0.0.0.0", "::", "db.example.com", "fe80::1%eth0"} {
		_, err := DecodeJSON([]byte(base(bad)))
		var ps Problems
		if err == nil || !errors.As(err, &ps) {
			t.Errorf("%q: got %v", bad, err)
			continue
		}
		if len(ps) != 1 || ps[0].Path != "servers.pg.address" || ps[0].Message != want {
			t.Errorf("%q: got %+v", bad, ps)
		}
	}
}
