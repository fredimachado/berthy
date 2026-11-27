//go:build !linux

package uptimeprobe

import (
	"errors"
	"time"
)

func processAge() (time.Duration, error) {
	return 0, errors.New("PID 1 uptime is only available on Linux")
}
