package agentclient

import "testing"

func TestEnvironmentName(t *testing.T) {
	for _, tc := range []struct {
		name        string
		flag        string
		given       bool
		variable    string
		wantName    string
		wantFromVar bool
	}{
		{"given with a value", "dev", true, "prod", "dev", false},
		{"given and empty with the variable set", "", true, "prod", "", false},
		{"not given with the variable", "", false, "prod", "prod", true},
		{"neither", "", false, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvironmentEnv, tc.variable)
			got, fromVar := EnvironmentName(tc.flag, tc.given)
			if got != tc.wantName || fromVar != tc.wantFromVar {
				t.Fatalf("EnvironmentName(%q, %v) = %q, %v; want %q, %v", tc.flag, tc.given, got, fromVar, tc.wantName, tc.wantFromVar)
			}
		})
	}
}
