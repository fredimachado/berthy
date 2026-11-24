package state

import (
	"errors"
	"io/fs"
	"os"
)

// ErrRotationUnsettled means the next key may not yet be in place.
var ErrRotationUnsettled = errors.New("a key rotation is not finished")

// ReadSettledKey reads the configured key only when no unfinished rotation is visible.
func ReadSettledKey(path string) ([]byte, error) {
	raw, err := readKey(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path + KeyNextSuffix); !errors.Is(err, fs.ErrNotExist) {
		clear(raw)
		return nil, ErrRotationUnsettled
	}
	return raw, nil
}
