// Package keycopy stores private, immutable local copies of state keys.
package keycopy

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"berthy/internal/scope"
	"berthy/internal/state"
)

// ErrMoved means the checked key folder no longer names the published copy.
var ErrMoved = errors.New("the key folder changed while the copy was written")

// Test hooks are set only by tests that do not run concurrently.
var afterPin func()
var beforeCleanup func()
var syncDir = syncDirectory
var publish = linkFile

// FileName names a copy without exposing its key.
func FileName(id scope.ID, keyID string) string {
	return "berthy." + id.Project + "." + id.Environment + "." + keyID + ".key"
}

func cannotWrite(path string, err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return fmt.Errorf("cannot write the key copy in %s: %w", path, err)
}
func notPrivate(path string) error { return fmt.Errorf("%s is not a private folder of yours", path) }
func notUsable(path string) error {
	return fmt.Errorf("%s is there and is not a private file of yours", path)
}
func occupied(path string) error { return fmt.Errorf("%s is there and is not this key", path) }

// Save stores key below home without replacing existing copies.
func Save(home string, id scope.ID, key []byte) (string, bool, error) {
	if !filepath.IsAbs(home) || !scope.ValidProject(id.Project) || !scope.ValidEnvironment(id.Environment) || len(key) != 32 {
		return "", false, errors.New("invalid key copy input")
	}
	keyID, err := state.KeyIDOfBytes(key)
	if err != nil {
		return "", false, err
	}
	h, err := os.Open(home)
	if err != nil {
		return "", false, cannotWrite(home, err)
	}
	defer h.Close()
	bp := filepath.Join(home, ".berthy")
	b, err := childDir(h, ".berthy", bp, home)
	if err != nil {
		return "", false, err
	}
	defer b.Close()
	kp := filepath.Join(bp, "keys")
	k, err := childDir(b, "keys", kp, bp)
	if err != nil {
		return "", false, err
	}
	defer k.Close()
	if afterPin != nil {
		afterPin()
	}
	name := FileName(id, keyID)
	path := filepath.Join(kp, name)
	created, file, err := save(k, kp, name, key)
	if err != nil {
		return "", false, err
	}
	defer file.Close()
	for _, c := range []struct {
		path string
		f    *os.File
	}{{bp, b}, {kp, k}, {path, file}} {
		pi, e := os.Lstat(c.path)
		if e != nil || isLink(pi) {
			return "", false, ErrMoved
		}
		hi, e := c.f.Stat()
		if e != nil || !os.SameFile(pi, hi) {
			return "", false, ErrMoved
		}
	}
	return path, created, nil
}

func childDir(parent *os.File, name, path, parentPath string) (*os.File, error) {
	err := makeDir(parent, name)
	made := err == nil
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, cannotWrite(parentPath, err)
	}
	f, err := openDir(parent, name)
	if err != nil {
		return nil, notPrivate(path)
	}
	if err = checkPrivate(f, true); err != nil {
		f.Close()
		return nil, notPrivate(path)
	}
	if made {
		if err = syncDir(parent); err != nil {
			f.Close()
			return nil, cannotWrite(parentPath, err)
		}
	}
	return f, nil
}

func readCopy(dir *os.File, path, name string, key []byte) (*os.File, error) {
	f, err := openFile(dir, name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fs.ErrNotExist
	}
	if err != nil {
		return nil, notUsable(path)
	}
	if err = checkPrivate(f, false); err != nil {
		f.Close()
		return nil, notUsable(path)
	}
	b, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil {
		f.Close()
		return nil, cannotWrite(filepath.Dir(path), err)
	}
	if subtle.ConstantTimeCompare(b, key) != 1 {
		f.Close()
		return nil, occupied(path)
	}
	return f, nil
}

func save(dir *os.File, path, name string, key []byte) (bool, *os.File, error) {
	final := filepath.Join(path, name)
	f, err := readCopy(dir, final, name, key)
	if err == nil {
		return false, f, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return false, nil, err
	}
	var nonce [8]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return false, nil, cannotWrite(path, err)
	}
	tmp := "." + name + "." + hex.EncodeToString(nonce[:]) + ".tmp"
	w, err := createFile(dir, tmp)
	if err != nil {
		return false, nil, cannotWrite(path, err)
	}
	info, statErr := w.Stat()
	if statErr == nil {
		statErr = checkPrivate(w, false)
	}
	if statErr != nil {
		w.Close()
		return false, nil, cannotWrite(path, statErr)
	}
	// The cleanup checks the created identity; it never removes a later occupant.
	cleaned := false
	defer func() {
		if !cleaned {
			removeOwn(dir, tmp, info)
		}
	}()
	entry, err := openFile(dir, tmp)
	if err != nil {
		w.Close()
		return false, nil, cannotWrite(path, err)
	}
	current, e := entry.Stat()
	entry.Close()
	if e != nil || !os.SameFile(info, current) {
		w.Close()
		return false, nil, cannotWrite(path, errors.New("temporary entry changed"))
	}
	if _, err = w.Write(key); err == nil {
		err = w.Sync()
	}
	if e := w.Close(); err == nil {
		err = e
	}
	if err != nil {
		return false, nil, cannotWrite(path, err)
	}
	linkErr := publish(dir, tmp, name, info)
	if beforeCleanup != nil {
		beforeCleanup()
	}
	removeOwn(dir, tmp, info)
	cleaned = true
	if linkErr != nil && !errors.Is(linkErr, fs.ErrExist) {
		return false, nil, cannotWrite(path, linkErr)
	}
	if err = syncDir(dir); err != nil {
		return false, nil, cannotWrite(path, err)
	}
	f, err = readCopy(dir, final, name, key)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if errors.Is(linkErr, fs.ErrExist) {
				return false, nil, occupied(final)
			}
			return false, nil, cannotWrite(path, errors.New("published copy disappeared"))
		}
		return false, nil, err
	}
	return linkErr == nil, f, nil
}
