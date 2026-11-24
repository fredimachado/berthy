//go:build unix && !darwin

package keycopy

import "os"

func hasACL(*os.File) (bool, error) { return false, nil }
