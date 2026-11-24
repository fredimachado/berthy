//go:build windows

package keycopy

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

const fileAllAccess = 0x1f01ff

func currentSID() (*windows.SID, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return u.User.Sid, nil
}
func privateDescriptor(dir bool) (*windows.SECURITY_DESCRIPTOR, error) {
	sid, err := currentSID()
	if err != nil {
		return nil, err
	}
	flags := ""
	if dir {
		flags = "OICI"
	}
	return windows.SecurityDescriptorFromString(fmt.Sprintf("O:%sD:P(A;%s;FA;;;%s)", sid.String(), flags, sid.String()))
}
func ntError(err error) error {
	if err == nil {
		return nil
	}
	var status windows.NTStatus
	if errors.As(err, &status) {
		return status.Errno()
	}
	return err
}
func ntOpen(parent *os.File, name string, access, disposition, options uint32, sd *windows.SECURITY_DESCRIPTOR) (*os.File, error) {
	n, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(parent.Fd()), ObjectName: n, Attributes: windows.OBJ_CASE_INSENSITIVE, SecurityDescriptor: sd}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&h, access|windows.SYNCHRONIZE, &oa, &iosb, nil, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, disposition, options|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		return nil, ntError(err)
	}
	return os.NewFile(uintptr(h), name), nil
}
func makeDir(parent *os.File, name string) error {
	sd, err := privateDescriptor(true)
	if err != nil {
		return err
	}
	f, err := ntOpen(parent, name, windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE, windows.FILE_CREATE, windows.FILE_DIRECTORY_FILE, sd)
	if err != nil {
		return err
	}
	return f.Close()
}
func createFile(parent *os.File, name string) (*os.File, error) {
	sd, err := privateDescriptor(false)
	if err != nil {
		return nil, err
	}
	return ntOpen(parent, name, windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.DELETE, windows.FILE_CREATE, windows.FILE_NON_DIRECTORY_FILE, sd)
}
func createStage(parent *os.File, name string) (*staging, *os.File, error) {
	f, err := createFile(parent, name)
	if err != nil {
		return nil, nil, err
	}
	return &staging{parent: parent, dir: parent, name: name}, f, nil
}
func finishStageWrite(f *os.File) error        { return f.Close() }
func closeStageWrite(*os.File) error           { return nil }
func closeStage(*staging)                      {}
func removeStage(s *staging, info os.FileInfo) { removeOwn(s.dir, s.name, info) }
func openDir(parent *os.File, name string) (*os.File, error) {
	return ntOpen(parent, name, windows.FILE_GENERIC_READ, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, nil)
}
func openFile(parent *os.File, name string) (*os.File, error) {
	return ntOpen(parent, name, windows.FILE_GENERIC_READ, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE, nil)
}
func openDelete(parent *os.File, name string) (*os.File, error) {
	return ntOpen(parent, name, windows.FILE_GENERIC_READ|windows.DELETE, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE, nil)
}
func checkPrivate(f *os.File, dir bool) error {
	h := windows.Handle(f.Fd())
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	a := info.FileAttributes
	if a&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DEVICE) != 0 || ((a&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != dir) {
		return errors.New("wrong file type")
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	sid, err := currentSID()
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(sid) {
		return errors.New("wrong owner")
	}
	control, _, err := sd.Control()
	if err != nil || control&(windows.SE_DACL_PRESENT|windows.SE_DACL_PROTECTED) != windows.SE_DACL_PRESENT|windows.SE_DACL_PROTECTED {
		return errors.New("unprotected DACL")
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil || acl.AceCount != 1 {
		return errors.New("wrong DACL")
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err = windows.GetAce(acl, 0, &ace); err != nil {
		return err
	}
	if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || uint32(ace.Mask)&fileAllAccess != fileAllAccess || !(*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(sid) {
		return errors.New("wrong DACL entry")
	}
	return nil
}

// The legacy FileLinkInformation class makes a no-replace hard link relative to RootDirectory.
type linkInfo struct {
	replace uint32
	root    windows.Handle
	length  uint32
	name    [1]uint16
}

func linkFile(from, to *os.File, old, name string, info os.FileInfo) error {
	f, err := openDelete(from, old)
	if err != nil {
		return err
	}
	defer f.Close()
	current, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, current) {
		return errors.New("temporary entry changed")
	}
	n, err := windows.UTF16FromString(name)
	if err != nil {
		return err
	}
	n = n[:len(n)-1]
	var shape linkInfo
	size := int(unsafe.Offsetof(shape.name)) + len(n)*2
	buf := make([]byte, size)
	p := (*linkInfo)(unsafe.Pointer(&buf[0]))
	p.root = windows.Handle(to.Fd())
	p.length = uint32(len(n) * 2)
	copy(unsafe.Slice(&p.name[0], len(n)), n)
	var iosb windows.IO_STATUS_BLOCK
	return ntError(windows.NtSetInformationFile(windows.Handle(f.Fd()), &iosb, &buf[0], uint32(len(buf)), 11))
}
func removeOwn(dir *os.File, name string, info os.FileInfo) {
	f, err := openDelete(dir, name)
	if err != nil {
		return
	}
	defer f.Close()
	current, err := f.Stat()
	if err != nil || !os.SameFile(info, current) {
		return
	}
	disposition := byte(1)
	var iosb windows.IO_STATUS_BLOCK
	_ = windows.NtSetInformationFile(windows.Handle(f.Fd()), &iosb, &disposition, 1, windows.FileDispositionInformation)
}
func syncDirectory(*os.File) error { return nil }
