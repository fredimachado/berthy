package state

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// A sealed value is version || key ID || nonce || ciphertext and tag. The version byte says how the rest is laid out and which
// cipher made it. The key ID says which key sealed it, so a state that holds the current key and the previous one opens each
// value with the right one. Version and key ID are also part of what the tag covers, so a header cannot be swapped.
const (
	sealVersion  = 1
	keyIDLen     = 8
	sealHeadLen  = 1 + keyIDLen
	keyIDLabel   = "berthy-state-key-id\x00"
	keyCheckName = "key_check"
)

var (
	// ErrUnknownKey means a sealed value names a key this agent does not hold. The error carries the key's ID, never the key.
	ErrUnknownKey = errors.New("sealed with a key this agent does not have")
	// ErrSealFormat means a value is not in a sealed format this agent reads.
	ErrSealFormat = errors.New("sealed value in an unknown format")
)

// keyID names a key without revealing it: the first bytes of SHA-256 over a fixed label and the key.
type keyID [keyIDLen]byte

func (k keyID) String() string { return hex.EncodeToString(k[:]) }

func keyIDOf(raw []byte) keyID {
	sum := sha256.Sum256(append([]byte(keyIDLabel), raw...))
	return keyID(sum[:keyIDLen])
}

// stateKey is a key with its ID and the cipher made from it. raw is kept so a rotation can write the key to a file.
type stateKey struct {
	raw  []byte
	id   keyID
	aead cipher.AEAD
}

func newStateKey(raw []byte) (stateKey, error) {
	block, err := aes.NewCipher(raw)
	if err != nil {
		return stateKey{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return stateKey{}, err
	}
	return stateKey{raw: raw, id: keyIDOf(raw), aead: aead}, nil
}

// keyring is the keys a store opens values with: the current one, which seals, and the others it still reads, which are the
// previous key between a rotation and the next state snapshot.
type keyring struct {
	current stateKey
	others  []stateKey
}

func (r keyring) find(id keyID) (stateKey, bool) {
	if r.current.id == id {
		return r.current, true
	}
	for _, k := range r.others {
		if k.id == id {
			return k, true
		}
	}
	return stateKey{}, false
}

// associated is what a sealed value is bound to besides its key: the environment and the record it belongs to, so a value
// moved to another row does not open. The key check is the same for every environment.
func associated(env, record string) string {
	if record == keyCheckName {
		return "berthy-state-key-check"
	}
	return env + "\x00" + record
}

func additional(head []byte, env, record string) []byte {
	return append(append([]byte{}, head...), associated(env, record)...)
}

func sealWith(k stateKey, env, record string, p []byte) ([]byte, error) {
	head := append([]byte{sealVersion}, k.id[:]...)
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return k.aead.Seal(append(head, nonce...), nonce, p, additional(head, env, record)), nil
}

// sealedKeyID reads the ID of the key that sealed b.
func sealedKeyID(b []byte) (keyID, error) {
	if len(b) < sealHeadLen {
		return keyID{}, errors.New("invalid ciphertext")
	}
	if b[0] != sealVersion {
		return keyID{}, fmt.Errorf("%w: version %d", ErrSealFormat, b[0])
	}
	return keyID(b[1:sealHeadLen]), nil
}

func openWith(r keyring, env, record string, b []byte) ([]byte, error) {
	id, err := sealedKeyID(b)
	if err != nil {
		return nil, err
	}
	k, ok := r.find(id)
	if !ok {
		return nil, fmt.Errorf("%w (key %s)", ErrUnknownKey, id)
	}
	n := k.aead.NonceSize()
	if len(b) < sealHeadLen+n {
		return nil, errors.New("invalid ciphertext")
	}
	return k.aead.Open(nil, b[sealHeadLen:sealHeadLen+n], b[sealHeadLen+n:], additional(b[:sealHeadLen], env, record))
}

func (s *Store) ring() keyring {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.keys
}

func (s *Store) seal(record string, p []byte) ([]byte, error) { return s.sealFor(s.env, record, p) }
func (s *Store) open(record string, b []byte) ([]byte, error) { return s.openFor(s.env, record, b) }

// sealFor and openFor name the environment of the value, which is the store's own except while a rotation reseals a row that
// belongs to another one.
func (s *Store) sealFor(env, record string, p []byte) ([]byte, error) {
	return sealWith(s.ring().current, env, record, p)
}
func (s *Store) openFor(env, record string, b []byte) ([]byte, error) {
	return openWith(s.ring(), env, record, b)
}

// KeyID is the ID of the key the store seals with now: 16 hex characters that name the key and never reveal it.
func (s *Store) KeyID() string { return s.ring().current.id.String() }
