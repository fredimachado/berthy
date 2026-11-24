//go:build unix

package keycopy

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSavePublishesFromItsPinnedStage(t *testing.T) {
	home := t.TempDir()
	key := testKey(43)
	var replacement string
	var hookErr error
	afterStageCheck = func() {
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
		hookErr = errors.New("stage missing")
	}
	t.Cleanup(func() { afterStageCheck = nil })
	path, made, err := Save(home, testID, key)
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err != nil || !made {
		t.Fatalf("save: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatal("published the wrong entry")
	}
	got, err = os.ReadFile(replacement)
	if err != nil || string(got) != "other data" {
		t.Fatal("replacement removed or changed")
	}
}

func TestSaveRefusesInexactUnixModes(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  bool
		mode os.FileMode
	}{
		{"folder", true, 0755}, {"read only", false, 0400}, {"world readable", false, 0644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			key := testKey(30)
			path, _, err := Save(home, testID, key)
			if err != nil {
				t.Fatal(err)
			}
			target := path
			want := "not a private file"
			if tc.dir {
				target = filepath.Dir(path)
				want = "not a private folder"
			}
			if err = os.Chmod(target, tc.mode); err != nil {
				t.Fatal(err)
			}
			_, _, err = Save(home, testID, key)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("expected privacy refusal: %v", err)
			}
			fi, err := os.Stat(target)
			if err != nil || fi.Mode().Perm() != tc.mode {
				t.Fatal("mode changed")
			}
		})
	}
}
func TestSaveRefusesUnixLinks(t *testing.T) {
	for _, component := range []string{"berthy", "berthy-outside", "keys", "file"} {
		t.Run(component, func(t *testing.T) {
			home := t.TempDir()
			target := filepath.Join(home, "target")
			if component == "berthy-outside" {
				target = t.TempDir()
			} else if err := os.Mkdir(target, 0700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(home, ".berthy")
			if component == "keys" || component == "file" {
				path, _, err := Save(home, testID, testKey(31))
				if err != nil {
					t.Fatal(err)
				}
				link = filepath.Dir(path)
				if component == "file" {
					link = path
				}
			}
			if component == "keys" || component == "file" {
				if err := os.Rename(link, link+".old"); err != nil {
					t.Fatal(err)
				}
			}
			if component == "file" {
				if err := os.WriteFile(filepath.Join(target, "victim"), testKey(31), 0600); err != nil {
					t.Fatal(err)
				}
				target = filepath.Join(target, "victim")
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Save(home, testID, testKey(31)); err == nil || (component == "file" && !strings.Contains(err.Error(), "not a private file")) {
				t.Fatal("accepted link")
			}
			if component == "file" {
				got, err := os.ReadFile(target)
				if err != nil || string(got) != string(testKey(31)) {
					t.Fatal("link target changed")
				}
				return
			}
			entries, err := os.ReadDir(target)
			if err != nil || len(entries) != 0 {
				t.Fatal("link target changed")
			}
		})
	}
}
