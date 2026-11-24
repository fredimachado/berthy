//go:build !unix && !windows

package keycopy

import (
	"errors"
	"os"
)

func makeDir(*os.File, string) error                                 { return errors.ErrUnsupported }
func createFile(*os.File, string) (*os.File, error)                  { return nil, errors.ErrUnsupported }
func openDir(*os.File, string) (*os.File, error)                     { return nil, errors.ErrUnsupported }
func openFile(*os.File, string) (*os.File, error)                    { return nil, errors.ErrUnsupported }
func checkPrivate(*os.File, bool) error                              { return errors.ErrUnsupported }
func linkFile(*os.File, *os.File, string, string, os.FileInfo) error { return errors.ErrUnsupported }
func createStage(*os.File, string) (*staging, *os.File, error) {
	return nil, nil, errors.ErrUnsupported
}
func finishStageWrite(*os.File) error         { return errors.ErrUnsupported }
func closeStageWrite(*os.File) error          { return errors.ErrUnsupported }
func closeStage(*staging)                     {}
func removeStage(*staging, os.FileInfo)       {}
func removeOwn(*os.File, string, os.FileInfo) {}
func syncDirectory(*os.File) error            { return errors.ErrUnsupported }
