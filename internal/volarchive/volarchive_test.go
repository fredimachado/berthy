package volarchive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
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
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
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
