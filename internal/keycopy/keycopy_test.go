package keycopy

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"berthy/internal/scope"
	"berthy/internal/state"
)

var testID = scope.ID{Project: "shop", Environment: "prod"}

func testKey(n byte) []byte { return bytes.Repeat([]byte{n}, 32) }

func TestAppendComponentPreservesRootAndTrailingSeparators(t *testing.T) {
	sep := string(os.PathSeparator)
	for _, prefix := range []string{sep, sep + "home", sep + "home" + sep, sep + "link" + sep + ".." + sep + "home"} {
		want := prefix
		if !os.IsPathSeparator(prefix[len(prefix)-1]) {
			want += sep
		}
		want += ".berthy"
		if got := appendComponent(prefix, ".berthy"); got != want {
			t.Errorf("append %q: got %q, want %q", prefix, got, want)
		}
	}
}

func snapshotFile(t *testing.T, path string) os.FileInfo {
	t.Helper()
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	f, err := openFile(dir, filepath.Base(path))
	dir.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func makePrivateFixture(t *testing.T, path string, content []byte) {
	t.Helper()
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	f, err := createFile(dir, filepath.Base(path))
	dir.Close()
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Write(content)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestIdentitySnapshotDetectsReplacement(t *testing.T) {
	home := t.TempDir()
	path, _, err := Save(home, testID, testKey(41))
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotFile(t, path)
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	makePrivateFixture(t, path, testKey(41))
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after := snapshotFile(t, path)
	if os.SameFile(before, after) {
		t.Fatal("replacement has the original identity")
	}
}

func TestAbandonedFixtureIsPrivate(t *testing.T) {
	home := t.TempDir()
	path, _, err := Save(home, testID, testKey(42))
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(filepath.Dir(path), ".abandoned.tmp")
	makePrivateFixture(t, tmp, testKey(42))
	assertPrivatePath(t, tmp, false)
}

func TestSaveWritesAndRetainsPrivateCopies(t *testing.T) {
	home := t.TempDir()
	key := testKey(17)
	path, created, err := Save(home, testID, key)
	if err != nil || !created {
		t.Fatalf("first save: created=%v err=%v", created, err)
	}
	id, err := state.KeyIDOfBytes(key)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(home, ".berthy", "keys", "berthy.shop.prod."+id+".key") {
		t.Fatal("unexpected path")
	}
	assertPrivatePath(t, filepath.Join(home, ".berthy"), true)
	assertPrivatePath(t, filepath.Join(home, ".berthy", "keys"), true)
	assertPrivatePath(t, path, false)
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatal("copy differs")
	}
	before := snapshotFile(t, path)
	again, made, err := Save(home, testID, key)
	if err != nil || made || again != path {
		t.Fatalf("repeat: created=%v err=%v", made, err)
	}
	after := snapshotFile(t, path)
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("copy replaced")
	}
	var replacement string
	var replaceErr error
	beforeCleanup = func() {
		entries, e := os.ReadDir(filepath.Dir(path))
		if e != nil {
			replaceErr = e
			return
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".tmp") {
				replacement = filepath.Join(filepath.Dir(path), entry.Name())
				if replaceErr = os.Rename(replacement, replacement+".moved"); replaceErr == nil {
					replaceErr = os.WriteFile(replacement, []byte("other data"), 0600)
				}
				return
			}
		}
		replaceErr = errors.New("temporary entry missing")
	}
	t.Cleanup(func() { beforeCleanup = nil })
	other, made, err := Save(home, testID, testKey(18))
	beforeCleanup = nil
	if replaceErr != nil {
		t.Fatal(replaceErr)
	}
	if err != nil || !made || other == path {
		t.Fatalf("second key: created=%v err=%v", made, err)
	}
	if got, err := os.ReadFile(replacement); err != nil || string(got) != "other data" {
		t.Fatal("replacement changed")
	}
	got, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatal("original changed")
	}
}

