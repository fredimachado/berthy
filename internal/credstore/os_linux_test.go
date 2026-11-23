//go:build linux

package credstore

import (
	"errors"
	"testing"
)

func TestOSStoreWithoutASessionBusIsUnavailable(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("PATH", "")
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
