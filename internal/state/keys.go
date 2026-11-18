package state

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
)

// A state key lives in one file, and a rotation uses two more beside it: <key>.next while it runs and <key>.prev after it.
//
//	<key>       the key the agent seals with
//	<key>.next  the key a rotation is moving to. It exists from before the rotation commits until its files are renamed.
//	<key>.prev  the key before the last rotation, kept until a state snapshot made with the current key has succeeded.
//
// The state's own key check says which key is current, so a crash at any point leaves a state that Open can settle.
const (
	nextSuffix = ".next"
	prevSuffix = ".prev"
)

// keyFiles are the keys found beside a key file. next and prev are nil when absent or unreadable: only main must be there.
type keyFiles struct {
	main       stateKey
	next, prev *stateKey
}

func loadKeyFiles(keyFile string) (keyFiles, error) {
	raw, err := readKey(keyFile)
	if err != nil {
		return keyFiles{}, err
	}
	var kf keyFiles
	if kf.main, err = newStateKey(raw); err != nil {
		return keyFiles{}, err
	}
	optional := func(suffix string) *stateKey {
		raw, err := readKey(keyFile + suffix)
		if err != nil {
			return nil
		}
		k, err := newStateKey(raw)
		if err != nil {
			return nil
		}
		return &k
	}
	kf.next, kf.prev = optional(nextSuffix), optional(prevSuffix)
	return kf, nil
}

// KeyIDOfFile is the ID of the key a key file holds, as KeyID shows it for a store. It reads the file as Open does.
func KeyIDOfFile(path string) (string, error) {
	raw, err := readKey(path)
	if err != nil {
		return "", err
	}
	return keyIDOf(raw).String(), nil
}

// chooseKeys picks the current key from what the state says sealed its key check. The key file is current, unless the state
// was sealed with the next key: then a rotation committed and the agent stopped before it renamed the files. finish says
// so, and settleKeyFiles then renames the files once the state is open.
func chooseKeys(kf keyFiles, check []byte) (ring keyring, finish bool, err error) {
	id, err := sealedKeyID(check)
	if err != nil {
		return keyring{}, false, err
	}
	switch {
	case id == kf.main.id:
		ring = keyring{current: kf.main}
		if kf.prev != nil && kf.prev.id != kf.main.id {
			ring.others = []stateKey{*kf.prev}
		}
	case kf.next != nil && id == kf.next.id:
		ring, finish = keyring{current: *kf.next, others: []stateKey{kf.main}}, true
	default:
		return keyring{}, false, ErrWrongKey
	}
	if plain, err := openWith(ring, "", keyCheckName, check); err != nil || string(plain) != keyCheckPlain {
		return keyring{}, false, ErrWrongKey
	}
	return ring, finish, nil
}

const keyCheckPlain = "berthy-state"

// settleKeyFiles puts the key files in the state the key check says they are in. When the check was sealed with the key file,
// a next key is the leftover of a rotation that never committed and is removed. When it was sealed with the next key, the
// rotation committed and the renames are finished here.
func settleKeyFiles(keyFile string, finish bool, old stateKey) error {
	if finish {
		return finishRotation(keyFile, old)
	}
	if err := os.Remove(keyFile + nextSuffix); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// finishRotation renames the key files after a rotation committed: the old key is kept as <key>.prev and the next key takes
// the key's place. The key file exists at every step, and the steps can be run again after a crash.
func finishRotation(keyFile string, old stateKey) error {
	if err := writeKeyFile(keyFile+prevSuffix, old.raw); err != nil {
		return err
	}
	if err := os.Rename(keyFile+nextSuffix, keyFile); err != nil {
		return err
	}
	return syncDir(filepath.Dir(keyFile))
}

// writeKeyFile writes a private key file that appears whole or not at all.
func writeKeyFile(path string, key []byte) error {
	tmp := path + ".tmp"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(key)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncDir makes the directory's entries durable. Windows cannot sync a directory, and a key rotation there is only for tests.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err = d.Sync(); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return nil
}

// HasPreviousKey reports whether the store still holds the key from before the last rotation.
func (s *Store) HasPreviousKey() bool { return len(s.ring().others) > 0 }

// ForgetPreviousKey drops the key from before the last rotation and deletes its file, once a state snapshot sealed with the
// current key, whose ID is snapshotKeyID, has been stored: from then on the copy of the previous key is not needed to restore.
// A snapshot of another key, or no previous key, changes nothing.
func (s *Store) ForgetPreviousKey(ctx context.Context, snapshotKeyID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.rotate.Lock()
	defer s.rotate.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if snapshotKeyID != s.keys.current.id.String() {
		return nil
	}
	if err := os.Remove(s.keyFile + prevSuffix); err != nil && !os.IsNotExist(err) {
		return err
	}
	s.keys.others = nil
	return syncDir(filepath.Dir(s.keyFile))
}
