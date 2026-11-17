package api

import (
	"strconv"
	"strings"
	"testing"
)

// assertNoValue fails when err's text contains the value of an entry. Values are long enough here that a match means a leak.
func assertNoValue(t *testing.T, err error, entries []SecretEntry) {
	t.Helper()
	if err == nil {
		return
	}
	for _, e := range entries {
		if len(e.Value) >= 8 && strings.Contains(err.Error(), e.Value) {
			t.Fatalf("error %q contains the value of %s", err, e.Name)
		}
	}
}

func TestCheckSecretName(t *testing.T) {
	for _, name := range []string{"A", "_", "a1_B", "DATABASE_URL"} {
		if err := CheckSecretName(name); err != nil {
			t.Errorf("%q refused: %v", name, err)
		}
	}
	for _, name := range []string{"", "1A", "A-B", "A B", "A=B", "TUNNEL_TOKEN", "é"} {
		if err := CheckSecretName(name); err == nil {
			t.Errorf("%q accepted", name)
		}
	}
	if err := CheckSecretName("TUNNEL_TOKEN"); err == nil || !strings.Contains(err.Error(), "TUNNEL_TOKEN is reserved for managed ingress") {
		t.Errorf("TUNNEL_TOKEN: %v", err)
	}
}

func TestCheckSecretImport(t *testing.T) {
	const canary = "import-canary-value"
	big := strings.Repeat("v", MaxSecretValue)
	many := make([]SecretEntry, MaxSecretImport+1)
	for i := range many {
		many[i] = SecretEntry{Name: "S" + strconv.Itoa(i), Value: canary}
	}
	for _, tc := range []struct {
		name    string
		entries []SecretEntry
		ok      bool
		want    string // text the error must contain
	}{
		{"good", []SecretEntry{{"DATABASE_URL", canary}, {"_A1", canary}}, true, ""},
		{"largest value", []SecretEntry{{"BIG", big}}, true, ""},
		{"most entries", many[:MaxSecretImport], true, ""},
		{"empty", nil, false, "no secrets"},
		{"501 entries", many, false, "at most 500"},
		{"bad name names the entry", []SecretEntry{{"GOOD", canary}, {"1BAD", canary}}, false, "entry 2: the name is not a valid secret name"},
		{"dash in name", []SecretEntry{{"A-B", canary}}, false, "entry 1"},
		{"TUNNEL_TOKEN", []SecretEntry{{"TUNNEL_TOKEN", canary}}, false, "TUNNEL_TOKEN is reserved for managed ingress"},
		{"duplicate", []SecretEntry{{"A", canary}, {"B", canary}, {"A", "other-canary-value"}}, false, "A appears more than once"},
		{"value of 64 KiB plus one", []SecretEntry{{"TOO_BIG", big + "v"}}, false, "TOO_BIG: the value is larger than 65536 bytes"},
		{"NUL in value", []SecretEntry{{"NUL_VALUE", "a\x00b-canary"}}, false, "NUL_VALUE: the value contains a NUL byte"},
	} {
		err := CheckSecretImport(tc.entries)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err %v, want ok %v", tc.name, err, tc.ok)
			continue
		}
		if err != nil && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %q, want it to contain %q", tc.name, err, tc.want)
		}
		assertNoValue(t, err, tc.entries)
	}
}
