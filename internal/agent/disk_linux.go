//go:build linux

package agent

import "berthy/internal/metrics"

func hostDisk() ([]metrics.Mount, error) { return metrics.ReadMounts() }
