package volarchive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// skipOnWindows skips a test that needs Unix modes, ownership or links.
func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs Unix modes, ownership and links")
	}
}

// entry is one member of a hand-built archive.
type entry struct {
	hdr  tar.Header
	data string
}

// buildArchive returns a gzip tar of the entries. Names, links and types are used as given: this is how a hostile archive
// is made.
func buildArchive(t *testing.T, entries ...entry) []byte {
	t.Helper()
	data, err := encodeArchive(entries...)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// headerError is the tar writer refusing a member's header, as opposed to a failure writing its data or closing.
type headerError struct{ err error }

func (e headerError) Error() string { return e.err.Error() }
func (e headerError) Unwrap() error { return e.err }

// encodeArchive is buildArchive that returns its error. It returns bytes only once both writers have closed.
func encodeArchive(entries ...entry) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := e.hdr
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.data))
		}
		if h.Uid == 0 && h.Gid == 0 {
			// A restore can only give files to an owner it may chown to: the test's own user, unless a test says otherwise.
			h.Uid, h.Gid = max(os.Getuid(), 0), max(os.Getgid(), 0)
		}
		if h.ModTime.IsZero() {
			h.ModTime = time.Unix(1_700_000_000, 0)
		}
		if err := tw.WriteHeader(&h); err != nil {
			return nil, headerError{err}
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.data)); err != nil {
				return nil, fmt.Errorf("write data of %q: %w", h.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("close gzip: %w", err)
	}
	return buf.Bytes(), nil
}

func file(name, data string) entry {
	return entry{hdr: tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644}, data: data}
}

// extractArchive extracts data into a new empty directory and returns the directory and the error.
func extractArchive(t *testing.T, data []byte) (string, error) {
	t.Helper()
	root := t.TempDir()
	_, err := Extract(bytes.NewReader(data), root)
	return root, err
}

func readNames(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func TestExtractRefusesNamesThatEscapeTheRoot(t *testing.T) {
	for _, name := range []string{"../x", "a/../../x", "/abs", "/etc/passwd", "a/../b", "..", ""} {
		outside := t.TempDir()
		root := filepath.Join(outside, "root")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := Extract(bytes.NewReader(buildArchive(t, file(name, "x"))), root)
		if err == nil {
			t.Errorf("name %q accepted", name)
		}
		if names := readNames(t, outside); len(names) != 1 || names[0] != "root" {
			t.Errorf("name %q wrote outside the root: %v", name, names)
		}
		if names := readNames(t, root); len(names) != 0 {
			t.Errorf("name %q left %v in the root", name, names)
		}
	}
}

func TestExtractRefusesDeviceAndFifoEntries(t *testing.T) {
	for name, typ := range map[string]byte{"char": tar.TypeChar, "block": tar.TypeBlock, "fifo": tar.TypeFifo} {
		data := buildArchive(t, entry{hdr: tar.Header{Name: name, Typeflag: typ, Mode: 0o600}})
		root, err := extractArchive(t, data)
		if err == nil {
			t.Errorf("%s entry accepted", name)
		}
		if names := readNames(t, root); len(names) != 0 {
			t.Errorf("%s entry left %v", name, names)
		}
	}
}

func TestExtractRefusesAHardLinkToAFileOutsideTheArchive(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	// "later" is in the archive, but only after the link: a link needs its target to be extracted already.
	for _, target := range []string{"missing", "../secret", filepath.Join(outside, "secret"), "later"} {
		data := buildArchive(t,
			entry{hdr: tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: target, Mode: 0o644}},
			file("later", "x"))
		if root, err := extractArchive(t, data); err == nil {
			t.Errorf("hard link to %q accepted (%v)", target, readNames(t, root))
		}
	}
}

