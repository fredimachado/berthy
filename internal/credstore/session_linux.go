//go:build linux

package credstore

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// sessionBusPath is the unix socket godbus checks before dbus-launch: /run/user/<uid>/bus.
// Tests point it at a path under a temp dir so they never touch the real bus.
var sessionBusPath = defaultSessionBusPath()

func defaultSessionBusPath() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return filepath.Join("/run/user", u.Uid, "bus")
}

// sessionBus reports whether a D-Bus session bus is already available. Without one, go-keyring's Linux backend runs
// dbus-launch (a PATH search and a new daemon), which we refuse. When the address is unset and sessionBusPath
// exists, it sets DBUS_SESSION_BUS_ADDRESS=unix:path=<sessionBusPath> so godbus uses that address and never
// reaches its autolaunch branch if the socket vanishes before the library call.
func sessionBus() bool {
	if addr := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); addr != "" && !strings.HasPrefix(addr, "autolaunch:") {
		return true
	}
	if sessionBusPath == "" {
		return false
	}
	if _, err := os.Stat(sessionBusPath); err != nil {
		return false
	}
	_ = os.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+sessionBusPath)
	return true
}
