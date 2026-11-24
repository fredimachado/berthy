//go:build darwin

package keycopy

import (
	"golang.org/x/sys/unix"
	"os"
	"unsafe"
)

func hasACL(f *os.File) (bool, error) {
	attrs := unix.Attrlist{Bitmapcount: 5, Commonattr: unix.ATTR_CMN_EXTENDED_SECURITY}
	buf := make([]byte, 65536)
	_, _, errno := unix.Syscall6(unix.SYS_FGETATTRLIST, f.Fd(), uintptr(unsafe.Pointer(&attrs)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0)
	if errno != 0 {
		return false, errno
	}
	return parseACLAttributes(buf)
}
