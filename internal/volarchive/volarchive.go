// Package volarchive is the archive format of a volume backup: a gzip-compressed PAX tar of the files under one directory.
//
// Write reads a tree and Extract rebuilds one. The format keeps what a database or an app needs from its data directory:
// file contents, modes (including setuid, setgid and sticky), numeric owner and group, modification times, symbolic links
// as text and hard links as links. Names are relative to the root, and the root itself is the entry "./".
//
// Files are archived as they are encountered: the walk is not a snapshot. A file that changes size while it is read keeps
// the size its header was written with, padded with zeros or cut short.
package volarchive

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Stats counts what Write archived or Extract restored.
type Stats struct {
	Files     int   `json:"files"`
	Dirs      int   `json:"dirs"` // the root is not counted
	Symlinks  int   `json:"symlinks"`
	Hardlinks int   `json:"hardlinks"`
	Skipped   int   `json:"skipped"` // sockets, devices, FIFOs and files that vanished while the tree was read
	Bytes     int64 `json:"bytes"`   // content bytes of regular files
}

// rootName is the entry of the root directory itself.
const rootName = "."

// Write streams a gzip-compressed PAX tar of the tree under root to w. See the package comment for what it keeps. Sockets,
// devices and FIFOs are skipped and counted. Entries come in lexical order, a directory before what it holds.
func Write(w io.Writer, root string) (Stats, error) {
	var st Stats
	r, err := os.OpenRoot(root)
	if err != nil {
		return st, err
	}
	defer r.Close()
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	wr := &writer{tw: tw, root: r, st: &st, firstLink: map[inodeKey]string{}}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return wr.gone(walkErr)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return wr.gone(err)
		}
		return wr.entry(path.Clean(filepath.ToSlash(rel)), p, info)
	})
	if err != nil {
		return st, err
	}
	if err = tw.Close(); err != nil {
		return st, err
	}
	return st, gz.Close()
}

type writer struct {
	tw   *tar.Writer
	root *os.Root
	st   *Stats
	// firstLink maps an inode with more than one name to the first name that was archived.
	firstLink map[inodeKey]string
}

// gone absorbs the error of an entry that vanished while the tree was walked, and counts it as skipped.
func (w *writer) gone(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		w.st.Skipped++
		return nil
	}
	return err
}

func (w *writer) entry(name, abs string, info fs.FileInfo) error {
	mode := info.Mode()
	switch {
	case mode.IsDir():
		h, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		h.Name = name + "/"
		if name == rootName {
			h.Name = "./"
		} else {
			w.st.Dirs++
		}
		return w.header(h)
	case mode&fs.ModeSymlink != 0:
		target, err := os.Readlink(abs)
		if err != nil {
			return w.gone(err)
		}
		h, err := tar.FileInfoHeader(info, target)
		if err != nil {
			return err
		}
		h.Name = name
		w.st.Symlinks++
		return w.header(h)
	case mode.IsRegular():
		return w.regular(name, info)
	}
	w.st.Skipped++
	return nil
}

func (w *writer) regular(name string, info fs.FileInfo) error {
	key, multi := linkKey(info)
	if first, ok := w.firstLink[key]; multi && ok {
		h, err := tar.FileInfoHeader(info, first)
		if err != nil {
			return err
		}
		h.Name, h.Typeflag, h.Linkname, h.Size = name, tar.TypeLink, first, 0
		w.st.Hardlinks++
		return w.header(h)
	}
	f, err := w.root.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return w.gone(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || !os.SameFile(info, fi) {
		// Replaced by something else between the walk and the open.
		w.st.Skipped++
		return nil
	}
	h, err := tar.FileInfoHeader(fi, "")
	if err != nil {
		return err
	}
	h.Name = name
	if err = w.header(h); err != nil {
		return err
	}
	if err = copyFixed(w.tw, f, h.Size); err != nil {
		return err
	}
	if multi {
		w.firstLink[key] = name
	}
	w.st.Files++
	w.st.Bytes += h.Size
	return nil
}

// header writes h in PAX format with numeric owners only: names of users and groups mean nothing on another host.
func (w *writer) header(h *tar.Header) error {
	h.Format = tar.FormatPAX
	h.Uname, h.Gname = "", ""
	h.AccessTime, h.ChangeTime = time.Time{}, time.Time{}
	return w.tw.WriteHeader(h)
}

// copyFixed writes exactly size bytes of r to w. A file that shrank is padded with zeros, one that grew is cut at size,
// because a tar header fixes the size before the content is written.
func copyFixed(w io.Writer, r io.Reader, size int64) error {
	n, err := io.Copy(w, io.LimitReader(r, size))
	if err != nil {
		return err
	}
	if n < size {
		_, err = io.CopyN(w, zeros{}, size-n)
	}
	return err
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// chownFunc changes the owner of name without following a link. It is a seam: Extract uses the real call.
type chownFunc func(root *os.Root, name string, uid, gid int) error

// Extract reads a gzip tar from r into root, which must exist and be empty. Every file operation goes through an
// os.Root, so no entry can reach outside root, not even through a symbolic link made earlier in the same archive. Names
// that are absolute or hold "..", device, FIFO and other special entries, and hard links to a file the archive did not
// extract before are refused. Owner, mode and times come from the headers: the owner first and the mode after it, so
// setuid and setgid survive, and directories last, deepest first, so a read-only directory still takes its files.
//
// Extract reads r to its end, so a checksum over what was fed into r covers the whole stream.
func Extract(r io.Reader, root string) (Stats, error) { return extract(r, root, chownLink) }

func extract(in io.Reader, rootPath string, chown chownFunc) (Stats, error) {
	var st Stats
	existing, err := os.ReadDir(rootPath)
	if err != nil {
		return st, err
	}
	if len(existing) != 0 {
		return st, errors.New("target is not empty")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return st, err
	}
	defer root.Close()
	gz, err := gzip.NewReader(in)
	if err != nil {
		return st, err
	}
	x := &extractor{root: root, chown: chown, st: &st, regular: map[string]bool{}}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return st, err
		}
		if err = x.entry(h, tr); err != nil {
			return st, fmt.Errorf("%s: %w", h.Name, err)
		}
	}
	// The gzip checksum is only checked at the end of the stream, and the caller's checksum needs all of it.
	if _, err = io.Copy(io.Discard, gz); err != nil {
		return st, err
	}
	if _, err = io.Copy(io.Discard, in); err != nil {
		return st, err
	}
	return st, x.finishDirs()
}

