package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/engine"
)

// These tests drive a real Docker daemon. They run only when BERTHY_DOCKER_SOCKET names its socket, and they need the
// image alpine:3.20 locally: they pull nothing. Everything they create is labeled and removed when they end.

const (
	itestEnv   = "itest"
	itestImage = "alpine:3.20"
)

func itestEngine(t *testing.T) *engine.Engine {
	t.Helper()
	socket := os.Getenv("BERTHY_DOCKER_SOCKET")
	if socket == "" {
		t.Skip("BERTHY_DOCKER_SOCKET is not set")
	}
	return engine.New(socket)
}

func itestCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func randomName(prefix string) string {
	b := make([]byte, 4)
	rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

// spyEngine records what the archiver asks Docker to create, and inspects each container that it created, as Docker made
// it, just before it starts.
type spyEngine struct {
	*engine.Engine
	mu       sync.Mutex
	specs    []json.RawMessage
	inspects []engine.ContainerDetail
}

func (s *spyEngine) CreateContainer(ctx context.Context, name string, spec json.RawMessage) (string, error) {
	s.mu.Lock()
	s.specs = append(s.specs, spec)
	s.mu.Unlock()
	return s.Engine.CreateContainer(ctx, name, spec)
}

func (s *spyEngine) StartContainer(ctx context.Context, id string) error {
	d, err := s.Engine.InspectContainerDetail(ctx, id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.inspects = append(s.inspects, d)
	s.mu.Unlock()
	return s.Engine.StartContainer(ctx, id)
}

// buildHelperImage builds the agent as a static binary and imports it as a one-file image, as the agent does for its
// helper, and returns the image reference.
func buildHelperImage(t *testing.T, ctx context.Context, e *engine.Engine) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		goBin = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	bin := filepath.Join(t.TempDir(), "berthy")
	cmd := exec.Command(goBin, "build", "-o", bin, "berthy/cmd/berthy")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err = tw.WriteHeader(&tar.Header{Name: "berthy", Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	tw.Write(data)
	tw.Close()
	sum := sha256.Sum256(data)
	repo, tag := "berthy-itest-helper", hex.EncodeToString(sum[:6])
	if err = e.ImportImage(ctx, repo, tag, nil, buf.Bytes()); err != nil {
		t.Fatalf("import: %v", err)
	}
	ref := repo + ":" + tag
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		e.RemoveUnusedImage(c, ref)
	})
	return ref
}

// mount is a named volume mounted in a throwaway alpine container.
type mount struct {
	volume, target string
	readOnly       bool
}

// shell runs script in an alpine container as root, without network, and returns what it printed.
func shell(t *testing.T, ctx context.Context, e *engine.Engine, mounts []mount, script string) string {
	t.Helper()
	var ms []any
	for _, m := range mounts {
		ms = append(ms, map[string]any{"Type": "volume", "Source": m.volume, "Target": m.target, "ReadOnly": m.readOnly, "VolumeOptions": map[string]any{"NoCopy": true}})
	}
	spec, _ := json.Marshal(map[string]any{
		"Image": itestImage, "Cmd": []string{"sh", "-c", script}, "User": "0:0", "AttachStdout": true, "AttachStderr": true,
		"Labels":     map[string]string{"berthy.itest": "aux"},
		"HostConfig": map[string]any{"NetworkMode": "none", "Mounts": ms},
	})
	id, err := e.CreateContainer(ctx, randomName("berthy-itest-aux"), spec)
	if err != nil {
		t.Fatalf("create alpine: %v", err)
	}
	defer e.RemoveContainer(context.WithoutCancel(ctx), id)
	out, _, stderr, err := e.Attach(ctx, id, false)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if err = e.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := e.WaitContainer(ctx, id); err != nil || code != 0 {
		t.Fatalf("script exit %d %v: %s", code, err, stderr.String())
	}
	return string(b)
}

const fillScript = `set -e
cd /v
chown 70:70 . && chmod 0750 .
mkdir -p data/nested sgid emptydir
echo one > data/f1000; chown 1000:1000 data/f1000; chmod 0600 data/f1000
echo two > data/f0;    chmod 0640 data/f0
echo three > data/f70; chown 70:70 data/f70; chmod 4755 data/f70
echo sticky > data/fsticky; chown 1000:70 data/fsticky; chmod 0644 data/fsticky
chown 1000:1000 data/nested; chmod 0750 data/nested
echo deep > data/nested/deep.txt; chown 1000:1000 data/nested/deep.txt
chown 70:1000 sgid; chmod 2775 sgid
chmod 1777 emptydir
ln -s /etc/passwd abs-link
ln -s data/f0 rel-link
ln -s ../../nowhere dangling
ln data/f0 data/f0-hard
dd if=/dev/urandom of=big bs=1M count=50 2>/dev/null
chown 1000:1000 big
touch -d @1650000000 data/f0 data/f70 big
`

// listScript describes everything in /v the way a restore must reproduce it: names, owners, modes (with setuid, setgid and
// sticky), types, sizes, link counts, contents and modification times of everything but the links.
const listScript = `cd /v
find . -exec stat -c '%n %u %g %a %F %s %h' {} + | sort
find . -type f -exec sha256sum {} + | sort
find . ! -type l -exec stat -c '%n %Y' {} + | sort
find . -type l -exec sh -c 'for l; do echo "$l -> $(readlink "$l")"; done' sh {} + | sort
`

func listing(t *testing.T, ctx context.Context, e *engine.Engine, volume string) string {
	t.Helper()
	return shell(t, ctx, e, []mount{{volume, "/v", true}}, listScript)
}

// leftovers returns the containers that carry the label key=value.
func leftovers(t *testing.T, ctx context.Context, e *engine.Engine, label string) []string {
	t.Helper()
	var cs []struct{ ID string }
	filters, _ := json.Marshal(map[string][]string{"label": {label}})
	if err := e.Get(ctx, "/containers/json?all=1&filters="+url.QueryEscape(string(filters)), &cs); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range cs {
		ids = append(ids, c.ID)
	}
	return ids
}

func TestIntegrationArchiveAndRestoreAVolume(t *testing.T) {
	e := itestEngine(t)
	ctx := itestCtx(t)
	image := buildHelperImage(t, ctx, e)
	spy := &spyEngine{Engine: e}
	a := &Archiver{Engine: spy, Image: func(context.Context) (string, error) { return image, nil }, Env: itestEnv, Log: func(s string) { t.Log("helper log:", s) }}

	var made []string
	newVolume := func(prefix string) string {
		name := randomName(prefix)
		made = append(made, name)
		return name
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, label := range []string{"berthy.environment=" + itestEnv, "berthy.itest=aux"} {
			for _, id := range leftovers(t, c, e, label) {
				e.RemoveContainer(c, id)
			}
		}
		for _, v := range made {
			e.RemoveVolume(c, v)
		}
	})

	// A source volume with what a real one holds, made as root with alpine.
	src := newVolume("berthy-itest-src")
	if err := e.CreateVolume(ctx, src, map[string]string{"berthy.itest": "1"}); err != nil {
		t.Fatal(err)
	}
	shell(t, ctx, e, []mount{{src, "/v", false}}, fillScript)
	want := listing(t, ctx, e, src)
	for _, must := range []string{"./data/f1000 1000 1000 600 regular file", "./data/f70 70 70 4755 regular file", "./sgid 70 1000 2775 directory",
		"./emptydir 0 0 1777 directory", ". 70 70 750 directory", "./abs-link 0 0 777 symbolic link", "./big 1000 1000 644 regular file 52428800"} {
		if !strings.Contains(want, must) {
			t.Fatalf("the fixture lacks %q:\n%s", must, want)
		}
	}

	archivePath := filepath.Join(t.TempDir(), "volume.tar.gz")
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	n, err := a.Archive(ctx, src, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if st, _ := os.Stat(archivePath); st.Size() != n || n < 50<<20 {
		t.Fatalf("archive is %d bytes, Archive says %d", st.Size(), n)
	}
	if ids := leftovers(t, ctx, e, "berthy.helper=archive"); len(ids) != 0 {
		t.Fatalf("archive container left behind: %v", ids)
	}

	// The archive container was hardened, as Docker made it.
	if len(spy.inspects) != 1 {
		t.Fatalf("%d containers inspected", len(spy.inspects))
	}
	d := spy.inspects[0]
	if !d.ReadonlyRootfs || !contains(d.CapDrop, "ALL") || !contains(d.SecurityOpt, "no-new-privileges") || d.Memory != 256<<20 || d.User != "0:0" {
		t.Errorf("archive container settings: %+v", d)
	}
	if _, none := d.Networks["none"]; len(d.Networks) != 1 || !none {
		t.Errorf("archive container networks: %v", d.Networks)
	}
	var spec struct {
		HostConfig struct {
			CapAdd []string
			Mounts []struct {
				Source, Target string
				ReadOnly       bool
			}
		}
	}
	if err = json.Unmarshal(spy.specs[0], &spec); err != nil || len(spec.HostConfig.Mounts) != 1 || !spec.HostConfig.Mounts[0].ReadOnly ||
		spec.HostConfig.Mounts[0].Source != src || !contains(spec.HostConfig.CapAdd, "DAC_READ_SEARCH") {
		t.Errorf("archive spec: %s", spy.specs[0])
	}

	// The archive is a gzip tar with the root and the files in it.
	names := tarNames(t, archivePath)
	for _, must := range []string{"./", "data/f70", "big", "data/f0-hard", "abs-link"} {
		if !contains(names, must) {
			t.Errorf("archive lacks %q: %v", must, names)
		}
	}

	archiveSum := fileSum(t, archivePath)
	restoreFrom := func(volume, path string, verify func(sum string) error) error {
		in, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer in.Close()
		h := sha256.New()
		return a.Restore(ctx, volume, io.TeeReader(in, h), map[string]string{"berthy.itest": "restored"}, func() error {
			return verify(hex.EncodeToString(h.Sum(nil)))
		})
	}

	dst := newVolume("berthy-itest-dst")
	if err = restoreFrom(dst, archivePath, func(sum string) error {
		if sum != archiveSum {
			return errors.New("checksum differs")
		}
		return nil
	}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got := listing(t, ctx, e, dst)
	if got != want {
		t.Fatalf("the restored volume differs\n--- source\n%s\n--- restored\n%s", want, got)
	}
	var vol struct{ Labels map[string]string }
	if err = e.Get(ctx, "/volumes/"+dst, &vol); err != nil || vol.Labels["berthy.itest"] != "restored" {
		t.Errorf("restored volume labels %v %v", vol.Labels, err)
	}
	if ids := leftovers(t, ctx, e, "berthy.helper=restore"); len(ids) != 0 {
		t.Fatalf("restore container left behind: %v", ids)
	}
	restoreSpec := spy.specs[1]
	var rspec struct {
		HostConfig struct{ CapAdd []string }
		StdinOnce  bool
	}
	json.Unmarshal(restoreSpec, &rspec)
	rd := spy.inspects[1]
	if !rd.ReadonlyRootfs || !contains(rd.CapDrop, "ALL") || !rspec.StdinOnce || len(rd.Networks) != 1 || !hasKey(rd.Networks, "none") || !contains(rspec.HostConfig.CapAdd, "CHOWN") {
		t.Errorf("restore container: %+v %s", rd, restoreSpec)
	}

	t.Run("an existing volume is refused and left alone", func(t *testing.T) {
		err := restoreFrom(dst, archivePath, func(string) error { return nil })
		if err == nil || err.Error() != "volume "+dst+" already exists; choose an unused name" {
			t.Fatalf("error %v", err)
		}
		if again := listing(t, ctx, e, dst); again != want {
			t.Fatal("the existing volume changed")
		}
	})

	t.Run("a corrupted archive fails and leaves no volume", func(t *testing.T) {
		data, err := os.ReadFile(archivePath)
		if err != nil {
			t.Fatal(err)
		}
		data[len(data)/2] ^= 0xff
		bad := filepath.Join(t.TempDir(), "bad.tar.gz")
		if err = os.WriteFile(bad, data, 0o600); err != nil {
			t.Fatal(err)
		}
		fresh := newVolume("berthy-itest-bad")
		err = restoreFrom(fresh, bad, func(string) error { return nil })
		if err == nil {
			t.Fatal("a corrupted archive was restored")
		}
		t.Logf("corrupted archive: %v", err)
		if ok, err := e.VolumeExists(ctx, fresh); err != nil || ok {
			t.Fatalf("the volume of a failed restore exists: %v %v", ok, err)
		}
		if ids := leftovers(t, ctx, e, "berthy.helper=restore"); len(ids) != 0 {
			t.Fatalf("restore container left behind: %v", ids)
		}
	})

	t.Run("a truncated archive fails and leaves no volume", func(t *testing.T) {
		data, _ := os.ReadFile(archivePath)
		short := filepath.Join(t.TempDir(), "short.tar.gz")
		if err := os.WriteFile(short, data[:len(data)-1000], 0o600); err != nil {
			t.Fatal(err)
		}
		fresh := newVolume("berthy-itest-short")
		if err := restoreFrom(fresh, short, func(string) error { return nil }); err == nil {
			t.Fatal("a truncated archive was restored")
		}
		if ok, _ := e.VolumeExists(ctx, fresh); ok {
			t.Fatal("the volume of a failed restore exists")
		}
	})

	t.Run("a checksum mismatch removes the volume", func(t *testing.T) {
		fresh := newVolume("berthy-itest-sum")
		err := restoreFrom(fresh, archivePath, func(string) error { return errors.New("checksum differs") })
		if err == nil || !strings.Contains(err.Error(), "checksum differs") {
			t.Fatalf("error %v", err)
		}
		if ok, _ := e.VolumeExists(ctx, fresh); ok {
			t.Fatal("the volume of a failed restore exists")
		}
		if ids := leftovers(t, ctx, e, "berthy.environment="+itestEnv); len(ids) != 0 {
			t.Fatalf("containers left behind: %v", ids)
		}
	})

	t.Run("a missing volume is not archived into existence", func(t *testing.T) {
		ghost := randomName("berthy-itest-ghost")
		if _, err := a.Archive(ctx, ghost, io.Discard); err == nil {
			t.Fatal("archive of a missing volume succeeded")
		}
		if ok, _ := e.VolumeExists(ctx, ghost); ok {
			e.RemoveVolume(ctx, ghost)
			t.Fatal("archiving created the volume")
		}
	})

	t.Run("a failing destination stops the archive and removes the container", func(t *testing.T) {
		_, err := a.Archive(ctx, src, failingWriter{errors.New("destination full")})
		if err == nil {
			t.Fatal("no error")
		}
		if ids := leftovers(t, ctx, e, "berthy.helper=archive"); len(ids) != 0 {
			t.Fatalf("archive container left behind: %v", ids)
		}
	})
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func fileSum(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func tarNames(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
}

func hasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}
