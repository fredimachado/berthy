//go:build unix

package keycopy

import (
	"os"
	"syscall"
	"testing"
)

func assertPrivatePath(t *testing.T, path string, dir bool) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	want := os.FileMode(0600)
	if dir {
		want = 0700
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) || fi.IsDir() != dir || fi.Mode().Perm() != want {
		t.Fatal("incorrect owner, type, or mode")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	acl, err := hasACL(f)
	if err != nil || acl {
		t.Fatalf("unexpected ACL: %v", err)
	}
}
