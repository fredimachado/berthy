//go:build windows

package keycopy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func setDACL(t *testing.T, path, sddl string) {
	t.Helper()
	var acl *windows.ACL
	if sddl != "" {
		sd, err := windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			t.Fatal(err)
		}
		acl, _, err = sd.DACL()
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}
func TestSaveRefusesUnsafeWindowsDACL(t *testing.T) {
	for _, tc := range []struct {
		name string
		file bool
		sddl string
	}{
		{"null directory", false, ""}, {"null file", true, ""}, {"extra entry", false, "extra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			key := testKey(23)
			path, _, err := Save(home, testID, key)
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Dir(path)
			want := "not a private folder"
			if tc.file {
				target = path
				want = "not a private file"
			}
			sddl := tc.sddl
			if sddl == "extra" {
				sid, e := currentSID()
				if e != nil {
					t.Fatal(e)
				}
				sddl = "D:P(A;OICI;FA;;;" + sid.String() + ")(A;OICI;FR;;;WD)"
			}
			setDACL(t, target, sddl)
			_, _, err = Save(home, testID, key)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("expected privacy refusal: %v", err)
			}
		})
	}
}
func TestSaveRefusesInheritedDACL(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".berthy"), 0700); err != nil {
		t.Fatal(err)
	}
	_, _, err := Save(home, testID, testKey(24))
	if err == nil || !strings.Contains(err.Error(), "not a private folder") {
		t.Fatalf("expected privacy refusal: %v", err)
	}
}
func TestSaveRefusesJunctions(t *testing.T) {
	for _, component := range []string{".berthy", "keys"} {
		t.Run(component, func(t *testing.T) {
			home := t.TempDir()
			target := filepath.Join(home, "target")
			if err := os.Mkdir(target, 0700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(home, ".berthy")
			if component == "keys" {
				if _, _, err := Save(home, testID, testKey(25)); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(link, "keys")
				if err := os.Rename(link, filepath.Join(home, "oldkeys")); err != nil {
					t.Fatal(err)
				}
			}
			if err := plantLink(link, target); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Save(home, testID, testKey(26)); err == nil {
				t.Fatal("accepted junction")
			}
			entries, err := os.ReadDir(target)
			if err != nil || len(entries) != 0 {
				t.Fatal("junction target changed")
			}
		})
	}
}

func TestSaveRefusesWindowsFileSymlink(t *testing.T) {
	home := t.TempDir()
	key := testKey(27)
	path, _, err := Save(home, testID, key)
	if err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(home, "victim")
	if err = os.Rename(path, victim); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(victim, path); err != nil {
		t.Skipf("file symlinks unavailable: %v", err)
	}
	_, _, err = Save(home, testID, key)
	if err == nil || !strings.Contains(err.Error(), "not a private file") {
		t.Fatalf("expected link refusal: %v", err)
	}
	got, err := os.ReadFile(victim)
	if err != nil || string(got) != string(key) {
		t.Fatal("victim changed")
	}
}
