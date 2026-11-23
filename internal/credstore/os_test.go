package credstore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
)

func TestOSStoreRoundTrip(t *testing.T) {
	if os.Getenv("BERTHY_TEST_OS_STORE") != "1" {
		t.Skip("set BERTHY_TEST_OS_STORE=1 to exercise the OS credential store")
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	service := "berthy-test-" + hex.EncodeToString(b[:])
	s := OS(service)
	key := Key{
		URL:         "http://127.0.0.1:7710",
		Project:     "todo",
		Environment: "dev",
		Variable:    "BERTHY_TODO_DEV_TOKEN",
	}
	t.Cleanup(func() {
		_, _ = s.Delete(key)
	})
	const secret = "bty.test.token-value-for-os-store"
	if err := s.Set(key, secret); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(key)
	if err != nil || got != secret {
		t.Fatalf("Get = %q, %v", got, err)
	}
	removed, err := s.Delete(key)
	if err != nil || !removed {
		t.Fatalf("Delete = %v, %v", removed, err)
	}
	if _, err := s.Get(key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete: %v", err)
	}
	removed, err = s.Delete(key)
	if err != nil || removed {
		t.Fatalf("second Delete = %v, %v", removed, err)
	}
}
