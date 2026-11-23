//go:build linux

package credstore

import (
	"os"
	"path/filepath"
)

// sessionBus reports whether a D-Bus session bus is already available. Without one, go-keyring's Linux backend runs
// dbus-launch (a PATH search and a new daemon), which we refuse.
func sessionBus() bool {
	if addr := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); addr != "" && addr != "autolaunch:" {
		return true
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "bus"))
	return err == nil
}
