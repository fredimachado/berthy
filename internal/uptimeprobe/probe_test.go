package uptimeprobe

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func procStat(pid int, state, start string) []byte {
	return fmt.Appendf(nil, "%d (work) x\n er)) %s%s %s%s\n", pid, state, strings.Repeat(" 0", 18), start, strings.Repeat(" 0", 30))
}

func TestProbeUptimeUsesPIDOneAndKernelTicks(t *testing.T) {
	const boot = 72 * time.Hour
	const threshold = 20 * time.Second
	for _, tc := range []struct {
		rate uint64
		age  time.Duration
		want time.Duration
	}{
		{100, threshold + 9*time.Millisecond, threshold - time.Millisecond},
		{100, threshold + 10*time.Millisecond, threshold},
		{250, threshold + 5*time.Millisecond, threshold + time.Millisecond},
		{3, threshold + time.Second/3, threshold - time.Nanosecond},
		{100, 0, 0},
	} {
		t.Run(fmt.Sprintf("%dHz/%s", tc.rate, tc.age), func(t *testing.T) {
			start := uint64(boot/time.Second-20) * tc.rate
			age, err := ageFromStat(procStat(1, "S", fmt.Sprint(start)), boot-threshold+tc.age, tc.rate)
			if err != nil || age != tc.want {
				t.Fatalf("age = %s, %v, want %s", age, err, tc.want)
			}
		})
	}
}

func TestProbeUptimeRejectsInvalidMeasurements(t *testing.T) {
	for _, tc := range []struct {
		name string
		stat []byte
		boot time.Duration
		rate uint64
	}{
		{"wrong PID", procStat(2, "S", "5"), time.Second, 100},
		{"missing delimiter", []byte("1 (worker"), time.Second, 100},
		{"truncated", []byte("1 (worker) S" + strings.Repeat(" 0", 19)), time.Second, 100},
		{"missing newline", []byte(strings.TrimSuffix(string(procStat(1, "S", "5")), "\n")), time.Second, 100},
		{"bad state", procStat(1, "SS", "5"), time.Second, 100},
		{"unknown state", procStat(1, "0", "5"), time.Second, 100},
		{"zombie", procStat(1, "Z", "5"), time.Second, 100},
		{"dead", procStat(1, "X", "5"), time.Second, 100},
		{"negative ticks", procStat(1, "S", "-5"), time.Second, 100},
		{"missing rate", procStat(1, "S", "5"), time.Second, 0},
		{"future start", procStat(1, "S", "101"), time.Second, 100},
		{"tick overflow", procStat(1, "S", fmt.Sprint(uint64(math.MaxUint64))), time.Second, 100},
		{"duration overflow", procStat(1, "S", "92233720368"), math.MaxInt64, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if age, err := ageFromStat(tc.stat, tc.boot, tc.rate); err == nil {
				t.Fatalf("accepted measurement: age = %s", age)
			}
		})
	}
}
