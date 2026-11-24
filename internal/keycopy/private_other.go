//go:build !unix && !windows

package keycopy

import (
	"errors"
	"io/fs"
	"os"
)

func makeDir(*os.File, string) error                       { return errors.ErrUnsupported }
func createFile(*os.File, string) (*os.File, error)        { return nil, errors.ErrUnsupported }
func openDir(*os.File, string) (*os.File, error)           { return nil, errors.ErrUnsupported }
func openFile(*os.File, string) (*os.File, error)          { return nil, errors.ErrUnsupported }
func checkPrivate(*os.File, bool) error                    { return errors.ErrUnsupported }
func isLink(fi fs.FileInfo) bool                           { return fi.Mode()&os.ModeSymlink != 0 }
func linkFile(*os.File, string, string, os.FileInfo) error { return errors.ErrUnsupported }
func removeOwn(*os.File, string, os.FileInfo)              {}
func syncDirectory(*os.File) error                         { return errors.ErrUnsupported }
