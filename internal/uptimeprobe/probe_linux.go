package uptimeprobe

import (
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func processAge() (time.Duration, error) {
	stat, err := os.ReadFile("/proc/1/stat")
	if err != nil {
		return 0, err
	}
	aux, err := unix.Auxv()
	if err != nil {
		return 0, fmt.Errorf("read auxiliary vector: %w", err)
	}
	const atClktck = 17
	var rate uint64
	for _, entry := range aux {
		if entry[0] == atClktck {
			rate = uint64(entry[1])
			break
		}
	}
	var now unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &now); err != nil {
		return 0, fmt.Errorf("read boot clock: %w", err)
	}
	sec, nsec := int64(now.Sec), int64(now.Nsec)
	if nsec < 0 || nsec >= int64(time.Second) || sec < 0 || sec > (math.MaxInt64-nsec)/int64(time.Second) {
		return 0, errors.New("invalid boot clock reading")
	}
	return ageFromStat(stat, time.Duration(sec)*time.Second+time.Duration(nsec), rate)
}