// dirMeta is what a directory takes once its content is in.
type dirMeta struct {
	name     string
	uid, gid int
	mode     fs.FileMode
	mtime    time.Time
}

type extractor struct {
	root    *os.Root
	chown   chownFunc
	st      *Stats
	regular map[string]bool // regular files extracted so far: the only valid hard link targets
	dirs    []dirMeta
	rootDir *dirMeta // the root's own entry, applied after every directory
}

// cleanName returns the slash-separated name inside the root that an entry name stands for, or an error when the name
// could leave it. The root itself is ".".
func cleanName(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) || strings.HasPrefix(name, "/") {
		return "", errors.New("unsafe name")
	}
	for _, el := range strings.Split(name, "/") {
		if el == ".." {
			return "", errors.New("unsafe name")
		}
	}
	c := path.Clean(name)
	if !filepath.IsLocal(filepath.FromSlash(c)) {
		return "", errors.New("unsafe name")
	}
	return c, nil
}

func (x *extractor) entry(h *tar.Header, content io.Reader) error {
	if h.Typeflag == tar.TypeXGlobalHeader {
		return nil
	}
	name, err := cleanName(h.Name)
	if err != nil {
		return err
	}
	switch h.Typeflag {
	case tar.TypeDir:
		return x.dir(name, h)
	case tar.TypeReg:
		if name == rootName {
			return errors.New("unsafe name")
		}
		return x.file(name, h, content)
	case tar.TypeSymlink:
		return x.symlink(name, h)
	case tar.TypeLink:
		return x.hardlink(name, h)
	}
	return fmt.Errorf("unsupported entry type %q", h.Typeflag)
}

func (x *extractor) parent(name string) error {
	if dir := path.Dir(name); dir != rootName {
		return x.root.MkdirAll(dir, 0o755)
	}
	return nil
}

func (x *extractor) dir(name string, h *tar.Header) error {
	if name != rootName {
		if err := x.root.MkdirAll(name, 0o755); err != nil {
			return err
		}
		x.st.Dirs++
	}
	d := dirMeta{name: name, uid: h.Uid, gid: h.Gid, mode: permBits(h), mtime: h.ModTime}
	if name == rootName {
		x.rootDir = &d
	} else {
		x.dirs = append(x.dirs, d)
	}
	return nil
}

func (x *extractor) file(name string, h *tar.Header, content io.Reader) error {
	if err := x.parent(name); err != nil {
		return err
	}
	f, err := x.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != h.Size {
		return errors.New("short file")
	}
	if err = x.meta(name, h); err != nil {
		return err
	}
	x.regular[name] = true
	x.st.Files++
	x.st.Bytes += n
	return nil
}

func (x *extractor) symlink(name string, h *tar.Header) error {
	if h.Linkname == "" || strings.ContainsRune(h.Linkname, 0) {
		return errors.New("invalid link target")
	}
	if err := x.parent(name); err != nil {
		return err
	}
	if err := x.root.Symlink(h.Linkname, name); err != nil {
		return err
	}
	x.st.Symlinks++
	return x.chown(x.root, name, h.Uid, h.Gid)
}

func (x *extractor) hardlink(name string, h *tar.Header) error {
	target, err := cleanName(h.Linkname)
	if err != nil {
		return err
	}
	if !x.regular[target] {
		return errors.New("hard link target is not a file of the archive")
	}
	if err = x.parent(name); err != nil {
		return err
	}
	if err = x.root.Link(target, name); err != nil {
		return err
	}
	x.st.Hardlinks++
	return nil
}

// meta applies owner, mode and times of a file: the owner first, because a chown clears setuid and setgid.
func (x *extractor) meta(name string, h *tar.Header) error {
	if err := x.chown(x.root, name, h.Uid, h.Gid); err != nil {
		return err
	}
	if err := x.root.Chmod(name, permBits(h)); err != nil {
		return err
	}
	return x.root.Chtimes(name, h.ModTime, h.ModTime)
}

// finishDirs gives the directories their owner, mode and times, the deepest first and the root last.
func (x *extractor) finishDirs() error {
	dirs := x.dirs
	if x.rootDir != nil {
		dirs = append([]dirMeta{*x.rootDir}, dirs...)
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		if err := x.chown(x.root, d.name, d.uid, d.gid); err != nil {
			return err
		}
		if err := x.root.Chmod(d.name, d.mode); err != nil {
			return err
		}
		if err := x.root.Chtimes(d.name, d.mtime, d.mtime); err != nil {
			return err
		}
	}
	return nil
}

// permBits are the permission, setuid, setgid and sticky bits of a header.
func permBits(h *tar.Header) fs.FileMode {
	return h.FileInfo().Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}

// The mount points a helper container sees: the volume to archive read-only, or the new volume to fill.
const (
	SourceDir = "/source"
	TargetDir = "/target"
)
