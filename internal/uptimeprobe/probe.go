// Package uptimeprobe checks how long PID 1 has been alive in a task.
package uptimeprobe

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"strconv"
	"time"
)

// Run executes the mounted uptime probe and returns its exit status.
func Run(args []string, errout io.Writer) int {
	const usage = "usage: berthy probe uptime DURATION"
	if len(args) != 2 || args[0] != "uptime" {
		fmt.Fprintln(errout, usage)
		return 2
	}
	threshold, err := time.ParseDuration(args[1])
	if err != nil || threshold < 0 {
		fmt.Fprintln(errout, usage)
		return 2
	}
	age, err := processAge()
	if err != nil {
		fmt.Fprintln(errout, "probe uptime:", err)
		return 1
	}
	if age < threshold {
		return 1
	}
	return 0
}

func ageFromStat(stat []byte, boot time.Duration, ticksPerSecond uint64) (time.Duration, error) {
	end := bytes.LastIndex(stat, []byte(") "))
	if !bytes.HasPrefix(stat, []byte("1 (")) || end < len("1 (") {
		return 0, errors.New("invalid PID 1 stat record")
	}
	fields := bytes.Fields(stat[end+2:])
	// The suffix starts at kernel field 3 (state) and ends at field 52 (exit_code).
	if len(fields) < 50 || len(fields[0]) != 1 || stat[len(stat)-1] != '\n' {
		return 0, errors.New("invalid PID 1 stat fields")
	}
	switch fields[0][0] {
	case 'R', 'S', 'D', 'T', 't', 'K', 'W', 'P', 'I':
	case 'Z', 'X', 'x':
		return 0, errors.New("PID 1 is dead")
	default:
		return 0, errors.New("invalid PID 1 state")
	}
	start, err := strconv.ParseUint(string(fields[19]), 10, 64)
	if err != nil || start == math.MaxUint64 || ticksPerSecond == 0 {
		return 0, errors.New("invalid PID 1 start ticks or clock frequency")
	}
	lower, ok := tickTime(start, ticksPerSecond)
	if !ok || lower > boot {
		return 0, errors.New("invalid PID 1 start time")
	}
	// The kernel reports a whole tick. Use the next tick to bound the latest possible start.
	upper, ok := tickTime(start+1, ticksPerSecond)
	if !ok {
		return 0, errors.New("invalid PID 1 start time")
	}
	return max(0, boot-upper), nil
}

// tickTime rounds a tick boundary up to a whole nanosecond.
func tickTime(tick, rate uint64) (time.Duration, bool) {
	sec, fraction := tick/rate, tick%rate
	if sec > math.MaxInt64/uint64(time.Second) {
		return 0, false
	}
	hi, lo := bits.Mul64(fraction, uint64(time.Second))
	nsec, remainder := bits.Div64(hi, lo, rate)
	if remainder != 0 {
		nsec++
	}
	total := sec*uint64(time.Second) + nsec
	return time.Duration(total), total <= math.MaxInt64
}
