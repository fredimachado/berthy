//go:build windows

package cli

import (
	"os"

	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/windows"
)

func setHiddenMode(fd uintptr) error {
	state, err := term.GetState(fd)
	if err != nil {
		return err
	}
	state.Mode &^= windows.ENABLE_ECHO_INPUT | windows.ENABLE_LINE_INPUT
	state.Mode |= windows.ENABLE_PROCESSED_INPUT
	return term.SetState(fd, state)
}

func readHiddenLine(fd uintptr) ([]byte, error) {
	process := windows.CurrentProcess()
	var handle windows.Handle
	if err := windows.DuplicateHandle(process, windows.Handle(fd), process, &handle, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), "hidden input")
	defer file.Close()
	return readPasswordLine(file.Read, '\r')
}
