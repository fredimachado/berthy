package keycopy_test

import (
	"os"
	"path/filepath"
	"testing"

	"berthy/internal/state"
)

func TestKeyIDOfBytesMatchesFile(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, key, 0600); err != nil {
		t.Fatal(err)
	}
	want, err := state.KeyIDOfFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := state.KeyIDOfBytes(key)
	if err != nil || got != want {
		t.Fatalf("key ID mismatch: %v", err)
	}
}

func TestKeyIDOfBytesRejectsWrongLengths(t *testing.T) {
	for _, n := range []int{31, 33} {
		if _, err := state.KeyIDOfBytes(make([]byte, n)); err == nil {
			t.Fatalf("accepted %d-byte key", n)
		}
	}
}
