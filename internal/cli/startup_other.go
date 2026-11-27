//go:build !linux

package cli

import (
	"errors"

	"berthy/internal/agent"
)

func publishStartupResult(*agent.Host, string, string) error { return errors.ErrUnsupported }
