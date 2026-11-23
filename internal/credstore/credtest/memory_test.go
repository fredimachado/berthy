package credtest

import (
	"errors"
	"testing"

	"berthy/internal/credstore"
)

func TestMemoryRoundTrip(t *testing.T) {
	m := NewMemory()
	if m.Name() != "test store" {
		t.Fatalf("Name = %q", m.Name())
	}
	key := credstore.Key{
		URL:         "http://10.0.0.5:7710",
		Project:     "todo",
		Environment: "dev",
		Variable:    "BERTHY_TODO_DEV_TOKEN",
	}
	if _, err := m.Get(key); !errors.Is(err, credstore.ErrNotFound) {
		t.Fatalf("Get empty: %v", err)
	}
	if err := m.Set(key, "secret"); err != nil {
		t.Fatal(err)
	}
	if m.Len() != 1 {
		t.Fatalf("Len = %d", m.Len())
	}
	got, err := m.Get(key)
	if err != nil || got != "secret" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	removed, err := m.Delete(key)
	if err != nil || !removed {
		t.Fatalf("Delete = %v, %v", removed, err)
	}
	removed, err = m.Delete(key)
	if err != nil || removed {
		t.Fatalf("second Delete = %v, %v", removed, err)
	}
	if m.Calls() < 5 {
		t.Fatalf("Calls = %d, want at least 5", m.Calls())
	}
}
