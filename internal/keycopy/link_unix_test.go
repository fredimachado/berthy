//go:build unix

package keycopy

import "os"

func plantLink(link, target string) error { return os.Symlink(target, link) }
