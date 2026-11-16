//go:build !unix

package volarchive

import (
	"io/fs"
	"os"
)

// inodeKey names an inode; without Unix there is none, and no file counts as having several names.
type inodeKey struct{}

func linkKey(fs.FileInfo) (inodeKey, bool) { return inodeKey{}, false }

// chownLink keeps no owner: there is none to keep without Unix.
func chownLink(*os.Root, string, int, int) error { return nil }
