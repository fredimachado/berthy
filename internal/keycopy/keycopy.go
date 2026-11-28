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
var afterVerifyBerthy func()
var beforeCleanup func()
var syncDir = syncDirectory
var publish = linkFile

type staging struct {
	parent, dir *os.File
	name, outer string
	outerInfo   os.FileInfo
}

// FileName names a copy without exposing its key.
func FileName(id scope.ID, keyID string) string {
	return "berthy." + id.Project + "." + id.Environment + "." + keyID + ".key"
}

// Present checks only the named copy, without reading it or creating directories.
func Present(home string, id scope.ID, keyID string) (bool, error) {
	path := appendComponent(appendComponent(appendComponent(home, ".berthy"), "keys"), FileName(id, keyID))
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular(), nil
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

// Keep the home prefix intact: cleaning it can change symlink/.. resolution.
func appendComponent(path, name string) string {
	if os.IsPathSeparator(path[len(path)-1]) {
		return path + name
	}
	return path + string(os.PathSeparator) + name
}

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
	bp := appendComponent(home, ".berthy")
	b, err := childDir(h, ".berthy", bp, home)
	if err != nil {
		return "", false, err
	}
	defer b.Close()
	kp := appendComponent(bp, "keys")
	k, err := childDir(b, "keys", kp, bp)
	if err != nil {
		return "", false, err
	}
	defer k.Close()
	if afterPin != nil {
		afterPin()
	}
	name := FileName(id, keyID)
	path := appendComponent(kp, name)
	created, file, err := save(k, kp, name, key)
	if err != nil {
		return "", false, err
	}
	defer file.Close()
	if err = verifyPath(h, b, k, file, name); err != nil {
		return "", false, err
	}
	return path, created, nil
}

func verifyPath(home, berthy, keys, file *os.File, name string) error {
	b, err := openDir(home, ".berthy")
	if err != nil {
		return ErrMoved
	}
	defer b.Close()
	if !samePrivate(b, berthy, true) {
		return ErrMoved
	}
	if afterVerifyBerthy != nil {
		afterVerifyBerthy()
	}
	k, err := openDir(b, "keys")
	if err != nil {
		return ErrMoved
	}
	defer k.Close()
	if !samePrivate(k, keys, true) {
		return ErrMoved
	}
	f, err := openFile(k, name)
	if err != nil {
		return ErrMoved
	}
	defer f.Close()
	if !samePrivate(f, file, false) {
		return ErrMoved
	}
	for _, c := range []struct {
		parent   *os.File
		name     string
		expected *os.File
		dir      bool
	}{
		{k, name, f, false}, {b, "keys", k, true}, {home, ".berthy", b, true},
	} {
		var current *os.File
		if c.dir {
			current, err = openDir(c.parent, c.name)
		} else {
			current, err = openFile(c.parent, c.name)
		}
		if err != nil {
			return ErrMoved
		}
		matches := samePrivate(current, c.expected, c.dir)
		current.Close()
		if !matches {
			return ErrMoved
		}
	}
	return nil
}

func samePrivate(a, b *os.File, dir bool) bool {
	if checkPrivate(a, dir) != nil {
		return false
	}
	ai, err := a.Stat()
	if err != nil {
		return false
	}
	bi, err := b.Stat()
	return err == nil && os.SameFile(ai, bi)
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

func readCopy(dir *os.File, folder, name string, key []byte) (*os.File, error) {
	path := appendComponent(folder, name)
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
		return nil, cannotWrite(folder, err)
	}
	if subtle.ConstantTimeCompare(b, key) != 1 {
		f.Close()
		return nil, occupied(path)
	}
	return f, nil
}

func save(dir *os.File, path, name string, key []byte) (bool, *os.File, error) {
	final := appendComponent(path, name)
	f, err := readCopy(dir, path, name, key)
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
	stage, w, err := createStage(dir, tmp)
	if err != nil {
		return false, nil, cannotWrite(path, err)
	}
	defer w.Close()
	info, statErr := w.Stat()
	if statErr == nil {
		statErr = checkPrivate(w, false)
	}
	if statErr != nil {
		closeStage(stage)
		return false, nil, cannotWrite(path, statErr)
	}
	// The cleanup checks the created identity; it never removes a later occupant.
	cleaned := false
	defer func() {
		if !cleaned {
			removeStage(stage, info)
		}
		closeStage(stage)
	}()
	entry, err := openFile(stage.dir, stage.name)
	if err != nil {
		return false, nil, cannotWrite(path, err)
	}
	current, e := entry.Stat()
	entry.Close()
	if e != nil || !os.SameFile(info, current) {
		return false, nil, cannotWrite(path, errors.New("temporary entry changed"))
	}
	if _, err = w.Write(key); err == nil {
		err = w.Sync()
	}
	if e := finishStageWrite(w); err == nil {
		err = e
	}
	if err != nil {
		return false, nil, cannotWrite(path, err)
	}
	linkErr := publish(stage.dir, dir, stage.name, name, info)
	if beforeCleanup != nil {
		beforeCleanup()
	}
	removeStage(stage, info)
	cleaned = true
	if err = closeStageWrite(w); err != nil {
		return false, nil, cannotWrite(path, err)
	}
	if linkErr != nil && !errors.Is(linkErr, fs.ErrExist) {
		return false, nil, cannotWrite(path, linkErr)
	}
	if err = syncDir(dir); err != nil {
		return false, nil, cannotWrite(path, err)
	}
	f, err = readCopy(dir, path, name, key)
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
