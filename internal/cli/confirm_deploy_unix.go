//go:build unix

package cli

import (
	"context"
	"io"
	"os"

	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"
)

func readDeployConfirmation(ctx context.Context, in *os.File, out io.Writer) (string, error) {
	state, err := term.MakeRaw(in.Fd())
	if err != nil {
		return "", err
	}
	defer term.Restore(in.Fd(), state)
	var p [2]int
	if err := unix.Pipe(p[:]); err != nil {
		return "", err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(done); _, _ = unix.Write(p[1], []byte{1}) })
	defer func() {
		if !stop() {
			<-done
		}
		unix.Close(p[0])
		unix.Close(p[1])
	}()
	fds := []unix.PollFd{{Fd: int32(in.Fd()), Events: unix.POLLIN}, {Fd: int32(p[0]), Events: unix.POLLIN}}
	var line confirmationLine
	for {
		_, err := unix.Poll(fds, -1)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return "", err
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if fds[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) == 0 {
			continue
		}
		var b [1]byte
		n, err := unix.Read(int(in.Fd()), b[:])
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return "", err
		}
		if n == 0 {
			return "", io.EOF
		}
		end, err := line.add(b[0], out)
		if err != nil {
			return "", err
		}
		if end {
			return line.answer(), nil
		}
	}
}
