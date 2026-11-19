package manifest

import "testing"

// deploy.verify turns off the checks at the end of a deploy. It is on unless the manifest says false.
func TestDeployVerifyDefaultsToOn(t *testing.T) {
	for _, tc := range []struct {
		deploy string
		want   bool
	}{{"", true}, {`{"verify":true}`, true}, {`{"verify":false}`, false}} {
		m, err := DecodeJSON([]byte(attemptManifest(tc.deploy, "", "")))
		if err != nil {
			t.Fatalf("deploy %q: %v", tc.deploy, err)
		}
		if got := m.Apps["api"].Deploy.Verifies(); got != tc.want {
			t.Errorf("deploy %q: Verifies() = %v, want %v", tc.deploy, got, tc.want)
		}
	}
	if !(Deploy{}).Verifies() {
		t.Error("a stored deploy block without the setting must verify")
	}
}
