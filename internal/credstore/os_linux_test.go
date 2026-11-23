//go:build linux

package credstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOSStoreWithoutASessionBusIsUnavailable(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	t.Setenv("PATH", "")
	xdg := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", xdg)
	// An XDG bus is not what godbus checks; if we looked there we would wrongly think a session bus exists.
	if err := os.WriteFile(filepath.Join(xdg, "bus"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "bus")
	prev := sessionBusPath
	sessionBusPath = missing
	t.Cleanup(func() { sessionBusPath = prev })

	if sessionBus() {
		t.Fatal("sessionBus true when only $XDG_RUNTIME_DIR/bus exists; godbus looks at sessionBusPath")
	}
	if got := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); got != "" {
		t.Fatalf("DBUS_SESSION_BUS_ADDRESS = %q, want unset", got)
	}
	s := OS("berthy-test-nosession")
	key := Key{URL: "http://10.0.0.5:7710", Project: "todo", Environment: "dev", Variable: "BERTHY_TODO_DEV_TOKEN"}
	if err := s.Set(key, "secret"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Set: %v", err)
	}
	if _, err := s.Get(key); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Get: %v", err)
	}
	if removed, err := s.Delete(key); removed || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Delete: removed=%v err=%v", removed, err)
	}
	if got := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); got != "" {
		t.Fatalf("after unavailable calls DBUS_SESSION_BUS_ADDRESS = %q, want unset", got)
	}
}

func TestSessionBusPinsTheBusAddress(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	bus := filepath.Join(t.TempDir(), "bus")
	if err := os.WriteFile(bus, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	prev := sessionBusPath
	sessionBusPath = bus
	t.Cleanup(func() { sessionBusPath = prev })

	if !sessionBus() {
		t.Fatal("sessionBus false when the godbus path exists")
	}
	want := "unix:path=" + bus
	if got := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); got != want {
		t.Fatalf("DBUS_SESSION_BUS_ADDRESS = %q, want %q", got, want)
	}
}
