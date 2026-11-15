//go:build linux

package state

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func checkDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("state directory must be a private 0700 directory")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) {
		return errors.New("state directory owner mismatch")
	}
	return nil
}
func readKey(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || st.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("key must be an owned regular 0600 file")
	}
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
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("state owner: %w", err)
	}
	return f, nil
}

func prepareDatabase(dir string) (bool, error) {
	for _, name := range []string{"state.db-wal", "state.db-shm", "state.db-journal"} {
		if err := checkPrivateFile(filepath.Join(dir, name), true); err != nil {
			return false, err
		}
	}
	path := filepath.Join(dir, "state.db")
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err == nil {
		unix.Close(fd)
		return true, nil
	}
	if !errors.Is(err, unix.EEXIST) {
		return false, err
	}
	if err = checkPrivateFile(path, false); err != nil {
		return false, err
	}
	return false, nil
}
func checkPrivateFile(path string, optional bool) error {
	info, err := os.Lstat(path)
	if optional && os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 {
		return errors.New("state file must be a private owned regular file with one link")
	}
	return nil
}
