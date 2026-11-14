//go:build !linux

package agent

import "fmt"

func hostDisk() (any, error) { return nil, fmt.Errorf("host disk is only available on Linux") }
