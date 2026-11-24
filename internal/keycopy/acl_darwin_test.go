//go:build darwin

package keycopy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHasACLIsFalseForAPlainFile(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "plain"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	has, err := hasACL(f)
	if err != nil || has {
		t.Fatalf("plain file ACL: %v, %v", has, err)
	}
}
func TestSaveRefusesMacACLs(t *testing.T) {
	t.Run("inherited", func(t *testing.T) {
		home := t.TempDir()
		if out, err := exec.Command("chmod", "+a", "everyone allow read,list,search,file_inherit,directory_inherit", home).CombinedOutput(); err != nil {
			t.Fatalf("chmod: %v: %s", err, out)
		}
		_, _, err := Save(home, testID, testKey(32))
		if err == nil || !strings.Contains(err.Error(), "not a private folder") {
			t.Fatalf("expected ACL refusal: %v", err)
		}
	})
	t.Run("existing", func(t *testing.T) {
		home := t.TempDir()
		path, _, err := Save(home, testID, testKey(33))
		if err != nil {
			t.Fatal(err)
		}
		if out, e := exec.Command("chmod", "+a", "everyone allow read", path).CombinedOutput(); e != nil {
			t.Fatalf("chmod: %v: %s", e, out)
		}
		_, _, err = Save(home, testID, testKey(33))
		if err == nil || !strings.Contains(err.Error(), "not a private file") {
			t.Fatalf("expected ACL refusal: %v", err)
		}
	})
}
