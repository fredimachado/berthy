//go:build linux

package state

import (
	"os"
	"path/filepath"
	"testing"
)

const strictKeyMode = true

func TestDatabaseRejectsRedirects(t *testing.T) {
	for _, name := range []string{"state.db", "state.db-wal", "state.db-shm", "state.db-journal"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "private")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			key := filepath.Join(dir, "key")
			if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(dir, "target")
			if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			if s, err := Open(dir, key, "test"); err == nil {
				s.Close()
				t.Fatal("accepted symlink")
			}
			b, _ := os.ReadFile(target)
			if string(b) != "untouched" {
				t.Fatal("target changed")
			}
		})
	}
}
func TestDatabaseRejectsHardLink(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	os.WriteFile(key, make([]byte, 32), 0600)
	target := filepath.Join(dir, "target")
	os.WriteFile(target, []byte("untouched"), 0600)
	if err := os.Link(target, filepath.Join(dir, "state.db")); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(dir, key, "test"); err == nil {
		s.Close()
		t.Fatal("accepted hard link")
	}
}
