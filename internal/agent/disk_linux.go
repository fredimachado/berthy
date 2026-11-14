//go:build linux

package agent

import "berthy/internal/metrics"

func hostDisk() (any, error) { return metrics.ReadMounts() }
