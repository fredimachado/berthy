//go:build unix

package keycopy

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

var afterStageCheck func()

func makeDir(parent *os.File, name string) error { return unix.Mkdirat(int(parent.Fd()), name, 0700) }
func createFile(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// A fresh private directory keeps other Berthy invocations from sharing a staging name.
func createStage(parent *os.File, name string) (*staging, *os.File, error) {
	if err := makeDir(parent, name); err != nil {
		return nil, nil, err
	}
	dir, err := openDir(parent, name)
	if err != nil {
		return nil, nil, err
	}
	if err = checkPrivate(dir, true); err != nil {
		dir.Close()
		return nil, nil, err
	}
	info, err := dir.Stat()
	if err != nil {
		dir.Close()
		return nil, nil, err
	}
	s := &staging{parent: parent, dir: dir, name: "key", outer: name, outerInfo: info}
	f, err := createFile(dir, s.name)
	if err != nil {
		removeStage(s, nil)
		dir.Close()
		return nil, nil, err
	}
	return s, f, nil
}

func finishStageWrite(*os.File) error  { return nil }
func closeStageWrite(f *os.File) error { return f.Close() }
func closeStage(s *staging)            { s.dir.Close() }

func removeStage(s *staging, info os.FileInfo) {
	if info != nil {
		removeOwn(s.dir, s.name, info)
	}
	current, err := openDir(s.parent, s.outer)
	if err != nil {
		return
	}
	fi, err := current.Stat()
	current.Close()
	if err == nil && os.SameFile(s.outerInfo, fi) {
		_ = unix.Unlinkat(int(s.parent.Fd()), s.outer, unix.AT_REMOVEDIR)
	}
}
func openDir(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}
func openFile(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}
func checkPrivate(f *os.File, dir bool) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("unknown owner")
	}
	if st.Uid != uint32(os.Geteuid()) || (dir && (!info.IsDir() || info.Mode().Perm() != 0700)) || (!dir && (!info.Mode().IsRegular() || info.Mode().Perm() != 0600)) || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return errors.New("not private")
	}
	acl, err := hasACL(f)
	if err != nil {
		return err
	}
	if acl {
		return errors.New("has ACL")
	}
	return nil
}
func linkFile(from, to *os.File, old, name string, info os.FileInfo) error {
	f, err := openFile(from, old)
	if err != nil {
		return err
	}
	current, err := f.Stat()
	f.Close()
	if err != nil {
		return err
	}
	if !os.SameFile(info, current) {
		return errors.New("temporary entry changed")
	}
	if afterStageCheck != nil {
		afterStageCheck()
	}
	return unix.Linkat(int(from.Fd()), old, int(to.Fd()), name, 0)
}
func removeOwn(dir *os.File, name string, info os.FileInfo) {
	f, err := openFile(dir, name)
	if err != nil {
		return
	}
	current, err := f.Stat()
	f.Close()
	if err == nil && os.SameFile(info, current) {
		_ = unix.Unlinkat(int(dir.Fd()), name, 0)
	}
}
func syncDirectory(dir *os.File) error { return dir.Sync() }
