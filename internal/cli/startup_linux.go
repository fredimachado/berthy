package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"berthy/internal/agent"
	"golang.org/x/sys/unix"
)

func publishStartupResult(h *agent.Host, path, invocation string) error {
	dir, name := filepath.Split(path)
	parent, err := unix.Open(filepath.Clean(dir), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("opening startup directory: %w", err)
	}
	defer unix.Close(parent)
	var info unix.Stat_t
	if err = unix.Fstat(parent, &info); err != nil {
		return err
	}
	gid := uint32(os.Getegid())
	if info.Mode != unix.S_IFDIR|0750 || info.Uid != 0 || info.Gid != gid {
		return errors.New("startup directory has unexpected ownership or permissions")
	}
	fd, err := unix.Openat(parent, name, unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("opening prepared startup result: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if err = unix.Fstat(fd, &info); err == nil {
		if info.Mode != unix.S_IFREG|0620 || info.Uid != 0 || info.Gid != gid || info.Nlink != 1 || info.Size != 0 {
			err = errors.New("startup result is not a fresh file with the expected ownership and permissions")
		} else {
			err = h.WriteStartupResult(file, invocation)
		}
	}
	return errors.Join(err, file.Close())
}
