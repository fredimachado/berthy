package state

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// randomKey is 32 random bytes.
func randomKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

// writeKey writes a key file and returns its path.
func writeKey(t *testing.T, path string, key []byte) string {
	t.Helper()
	if err := os.WriteFile(path, key, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// keyedStore opens a new state in its own directory with key, whose file sits in a directory of its own.
func keyedStore(t *testing.T, key []byte) (s *Store, dir, keyFile string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	keyFile = writeKey(t, filepath.Join(t.TempDir(), "key"), key)
	s, err := Open(dir, keyFile, "prod")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir, keyFile
}

func TestSealedValueCarriesItsVersionAndKeyID(t *testing.T) {
	key := randomKey(t)
	s, _, _ := keyedStore(t, key)
	sealed, err := s.seal("secret:a\x00", []byte("value"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(append([]byte("berthy-state-key-id\x00"), key...))
	if sealed[0] != 1 || !bytes.Equal(sealed[1:9], sum[:8]) {
		t.Fatalf("header % x, want version 1 and key id %x", sealed[:9], sum[:8])
	}
	if s.KeyID() != hex.EncodeToString(sum[:8]) {
		t.Fatalf("KeyID %s", s.KeyID())
	}
	if got, err := s.open("secret:a\x00", sealed); err != nil || string(got) != "value" {
		t.Fatalf("open: %q %v", got, err)
	}
	if bytes.Contains(sealed, key) || bytes.Contains(sealed, []byte("value")) {
		t.Fatal("the sealed value holds the key or the plaintext")
	}
}

func TestAValueSealedWithAnotherKeyNamesTheKeyItNeeds(t *testing.T) {
	a, _, _ := keyedStore(t, randomKey(t))
	b, _, _ := keyedStore(t, randomKey(t))
	sealed, err := a.seal("secret:a\x00", []byte("value"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.open("secret:a\x00", sealed)
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("open with another key: %v", err)
	}
	if want := "sealed with a key this agent does not have (key " + a.KeyID() + ")"; err.Error() != want {
		t.Fatalf("message %q, want %q", err, want)
	}
}

func TestATamperedHeaderOrRecordIsRefused(t *testing.T) {
	s, _, _ := keyedStore(t, randomKey(t))
	sealed, err := s.seal("secret:a\x00", []byte("value"))
	if err != nil {
		t.Fatal(err)
	}
	version := bytes.Clone(sealed)
	version[0] = 2
	if _, err = s.open("secret:a\x00", version); err == nil || errors.Is(err, ErrUnknownKey) || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("unknown version: %v", err)
	}
	id := bytes.Clone(sealed)
	id[1] ^= 0xff
	if _, err = s.open("secret:a\x00", id); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("changed key id: %v", err)
	}
	if _, err = s.open("secret:b\x00", sealed); err == nil {
		t.Fatal("a value opened under another record")
	}
	if _, err = s.open("secret:a\x00", sealed[:10]); err == nil {
		t.Fatal("a truncated value opened")
	}
	// The header is part of what the tag covers: a header that names the right key but is swapped in is not enough on its own.
	other := bytes.Clone(sealed)
	other[0] = 1
	other[9] ^= 1
	if _, err = s.open("secret:a\x00", other); err == nil {
		t.Fatal("a changed nonce opened")
	}
}

func TestKeyIDOfFileNamesAKeyWithoutRevealingIt(t *testing.T) {
	key := randomKey(t)
	path := writeKey(t, filepath.Join(t.TempDir(), "key"), key)
	id, err := KeyIDOfFile(path)
	if err != nil || len(id) != 16 || strings.Contains(id, hex.EncodeToString(key[:4])) {
		t.Fatalf("key id %q: %v", id, err)
	}
	s, _, _ := keyedStore(t, key)
	if id != s.KeyID() {
		t.Fatalf("the file says %s and the store %s", id, s.KeyID())
	}
	if _, err = KeyIDOfFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing file has a key id")
	}
}
