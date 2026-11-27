package agent

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

var errUptimeExecutableChanged = errors.New("installed executable differs from the running executable")

func uptimeProbeSource() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	running, err := os.Stat("/proc/self/exe")
	if err != nil {
		return "", err
	}
	if err := verifyUptimeProbeSource(path, running); err != nil {
		return "", err
	}
	return path, nil
}

func verifyUptimeProbeSource(path string, running os.FileInfo) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("invalid executable path")
	}
	// Check top down so untrusted users cannot swap an unchecked ancestor. A trusted sticky directory protects each
	// checked child from replacement by other directory users.
	var parents []string
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		parents = append(parents, dir)
		if dir == "/" {
			break
		}
	}
	uid := uint32(os.Geteuid())
	for i := len(parents) - 1; i >= 0; i-- {
		info, err := os.Lstat(parents[i])
		if err != nil || !info.IsDir() || !trustedProbeOwner(info, uid) ||
			(info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0) {
			return errors.New("executable directory is not protected")
		}
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, running) {
		return errUptimeExecutableChanged
	}
	mode := info.Mode()
	if !mode.IsRegular() || !trustedProbeOwner(info, uid) || mode.Perm()&0111 != 0111 || mode.Perm()&0022 != 0 ||
		mode&(os.ModeSetuid|os.ModeSetgid) != 0 || !staticELF(f) {
		return errors.New("installed executable is not protected, world-executable and static")
	}
	return nil
}

func trustedProbeOwner(info os.FileInfo, uid uint32) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && (stat.Uid == 0 || stat.Uid == uid)
}
