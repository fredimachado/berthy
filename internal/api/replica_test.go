package api

import "testing"

func TestFinishedOneShot(t *testing.T) {
	zero, three := 0, 3
	tests := []struct {
		name string
		r    Replica
		want bool
	}{
		{"exited with 0", Replica{State: "exited", ExitCode: &zero}, true},
		{"exited with 3", Replica{State: "exited", ExitCode: &three}, false},
		{"exited without a code", Replica{State: "exited"}, false},
		{"running", Replica{State: "running"}, false},
		{"dead with 0", Replica{State: "dead", ExitCode: &zero}, false},
	}
	for _, tt := range tests {
		if got := tt.r.FinishedOneShot(); got != tt.want {
			t.Errorf("%s: FinishedOneShot() = %v, want %v", tt.name, got, tt.want)
		}
	}
}