func TestExtractNeedsAnEmptyExistingRoot(t *testing.T) {
	data := buildArchive(t, file("a", "x"))
	full := t.TempDir()
	if err := os.WriteFile(filepath.Join(full, "old"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(bytes.NewReader(data), full); err == nil {
		t.Fatal("non-empty root accepted")
	}
	if names := readNames(t, full); len(names) != 1 {
		t.Fatalf("a refused extract changed the root: %v", names)
	}
	if _, err := Extract(bytes.NewReader(data), filepath.Join(full, "missing")); err == nil {
		t.Fatal("missing root accepted")
	}
}

func TestExtractRefusesADuplicateFile(t *testing.T) {
	if _, err := extractArchive(t, buildArchive(t, file("a", "1"), file("a", "2"))); err == nil {
		t.Fatal("duplicate file accepted")
	}
}

func TestExtractDetectsACorruptOrTruncatedStream(t *testing.T) {
	big := strings.Repeat("0123456789abcdef", 64<<10) // 1 MiB
	good := buildArchive(t, file("a", big), file("b", "tail"))
	if _, err := extractArchive(t, good); err != nil {
		t.Fatal(err)
	}
	flipped := bytes.Clone(good)
	flipped[len(flipped)/2] ^= 0xff
	if _, err := extractArchive(t, flipped); err == nil {
		t.Error("flipped byte accepted")
	}
	// A flip in the gzip trailer: every entry extracts, only the checksum is wrong.
	trailer := bytes.Clone(good)
	trailer[len(trailer)-6] ^= 0xff
	if _, err := extractArchive(t, trailer); err == nil {
		t.Error("bad gzip checksum accepted")
	}
	if _, err := extractArchive(t, good[:len(good)/2]); err == nil {
		t.Error("truncated stream accepted")
	}
	if _, err := extractArchive(t, []byte("not gzip")); err == nil {
		t.Error("garbage accepted")
	}
}

func TestExtractReadsTheWholeInput(t *testing.T) {
	// The caller's checksum covers what it fed into the reader, so the reader is drained even past the archive's end:
	// here, a second gzip member that follows the first.
	var tail bytes.Buffer
	gz := gzip.NewWriter(&tail)
	gz.Write(bytes.Repeat([]byte("padding"), 5000))
	gz.Close()
	data := append(buildArchive(t, file("a", "x")), tail.Bytes()...)
	r := bytes.NewReader(data)
	if _, err := Extract(r, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if r.Len() != 0 {
		t.Fatalf("%d bytes left unread", r.Len())
	}
}

func TestExtractCreatesParentsAndAppliesDirectoryModesLast(t *testing.T) {
	// A read-only directory must still take its files, and its mode only counts once they are in.
	data := buildArchive(t,
		entry{hdr: tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o750}},
		entry{hdr: tar.Header{Name: "ro/", Typeflag: tar.TypeDir, Mode: 0o555, ModTime: time.Unix(1_600_000_000, 0)}},
		file("ro/inner/file", "x"),
		file("implicit/parent/file", "y"),
	)
	root, err := extractArchive(t, data)
	t.Cleanup(func() { os.Chmod(filepath.Join(root, "ro"), 0o755) })
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "ro", "inner", "file")); err != nil || string(b) != "x" {
		t.Fatalf("file in the read-only directory: %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "implicit", "parent", "file")); err != nil || string(b) != "y" {
		t.Fatalf("file with implicit parents: %q %v", b, err)
	}
	skipOnWindows(t)
	if fi, err := os.Stat(filepath.Join(root, "ro")); err != nil || fi.Mode().Perm() != 0o555 || !fi.ModTime().Equal(time.Unix(1_600_000_000, 0)) {
		t.Fatalf("directory ro: %v %v", fi, err)
	}
	if fi, err := os.Stat(root); err != nil || fi.Mode().Perm() != 0o750 {
		t.Fatalf("root: %v %v", fi, err)
	}
}

func TestCopyFixedKeepsTheHeaderSizeWhateverTheFileDoes(t *testing.T) {
	cases := map[string]struct {
		content string
		size    int64
		want    string
	}{
		"exact":  {"abcd", 4, "abcd"},
		"shrank": {"ab", 5, "ab\x00\x00\x00"},
		"grew":   {"abcdefgh", 3, "abc"},
		"empty":  {"", 2, "\x00\x00"},
	}
	for name, c := range cases {
		var out bytes.Buffer
		if err := copyFixed(&out, strings.NewReader(c.content), c.size); err != nil || out.String() != c.want {
			t.Errorf("%s: %q %v", name, out.String(), err)
		}
	}
	boom := errors.New("read failed")
	if err := copyFixed(io.Discard, io.MultiReader(strings.NewReader("a"), errReader{boom}), 5); !errors.Is(err, boom) {
		t.Errorf("read error: %v", err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// archiveFixture is the tree a fuzz run extracts into: an empty root and a sibling directory with a known file, side by side
// inside a parent that holds nothing else. Nothing an archive does may touch the sibling or add a name to the parent.
type archiveFixture struct {
	base, parent, root, sibling string
	modes                       map[string]fs.FileMode
	times                       map[string]time.Time
}

const siblingSentinel = "sentinel"

func newArchiveFixture(t *testing.T) *archiveFixture {
	t.Helper()
	x := &archiveFixture{base: t.TempDir()}
	x.parent = filepath.Join(x.base, "parent")
	x.root = filepath.Join(x.parent, "root")
	x.sibling = filepath.Join(x.parent, "sibling")
	for _, d := range []string{x.root, x.sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(x.sibling, "known"), []byte(siblingSentinel), 0o644); err != nil {
		t.Fatal(err)
	}
	x.modes, x.times = map[string]fs.FileMode{}, map[string]time.Time{}
	for _, p := range []string{x.sibling, filepath.Join(x.sibling, "known")} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		x.modes[p], x.times[p] = fi.Mode(), fi.ModTime()
	}
	return x
}

// checkContained fails when an extraction reached outside its root.
func (x *archiveFixture) checkContained(t *testing.T) {
	t.Helper()
	if names := readNames(t, x.base); len(names) != 1 || names[0] != "parent" {
		t.Errorf("names beside the parent: %v", names)
	}
	if names := readNames(t, x.parent); len(names) != 2 || names[0] != "root" || names[1] != "sibling" {
		t.Errorf("names in the parent: %v", names)
	}
	if names := readNames(t, x.sibling); len(names) != 1 || names[0] != "known" {
		t.Errorf("names in the sibling: %v", names)
	}
	if b, err := os.ReadFile(filepath.Join(x.sibling, "known")); err != nil || string(b) != siblingSentinel {
		t.Errorf("sibling file: %q %v", b, err)
	}
	for p, mode := range x.modes {
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode() != mode || !fi.ModTime().Equal(x.times[p]) {
			t.Errorf("metadata of %s changed: %v %v", filepath.Base(p), fi, err)
		}
	}
}

// disposableName is a short stand-in for one path component: fuzz input never names a real path.
func disposableName(component string) string {
	return fmt.Sprintf("c%03x", crc32.ChecksumIEEE([]byte(component))&0xfff)
}

// localName maps a fuzzed name to a relative path of at most four disposable components, with no dot elements.
func localName(name string) string {
	var parts []string
	for _, c := range strings.Split(name, "/") {
		if c != "" && c != "." && c != ".." && len(parts) < 4 {
			parts = append(parts, disposableName(c))
		}
	}
	if len(parts) == 0 {
		return "f"
	}
	return strings.Join(parts, "/")
}

// hostileName maps a fuzzed name to an archive member name that keeps what an escape needs: a leading slash (an absolute
// path into the sibling), up to two ".." elements, and the sibling's own names. Other components are disposable.
func hostileName(name, sibling string) (mapped string, escapes bool) {
	var parts []string
	dots := 0
	for _, c := range strings.Split(name, "/") {
		switch {
		case c == "..":
			if dots < 2 {
				parts = append(parts, c)
				dots++
			}
		case c == ".", c == "sibling", c == "known":
			parts = append(parts, c)
		case c != "":
			parts = append(parts, disposableName(c))
		}
		if len(parts) == 6 {
			break
		}
	}
	mapped = strings.Join(parts, "/")
	if strings.HasPrefix(name, "/") {
		return filepath.ToSlash(sibling) + "/" + mapped, true
	}
	return mapped, slices.Contains(parts, "..")
}

// checkCreationRoundTrip archives a source tree holding one file and restores it: the tree, the bytes and the counts must
// come back exactly.
func checkCreationRoundTrip(t *testing.T, name string, data []byte) {
	t.Helper()
	rel := localName(name)
	src := t.TempDir()
	p := filepath.Join(src, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	wst, err := Write(&buf, src)
	if err != nil {
		t.Fatalf("write %q: %v", rel, err)
	}
	dst := t.TempDir()
	est, err := Extract(&buf, dst)
	if err != nil {
		t.Fatalf("extract %q: %v", rel, err)
	}
	want := Stats{Files: 1, Dirs: strings.Count(rel, "/"), Bytes: int64(len(data))}
	if wst != want || est != want {
		t.Errorf("%q: write stats %+v, extract stats %+v, want %+v", rel, wst, est, want)
	}
	got := map[string][]byte{}
	err = filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		b, err := os.ReadFile(p)
		r, _ := filepath.Rel(dst, p)
		got[filepath.ToSlash(r)] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !bytes.Equal(got[rel], data) {
		t.Errorf("restored %d files, %q has %d bytes, want %d", len(got), rel, len(got[rel]), len(data))
	}
}

// The member kinds FuzzVolumeArchive builds, in the low three bits of its kind argument.
const (
	memberPlain = iota
	memberTraversal
	memberAbsolute
	memberThroughSymlink
	memberLinkUnderSymlink
	memberHardlink
	memberDirectory
	memberDamaged
)

func FuzzVolumeArchive(f *testing.F) {
	for _, s := range []struct {
		kind         uint8
		name, target string
		data         string
	}{
		{memberPlain, "dir/file", "", "hello"},
		{memberPlain, "empty", "", ""},
		{memberPlain, "a/b/c/d/e", "", "deep"},
		{memberTraversal, "../x", "", "x"},
		{memberTraversal, "../sibling/known", "", "owned"},
		{memberTraversal, "a/../../x", "", "x"},
		{memberTraversal, "..", "", "x"},
		{memberTraversal, "", "", "x"},
		{memberAbsolute, "/etc/passwd", "", "x"},
		{memberAbsolute, "/sibling/known", "", "owned"},
		{memberThroughSymlink, "evil", "", "owned"},
		{memberThroughSymlink | 1<<3, "evil", "", "owned"},
		{memberThroughSymlink | 2<<3, "evil", "", "owned"},
		{memberThroughSymlink | 3<<3, "evil", "../sibling", "owned"},
		{memberLinkUnderSymlink, "evil", "", "x"},
		{memberLinkUnderSymlink | 1<<3, "evil", "", "x"},
		{memberLinkUnderSymlink | 2<<3, "evil", "", "x"},
		{memberHardlink, "link", "../secret", ""},
		{memberHardlink, "link", "/sibling/known", ""},
		{memberDirectory, "../d", "", "x"},
		{memberDirectory, "/sibling", "", "x"},
		{memberDamaged, "a/b", "", "some content to cut and flip"},
	} {
		f.Add(s.kind, s.name, s.target, []byte(s.data))
	}
	f.Fuzz(func(t *testing.T, kind uint8, name, target string, data []byte) {
		if len(name) > 256 || len(target) > 256 || len(data) > 1<<16 {
			t.Skip()
		}
		checkCreationRoundTrip(t, name, data)

		x := newArchiveFixture(t)
		link := strings.SplitN(localName(name), "/", 2)[0]
		var entries []entry
		var mustFail bool
		switch kind & 7 {
		case memberPlain:
			entries = []entry{file(localName(name), string(data))}
		case memberTraversal:
			var n string
			n, mustFail = hostileName(name, x.sibling)
			entries = []entry{file(n, string(data))}
		case memberAbsolute:
			entries, mustFail = []entry{file(filepath.ToSlash(x.sibling)+"/"+localName(name), string(data))}, true
		case memberThroughSymlink:
			var to string
			switch v := kind >> 3 & 3; v {
			case 0:
				to, mustFail = x.sibling, true
			case 1:
				to, mustFail = "../sibling", true
			case 2:
				to, mustFail = "/", true
			default:
				to, _ = hostileName(target, x.sibling)
			}
			entries = []entry{
				{hdr: tar.Header{Name: link, Typeflag: tar.TypeSymlink, Linkname: to, Mode: 0o777}},
				file(link+"/x", string(data)),
			}
		case memberLinkUnderSymlink:
			last := [...]entry{
				{hdr: tar.Header{Name: link + "/hard", Typeflag: tar.TypeLink, Linkname: "real", Mode: 0o644}},
				{hdr: tar.Header{Name: link + "/dir", Typeflag: tar.TypeDir, Mode: 0o755}},
				{hdr: tar.Header{Name: link + "/sym", Typeflag: tar.TypeSymlink, Linkname: "x", Mode: 0o777}},
			}[kind>>3%3]
			entries, mustFail = []entry{
				{hdr: tar.Header{Name: link, Typeflag: tar.TypeSymlink, Linkname: x.sibling, Mode: 0o777}},
				file("real", string(data)), last,
			}, true
		case memberHardlink:
			to, _ := hostileName(target, x.sibling)
			entries, mustFail = []entry{
				file("real", string(data)),
				{hdr: tar.Header{Name: link, Typeflag: tar.TypeLink, Linkname: to, Mode: 0o644}},
			}, true
		case memberDirectory:
			var n string
			n, mustFail = hostileName(name, x.sibling)
			entries = []entry{
				{hdr: tar.Header{Name: n + "/", Typeflag: tar.TypeDir, Mode: 0o755}},
				file(n+"/inner", string(data)),
			}
		case memberDamaged:
			x.checkDamaged(t, name, data)
			x.checkContained(t)
			return
		}
		archive, err := encodeArchive(entries...)
		var refused headerError
		if errors.As(err, &refused) {
			return // the tar writer cannot represent this member, so there is nothing to extract
		}
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		_, err = Extract(bytes.NewReader(archive), x.root)
		if mustFail && err == nil {
			t.Errorf("kind %d: archive of %q accepted", kind, namesOf(entries))
		}
		if kind&7 == memberPlain {
			if err != nil {
				t.Fatalf("plain file %q: %v", entries[0].hdr.Name, err)
			}
			if b, err := os.ReadFile(filepath.Join(x.root, filepath.FromSlash(entries[0].hdr.Name))); err != nil || !bytes.Equal(b, data) {
				t.Errorf("plain file restored as %d bytes, %v", len(b), err)
			}
		}
		x.checkContained(t)
	})
}

// checkDamaged extracts a valid archive cut short, and the same archive with one byte flipped. The cut stream must be
// refused, whichever byte it ends at.
func (x *archiveFixture) checkDamaged(t *testing.T, name string, data []byte) {
	t.Helper()
	archive, err := encodeArchive(
		entry{hdr: tar.Header{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o755}},
		file("d/"+localName(name), string(data)),
		entry{hdr: tar.Header{Name: "ln", Typeflag: tar.TypeSymlink, Linkname: "d", Mode: 0o777}},
	)
	if err != nil {
		t.Fatal(err)
	}
	at := int(crc32.ChecksumIEEE(data) % uint32(len(archive)))
	if _, err := Extract(bytes.NewReader(archive[:at]), t.TempDir()); err == nil {
		t.Errorf("archive cut at %d of %d accepted", at, len(archive))
	}
	flipped := bytes.Clone(archive)
	flipped[at] ^= 0xff
	_, _ = Extract(bytes.NewReader(flipped), t.TempDir())
}

func namesOf(entries []entry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.hdr.Name)
	}
	return out
}