func TestSaveRefusesOccupiedAndInvalidInput(t *testing.T) {
	home := t.TempDir()
	key := testKey(2)
	path, _, err := Save(home, testID, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, testKey(3), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Save(home, testID, key); err == nil || !strings.Contains(err.Error(), "is not this key") {
		t.Fatalf("occupied: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, testKey(3)) {
		t.Fatal("occupied file changed")
	}
	if err := os.WriteFile(path, []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Save(home, testID, key); err == nil || !strings.Contains(err.Error(), "is not this key") {
		t.Fatalf("short occupied: %v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil || string(got) != "other" {
		t.Fatal("short occupied file changed")
	}
	for _, tc := range []struct {
		home string
		id   scope.ID
		key  []byte
	}{
		{"relative", testID, key}, {home, scope.ID{Project: "../x", Environment: "prod"}, key},
		{home, scope.ID{Project: "shop", Environment: "../x"}, key}, {home, testID, key[:31]},
	} {
		if _, _, err := Save(tc.home, tc.id, tc.key); err == nil {
			t.Fatal("accepted invalid input")
		}
	}
}

func TestConcurrentSaves(t *testing.T) {
	home := t.TempDir()
	key := testKey(4)
	const n = 8
	var wg sync.WaitGroup
	results := make(chan struct {
		path string
		made bool
		err  error
	}, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, c, e := Save(home, testID, key)
			results <- struct {
				path string
				made bool
				err  error
			}{p, c, e}
		}()
	}
	wg.Wait()
	close(results)
	var path string
	count := 0
	for r := range results {
		if r.err != nil {
			t.Fatalf("save: %v", r.err)
		}
		if path != "" && r.path != path {
			t.Fatal("different paths")
		}
		path = r.path
		if r.made {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("created %d times", count)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries: %d err=%v", len(entries), err)
	}
}

func TestSaveReportsMovedFolder(t *testing.T) {
	home := t.TempDir()
	planted := filepath.Join(home, "plant")
	if err := os.MkdirAll(filepath.Join(planted, "keys"), 0700); err != nil {
		t.Fatal(err)
	}
	var hookErr error
	afterPin = func() {
		hookErr = os.Rename(filepath.Join(home, ".berthy"), filepath.Join(home, "moved"))
		if hookErr == nil {
			hookErr = plantLink(filepath.Join(home, ".berthy"), planted)
		}
	}
	t.Cleanup(func() { afterPin = nil })
	path, _, err := Save(home, testID, testKey(5))
	if hookErr != nil {
		if _, statErr := os.Stat(filepath.Join(home, ".berthy")); statErr == nil {
			if err != nil || path == "" {
				t.Fatalf("rename refused, save failed: %v", err)
			}
			return
		}
		t.Fatalf("plant link: %v", hookErr)
	}
	if path != "" || !errors.Is(err, ErrMoved) {
		t.Fatalf("moved: path=%q err=%v", path, err)
	}
	entries, e := os.ReadDir(filepath.Join(planted, "keys"))
	if e != nil || len(entries) != 0 {
		t.Fatalf("planted folder changed: %v", e)
	}
	moved, e := os.ReadDir(filepath.Join(home, "moved", "keys"))
	if e != nil || len(moved) > 1 {
		t.Fatal("unexpected moved folder contents")
	}
	if len(moved) == 1 {
		got, e := os.ReadFile(filepath.Join(home, "moved", "keys", moved[0].Name()))
		if e != nil || !bytes.Equal(got, testKey(5)) {
			t.Fatal("moved copy incomplete")
		}
	}
}

func TestSaveRejectsLinkToMovedFolderDuringVerification(t *testing.T) {
	home := t.TempDir()
	var moveErr error
	afterVerifyBerthy = func() {
		moveErr = os.Rename(filepath.Join(home, ".berthy"), filepath.Join(home, "moved"))
		if moveErr == nil {
			moveErr = plantLink(filepath.Join(home, ".berthy"), filepath.Join(home, "moved"))
		}
	}
	t.Cleanup(func() { afterVerifyBerthy = nil })
	path, _, err := Save(home, testID, testKey(40))
	if moveErr != nil {
		if _, e := os.Lstat(filepath.Join(home, ".berthy")); e == nil && err == nil && path != "" {
			return
		}
		t.Fatalf("move or link failed: %v, save: %v", moveErr, err)
	}
	if path != "" || !errors.Is(err, ErrMoved) {
		t.Fatalf("returned path through moved directory: %q, %v", path, err)
	}
}

func TestSaveDoesNotRemoveAReplacedTemporaryEntry(t *testing.T) {
	home := t.TempDir()
	var replacement string
	var hookErr error
	beforeCleanup = func() {
		folder := filepath.Join(home, ".berthy", "keys")
		entries, e := os.ReadDir(folder)
		if e != nil {
			hookErr = e
			return
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".tmp") {
				replacement = filepath.Join(folder, entry.Name())
				if hookErr = os.Rename(replacement, replacement+".moved"); hookErr == nil {
					hookErr = os.WriteFile(replacement, []byte("other data"), 0600)
				}
				return
			}
		}
		hookErr = errors.New("temporary entry missing")
	}
	t.Cleanup(func() { beforeCleanup = nil })
	path, created, err := Save(home, testID, testKey(7))
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err != nil || !created {
		t.Fatalf("save: %v", err)
	}
	got, err := os.ReadFile(replacement)
	if err != nil || string(got) != "other data" {
		t.Fatal("replacement was removed or changed")
	}
	got, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(got, testKey(7)) {
		t.Fatal("published copy changed")
	}
}

func TestSyncFailureIsNotRescuedByPublishedFile(t *testing.T) {
	home := t.TempDir()
	if _, _, err := Save(home, testID, testKey(8)); err != nil {
		t.Fatal(err)
	}
	realSync := syncDir
	syncDir = func(*os.File) error { return errors.New("injected sync failure") }
	t.Cleanup(func() { syncDir = realSync })
	path, _, err := Save(home, testID, testKey(9))
	if path != "" || err == nil || !strings.Contains(err.Error(), "injected sync failure") {
		t.Fatalf("sync failure: %v", err)
	}
	syncDir = realSync
	path, made, err := Save(home, testID, testKey(9))
	if err != nil || made || path == "" {
		t.Fatalf("repeat: made=%v err=%v", made, err)
	}
}

func TestPublicationErrorIsNotRescuedByPublishedFile(t *testing.T) {
	home := t.TempDir()
	realPublish := publish
	publish = func(from, to *os.File, old, name string, info os.FileInfo) error {
		if err := realPublish(from, to, old, name, info); err != nil {
			return err
		}
		return errors.New("injected publication failure")
	}
	t.Cleanup(func() { publish = realPublish })
	path, _, err := Save(home, testID, testKey(12))
	if path != "" || err == nil || !strings.Contains(err.Error(), "injected publication failure") {
		t.Fatalf("publication failure: %v", err)
	}
	publish = realPublish
	path, made, err := Save(home, testID, testKey(12))
	if err != nil || made || path == "" {
		t.Fatalf("repeat: made=%v err=%v", made, err)
	}
}

func TestFileNameAvoidsWindowsDeviceNames(t *testing.T) {
	home := t.TempDir()
	id := scope.ID{Project: "con", Environment: "nul"}
	path, created, err := Save(home, id, testKey(13))
	if err != nil || !created {
		t.Fatalf("save: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(path), "berthy.con.nul.") {
		t.Fatal("incorrect filename")
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatal("not a regular file")
	}
}

func TestSaveErrorDoesNotRevealKey(t *testing.T) {
	home := t.TempDir()
	key := testKey(10)
	path, _, err := Save(home, testID, key)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, testKey(11), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err = Save(home, testID, key)
	if err == nil {
		t.Fatal("expected refusal")
	}
	for _, secret := range []string{string(key), hex.EncodeToString(key), base64.StdEncoding.EncodeToString(key), base64.RawStdEncoding.EncodeToString(key), base64.URLEncoding.EncodeToString(key), base64.RawURLEncoding.EncodeToString(key)} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("error reveals key")
		}
	}
}

func TestSaveLeavesEarlierTemporaryFiles(t *testing.T) {
	home := t.TempDir()
	key := testKey(6)
	path, _, err := Save(home, testID, testKey(14))
	if err != nil {
		t.Fatal(err)
	}
	id, err := state.KeyIDOfBytes(key)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	var before []os.FileInfo
	for i, content := range [][]byte{nil, key} {
		tmp := filepath.Join(filepath.Dir(path), "."+FileName(testID, id)+fmt.Sprintf(".%016x.tmp", i))
		makePrivateFixture(t, tmp, content)
		info := snapshotFile(t, tmp)
		paths = append(paths, tmp)
		before = append(before, info)
	}
	for i := range 2 {
		_, made, err := Save(home, testID, key)
		if err != nil || made != (i == 0) {
			t.Fatalf("save %d: %v", i, err)
		}
		for j, tmp := range paths {
			after := snapshotFile(t, tmp)
			if !os.SameFile(before[j], after) || before[j].Size() != after.Size() || !before[j].ModTime().Equal(after.ModTime()) {
				t.Fatal("earlier temporary file changed")
			}
		}
	}
}
