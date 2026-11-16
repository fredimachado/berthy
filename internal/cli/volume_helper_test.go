package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/volarchive"
)

func readArchive(t *testing.T, data []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("the output is not gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out[h.Name] = string(b)
	}
}

func TestVolumeArchiveWritesOnlyTheArchiveToStdout(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := runVolumeArchive(src, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if got := readArchive(t, out.Bytes()); got["a.txt"] != "hello" {
		t.Fatalf("archive %v", got)
	}
	var st volarchive.Stats
	if err := json.Unmarshal(errout.Bytes(), &st); err != nil || st.Files != 1 || st.Bytes != 5 || strings.Count(errout.String(), "\n") != 1 {
		t.Fatalf("summary %q: %+v %v", errout.String(), st, err)
	}
	if strings.Contains(errout.String(), "a.txt") {
		t.Fatal("the summary names a file")
	}
}

func TestVolumeArchiveFailsWithOneFixedLine(t *testing.T) {
	var out, errout bytes.Buffer
	missing := filepath.Join(t.TempDir(), "secret-path-that-must-not-be-echoed")
	if code := runVolumeArchive(missing, &out, &errout); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if errout.String() != "volume archive failed\n" {
		t.Fatalf("stderr %q", errout.String())
	}
}

func TestVolumeExtractRestoresTheArchiveFromStdin(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if _, err := volarchive.Write(&archive, src); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	var errout bytes.Buffer
	if code := runVolumeExtract(bytes.NewReader(archive.Bytes()), dst, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if b, err := os.ReadFile(filepath.Join(dst, "a.txt")); err != nil || string(b) != "hello" {
		t.Fatalf("restored %q %v", b, err)
	}
	var st volarchive.Stats
	if err := json.Unmarshal(errout.Bytes(), &st); err != nil || st.Files != 1 {
		t.Fatalf("summary %q: %+v %v", errout.String(), st, err)
	}
}

func TestVolumeExtractFailsWithOneFixedLine(t *testing.T) {
	for name, in := range map[string]io.Reader{"garbage": strings.NewReader("not an archive"), "empty": strings.NewReader("")} {
		var errout bytes.Buffer
		if code := runVolumeExtract(in, t.TempDir(), &errout); code != 1 || errout.String() != "volume extract failed\n" {
			t.Errorf("%s: exit %d stderr %q", name, code, errout.String())
		}
	}
}

// The volume commands are hidden like probe-helper: not in the usage. Run reads the volume at /source and writes it to
// /target, which a test host does not have, so both refuse here.
func TestVolumeCommandsAreHiddenAndUseTheMountPoints(t *testing.T) {
	var out, errout bytes.Buffer
	if code := Run(nil, &out, &errout); code != 2 || strings.Contains(errout.String(), "volume-") {
		t.Fatalf("usage %d %q", code, errout.String())
	}
	if _, err := os.Stat(volarchive.SourceDir); err == nil {
		t.Skip("this host has a " + volarchive.SourceDir)
	}
	errout.Reset()
	if code := Run([]string{"volume-archive"}, &out, &errout); code != 1 || errout.String() != "volume archive failed\n" {
		t.Fatalf("volume-archive: %d %q", code, errout.String())
	}
}
