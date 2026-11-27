//go:build unix

package cli

import (
	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"
)

func setHiddenMode(fd uintptr) error {
	state, err := term.GetState(fd)
	if err != nil {
		return err
	}
	state.Lflag &^= unix.ECHO
	state.Lflag |= unix.ICANON | unix.ISIG
	state.Iflag |= unix.ICRNL
	return term.SetState(fd, state)
}

func readHiddenLine(fd uintptr) ([]byte, error) {
	return readPasswordLine(func(p []byte) (int, error) {
		for {
			n, err := unix.Read(int(fd), p)
			if err != unix.EINTR {
				return n, err
			}
		}
	}, '\n')
}
