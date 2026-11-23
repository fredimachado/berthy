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
}
