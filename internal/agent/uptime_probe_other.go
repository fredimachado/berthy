//go:build !linux

package agent

import "errors"

func uptimeProbeSource() (string, error) {
	return "", errors.New("the uptime probe requires a Linux agent")
}
