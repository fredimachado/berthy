//go:build unix

package volarchive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"syscall"
	"testing"
	"time"
)

// fixtureTime is a modification time with a fraction of a second, so the archive must keep more than whole seconds.
var fixtureTime = time.Unix(1_650_000_000, 123_456_789)

// makeTree builds the volume a database or an app might leave: nested directories, modes of every kind, links of every
// kind, and the files that cannot be archived.
func makeTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mk := func(rel string, mode fs.FileMode, data string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	mk("dir/nested/deep/file.txt", 0o640, "deep content")
	mk("private", 0o600, "secret")
	mk("run.sh", 0o755, "#!/bin/sh\n")
	mk("setuid", 0o755|fs.ModeSetuid, "suid")
	mk("empty", 0o644, "")
	mk("pair-a", 0o644, "linked")
	if err := os.Link(filepath.Join(root, "pair-a"), filepath.Join(root, "pair-b")); err != nil {
		t.Fatal(err)
	}
	for rel, mode := range map[string]fs.FileMode{
		"emptydir": 0o755, "setgid": 0o755 | fs.ModeSetgid, "sticky": 0o777 | fs.ModeSticky, "dir": 0o750,
	} {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range map[string]string{"abs-link": "/etc/passwd", "rel-link": "dir/nested", "dangling": "../does/not/exist"} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	sock, err := net.Listen("unix", filepath.Join(root, "sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sock.Close() })
	// Times last, deepest first, so nothing above changes them.
	var all []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type()&(fs.ModeSymlink|fs.ModeNamedPipe|fs.ModeSocket) == 0 {
			all = append(all, p)
		}
		return nil
	})
	sort.Sort(sort.Reverse(sort.StringSlice(all)))
	for i, p := range all {
		if err := os.Chtimes(p, fixtureTime, fixtureTime.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func readHeaders(t *testing.T, archive []byte) (map[string]*tar.Header, []string, map[string]string) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	hdrs, data := map[string]*tar.Header{}, map[string]string{}
	var order []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return hdrs, order, data
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		hdrs[h.Name] = h
		data[h.Name] = string(b)
		order = append(order, h.Name)
	}
}

func lstat(t *testing.T, p string) fs.FileInfo {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

func TestWriteArchivesTheTreeWithItsMetadata(t *testing.T) {
	root := makeTree(t)
	var buf bytes.Buffer
	st, err := Write(&buf, root)
	if err != nil {
		t.Fatal(err)
	}
	hdrs, order, data := readHeaders(t, buf.Bytes())
	wantOrder := []string{"./", "abs-link", "dangling", "dir/", "dir/nested/", "dir/nested/deep/", "dir/nested/deep/file.txt",
		"empty", "emptydir/", "pair-a", "pair-b", "private", "rel-link", "run.sh", "setgid/", "setuid", "sticky/"}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("entries\n got %v\nwant %v", order, wantOrder)
	}
	uid, gid := os.Getuid(), os.Getgid()
	for name, want := range map[string]struct {
		typ  byte
		mode fs.FileMode
	}{
		"./":   {tar.TypeDir, lstat(t, root).Mode().Perm() | lstat(t, root).Mode()&(fs.ModeSetgid|fs.ModeSticky)},
		"dir/": {tar.TypeDir, 0o750}, "setgid/": {tar.TypeDir, 0o755 | fs.ModeSetgid},
		"sticky/": {tar.TypeDir, 0o777 | fs.ModeSticky}, "emptydir/": {tar.TypeDir, 0o755},
		"dir/nested/deep/file.txt": {tar.TypeReg, 0o640}, "private": {tar.TypeReg, 0o600}, "run.sh": {tar.TypeReg, 0o755},
		"setuid": {tar.TypeReg, 0o755 | fs.ModeSetuid}, "empty": {tar.TypeReg, 0o644},
	} {
		h := hdrs[name]
		if h.Typeflag != want.typ || h.FileInfo().Mode()&^fs.ModeDir != want.mode || h.Uid != uid || h.Gid != gid {
			t.Errorf("%s: type %c mode %v uid %d gid %d, want %c %v %d %d", name, h.Typeflag, h.FileInfo().Mode(), h.Uid, h.Gid, want.typ, want.mode, uid, gid)
		}
		if h.Uname != "" || h.Gname != "" {
			t.Errorf("%s carries user names %q %q", name, h.Uname, h.Gname)
		}
		if name != "./" && !h.ModTime.Equal(lstat(t, filepath.Join(root, name)).ModTime()) {
			t.Errorf("%s: mtime %v, want %v", name, h.ModTime, lstat(t, filepath.Join(root, name)).ModTime())
		}
	}
	for name, target := range map[string]string{"abs-link": "/etc/passwd", "rel-link": "dir/nested", "dangling": "../does/not/exist"} {
		if h := hdrs[name]; h.Typeflag != tar.TypeSymlink || h.Linkname != target {
			t.Errorf("%s: %c %q", name, h.Typeflag, h.Linkname)
		}
	}
	if h := hdrs["pair-a"]; h.Typeflag != tar.TypeReg || data["pair-a"] != "linked" {
		t.Errorf("pair-a: %c %q", h.Typeflag, data["pair-a"])
	}
	if h := hdrs["pair-b"]; h.Typeflag != tar.TypeLink || h.Linkname != "pair-a" || data["pair-b"] != "" {
		t.Errorf("pair-b: %c %q %q", h.Typeflag, h.Linkname, data["pair-b"])
	}
	if data["dir/nested/deep/file.txt"] != "deep content" {
		t.Errorf("content %q", data["dir/nested/deep/file.txt"])
	}
	want := Stats{Files: 6, Dirs: 6, Symlinks: 3, Hardlinks: 1, Skipped: 2,
		Bytes: int64(len("deep content") + len("secret") + len("#!/bin/sh\n") + len("suid") + len("linked"))}
	if st != want {
		t.Errorf("stats %+v, want %+v", st, want)
	}
}

// node describes one entry of a tree the way a restore must reproduce it.
type node struct {
	mode     fs.FileMode
	uid, gid uint32
	mtime    time.Time
	link     string
	data     string
	linked   bool
}

func snapshot(t *testing.T, root string) map[string]node {
	t.Helper()
	out := map[string]node{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi := lstat(t, p)
		rel, _ := filepath.Rel(root, p)
		st := fi.Sys().(*syscall.Stat_t)
		n := node{mode: fi.Mode(), uid: st.Uid, gid: st.Gid, mtime: fi.ModTime()}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			n.link, _ = os.Readlink(p)
			n.mtime = time.Time{} // a symlink's own time is not kept
		case fi.Mode().IsRegular():
			b, _ := os.ReadFile(p)
			n.data, n.linked = string(b), st.Nlink > 1
		}
		out[rel] = n
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestExtractRecreatesWhatWriteArchived(t *testing.T) {
	src := makeTree(t)
	var buf bytes.Buffer
	wst, err := Write(&buf, src)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	est, err := Extract(bytes.NewReader(buf.Bytes()), dst)
	if err != nil {
		t.Fatal(err)
	}
	wantStats := wst
	wantStats.Skipped = 0
	if est != wantStats {
		t.Errorf("extract stats %+v, write stats %+v", est, wst)
	}
	a, b := snapshot(t, src), snapshot(t, dst)
	// The skipped files are the only ones that may be missing.
	delete(a, "fifo")
	delete(a, "sock")
	for k := range a {
		if !reflect.DeepEqual(a[k], b[k]) {
			t.Errorf("%s:\n src %+v\n dst %+v", k, a[k], b[k])
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			t.Errorf("%s only in the restore", k)
		}
	}
	if !os.SameFile(lstat(t, filepath.Join(dst, "pair-a")), lstat(t, filepath.Join(dst, "pair-b"))) {
		t.Error("the hard link pair was restored as two files")
	}
	if lstat(t, filepath.Join(dst, "dir/nested/deep/file.txt")).ModTime().Nanosecond() != fixtureTime.Nanosecond() {
		t.Error("mtime lost its fraction of a second")
	}
}

func TestExtractWillNotFollowASymlinkOutOfTheRoot(t *testing.T) {
	outside := t.TempDir()
	for name, target := range map[string]string{"absolute": outside, "slash": "/", "relative": "../" + filepath.Base(outside)} {
		data := buildArchive(t,
			entry{hdr: tar.Header{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777}},
			file("evil/passwd", "owned"))
		if _, err := Extract(bytes.NewReader(data), t.TempDir()); err == nil {
			t.Errorf("%s: write through the symlink accepted", name)
		}
	}
	if names := readNames(t, outside); len(names) != 0 {
		t.Fatalf("something was written outside the root: %v", names)
	}
	if _, err := os.Lstat("/passwd"); err == nil {
		t.Fatal("/passwd exists")
	}
	// A hard link may not be made through a symlink either, and neither may a directory.
	for _, last := range []entry{
		{hdr: tar.Header{Name: "evil/hard", Typeflag: tar.TypeLink, Linkname: "real", Mode: 0o644}},
		{hdr: tar.Header{Name: "evil/dir", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "evil/sym", Typeflag: tar.TypeSymlink, Linkname: "x", Mode: 0o777}},
	} {
		data := buildArchive(t,
			entry{hdr: tar.Header{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0o777}},
			file("real", "x"), last)
		if _, err := Extract(bytes.NewReader(data), t.TempDir()); err == nil {
			t.Errorf("%s accepted through a symlink", last.hdr.Name)
		}
	}
	if names := readNames(t, outside); len(names) != 0 {
		t.Fatalf("something was written outside the root: %v", names)
	}
}

// chownCall records one ownership change and the file's mode at that moment.
type chownCall struct {
	name     string
	uid, gid int
	mode     fs.FileMode
}

func TestExtractAppliesTheOwnerBeforeTheModeAndDirectoriesLast(t *testing.T) {
	data := buildArchive(t,
		entry{hdr: tar.Header{Name: "app/", Typeflag: tar.TypeDir, Mode: 0o2750, Uid: 70, Gid: 71}},
		entry{hdr: tar.Header{Name: "app/suid", Typeflag: tar.TypeReg, Mode: 0o4755, Uid: 1000, Gid: 1001}, data: "x"},
		entry{hdr: tar.Header{Name: "app/link", Typeflag: tar.TypeSymlink, Linkname: "suid", Mode: 0o777, Uid: 5, Gid: 6}},
		entry{hdr: tar.Header{Name: "app/hard", Typeflag: tar.TypeLink, Linkname: "app/suid", Mode: 0o4755, Uid: 1000, Gid: 1001}},
		entry{hdr: tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 9, Gid: 10}},
	)
	root := t.TempDir()
	var calls []chownCall
	chown := func(r *os.Root, name string, uid, gid int) error {
		fi, err := r.Lstat(name)
		if err != nil {
			t.Fatalf("chown %s: %v", name, err)
		}
		calls = append(calls, chownCall{name, uid, gid, fi.Mode()})
		return nil
	}
	if _, err := extract(bytes.NewReader(data), root, chown); err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(calls))
	for i, c := range calls {
		names[i] = c.name
	}
	// Content first (a hard link shares its target's owner), then directories deepest first, the root last.
	if want := []string{"app/suid", "app/link", "app", "."}; !reflect.DeepEqual(names, want) {
		t.Fatalf("ownership order %v, want %v", names, want)
	}
	for i, want := range []struct{ uid, gid int }{{1000, 1001}, {5, 6}, {70, 71}, {9, 10}} {
		if calls[i].uid != want.uid || calls[i].gid != want.gid {
			t.Errorf("%s owner %d:%d, want %d:%d", calls[i].name, calls[i].uid, calls[i].gid, want.uid, want.gid)
		}
	}
	// The kernel clears setuid on chown, so the mode must be set after it: at the chown the bit is not set yet.
	if calls[0].mode&fs.ModeSetuid != 0 {
		t.Errorf("mode %v was set before the owner", calls[0].mode)
	}
	if fi := lstat(t, filepath.Join(root, "app", "suid")); fi.Mode()&fs.ModeSetuid == 0 || fi.Mode().Perm() != 0o755 {
		t.Errorf("final mode %v", fi.Mode())
	}
	if fi := lstat(t, filepath.Join(root, "app")); fi.Mode()&fs.ModeSetgid == 0 || fi.Mode().Perm() != 0o750 {
		t.Errorf("directory mode %v", fi.Mode())
	}
}

func TestExtractFailsWhenTheOwnerCannotBeSet(t *testing.T) {
	data := buildArchive(t, file("a", "x"))
	failing := func(*os.Root, string, int, int) error { return os.ErrPermission }
	if _, err := extract(bytes.NewReader(data), t.TempDir(), failing); err == nil {
		t.Fatal("a restore that cannot keep ownership reported success")
	}
}
