//go:build !linux

package agent

import (
	"berthy/internal/metrics"
	"fmt"
)

func hostDisk() ([]metrics.Mount, error) {
	return nil, fmt.Errorf("host disk is only available on Linux")
}
