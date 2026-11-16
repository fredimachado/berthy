//go:build unix

package volarchive

import (
	"io/fs"
	"os"
	"syscall"
)

// inodeKey names an inode on this host.
type inodeKey struct{ dev, ino uint64 }

// linkKey returns the inode of a file and whether it has more than one name.
func linkKey(fi fs.FileInfo) (inodeKey, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return inodeKey{}, false
	}
	return inodeKey{dev: uint64(st.Dev), ino: uint64(st.Ino)}, uint64(st.Nlink) > 1
}

// chownLink changes the owner of name, or of the link itself when name is one.
func chownLink(root *os.Root, name string, uid, gid int) error { return root.Lchown(name, uid, gid) }
