package manifest

import (
	"testing"
	"time"
)

func TestParseCommandTimeout(t *testing.T) {
	for in, want := range map[string]time.Duration{"": time.Hour, "30s": 30 * time.Second, "1s": time.Second, "24h": 24 * time.Hour, "90m": 90 * time.Minute} {
		got, err := ParseCommandTimeout(in)
		if err != nil || got != want {
			t.Errorf("ParseCommandTimeout(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"0s", "500ms", "25h", "-1h", "x", "1"} {
		if _, err := ParseCommandTimeout(in); err == nil {
			t.Errorf("ParseCommandTimeout(%q) accepted", in)
		}
	}
}
