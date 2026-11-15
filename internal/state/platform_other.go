//go:build !linux && !windows

package state

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

func checkDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("state directory must be a directory")
	}
	return nil
}
func readKey(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("key must be a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil {
		return nil, err
	}
	if len(b) != 32 {
		return nil, errors.New("key must contain exactly 32 bytes")
	}
	return b, nil
}
func lockOwner(path string) (*os.File, error) {
	return nil, errors.New("state ownership is supported only on Linux")
}

func prepareDatabase(dir string) (bool, error) {
	p := filepath.Join(dir, "state.db")
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		f.Close()
		return true, nil
	}
	if !os.IsExist(err) {
		return false, err
	}
	info, err := os.Lstat(p)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("unsafe state file")
	}
	return false, nil
}
