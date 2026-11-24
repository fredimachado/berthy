//go:build windows

package cli

import (
	"context"
	"io"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var readDeployConsoleInput = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReadConsoleInputW")

type deployInputRecord struct {
	kind uint16
	_    uint16
	data [16]byte
}
type deployKeyEvent struct {
	down    int32
	repeat  uint16
	key     uint16
	scan    uint16
	char    uint16
	control uint32
}

func readDeployConfirmation(ctx context.Context, in *os.File, out io.Writer) (string, error) {
	h := windows.Handle(in.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return "", err
	}
	if err := windows.SetConsoleMode(h, mode&^1); err != nil {
		return "", err
	}
	defer windows.SetConsoleMode(h, mode)
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return "", err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(done); _ = windows.SetEvent(event) })
	defer func() {
		if !stop() {
			<-done
		}
		windows.CloseHandle(event)
	}()
	var line confirmationLine
	for {
		_, err := windows.WaitForMultipleObjects([]windows.Handle{event, h}, false, windows.INFINITE)
		if err != nil {
			return "", err
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var rec deployInputRecord
		var n uint32
		ok, _, callErr := readDeployConsoleInput.Call(uintptr(h), uintptr(unsafe.Pointer(&rec)), 1, uintptr(unsafe.Pointer(&n)))
		if ok == 0 {
			return "", callErr
		}
		if n != 1 || rec.kind != 1 {
			continue
		}
		key := (*deployKeyEvent)(unsafe.Pointer(&rec.data[0]))
		if key.down == 0 {
			continue
		}
		if key.char == 0 {
			if key.key != 16 && key.key != 17 && key.key != 18 && key.key != 20 {
				line.invalid = true
			}
			continue
		}
		if key.char > 127 {
			line.invalid = true
			continue
		}
		for i := uint16(0); i < key.repeat; i++ {
			end, err := line.add(byte(key.char), out)
			if err != nil {
				return "", err
			}
			if end {
				return line.answer(), nil
			}
		}
	}
}
