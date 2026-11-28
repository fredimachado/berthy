//go:build !linux

package state

import (
	"context"
	"errors"

	"berthy/internal/scope"
)

// HasAppliedApps runs on the Linux server, where the SQLite probe has read-only WAL support.
func HasAppliedApps(context.Context, string, scope.ID) (bool, error) {
	return false, errors.New("reading hosted state requires Linux")
}
