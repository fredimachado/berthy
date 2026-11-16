package api

import (
	"testing"
	"time"
)

func TestHumanSizeAndAge(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 3074018: "2.9 MiB", 5 << 30: "5.0 GiB"} {
		if got := HumanSize(n); got != want {
			t.Errorf("size %d: %q, want %q", n, got, want)
		}
	}
	for d, want := range map[time.Duration]string{-time.Second: "0s", 0: "0s", 42 * time.Second: "42s", 5 * time.Minute: "5m", 59*time.Minute + 59*time.Second: "59m", 3 * time.Hour: "3h", 47 * time.Hour: "47h", 48 * time.Hour: "2d", 10 * 24 * time.Hour: "10d"} {
		if got := HumanAge(d); got != want {
			t.Errorf("age %v: %q, want %q", d, got, want)
		}
	}
}
