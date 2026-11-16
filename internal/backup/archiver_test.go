package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"berthy/internal/engine"
)

// fakeContainer is a container of the fake Docker.
type fakeContainer struct {
	name    string
	spec    json.RawMessage
	started bool
	removed bool
}

// fakeDocker is an in-memory Docker for the archiver. It records every call in order, keeps containers and volumes, and
// behaves as Docker does where the archiver depends on it: a stream needs a deadline, a volume in use cannot be removed,
// and a container can be removed while it runs.
type fakeDocker struct {
	mu         sync.Mutex
	calls      []string
	containers map[string]*fakeContainer
	volumes    map[string]map[string]string // name to labels
	nextID     int

	// What the helper container does.
	stdout     io.Reader // what an archive container writes
	stderr     string
	exit       int
	stdinSeen  bytes.Buffer
	stdinClose bool

	// Failures to inject.
	createErr, attachErr, startErr, waitErr, createVolumeErr, removeContainerErr error
	// inventory is what InventoryContainers lists, and inventoryErr fails it.
	inventory    []engine.InventoryContainer
	inventoryErr error
	// onWait runs when the container is waited for, with the context Wait got.
	onWait func(ctx context.Context)
	// ctxErrOnRemove records the state of the context each removal got.
	removeCtxDone []bool
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{containers: map[string]*fakeContainer{}, volumes: map[string]map[string]string{}, stdout: strings.NewReader("")}
}

func (f *fakeDocker) call(format string, args ...any) {
	f.mu.Lock()
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
	f.mu.Unlock()
}

func (f *fakeDocker) CreateContainer(_ context.Context, name string, spec json.RawMessage) (string, error) {
	f.call("CreateContainer")
	if f.createErr != nil {
		return "", f.createErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("c%d", f.nextID)
	f.containers[id] = &fakeContainer{name: name, spec: spec}
	return id, nil
}

func (f *fakeDocker) Attach(_ context.Context, id string, stdin bool) (io.ReadCloser, io.WriteCloser, *engine.BoundedBuffer, error) {
	f.call("Attach stdin=%v", stdin)
	if f.attachErr != nil {
		return nil, nil, nil, f.attachErr
	}
	stderr := engine.NewBoundedBuffer(4096)
	stderr.Write([]byte(f.stderr))
	var in io.WriteCloser
	if stdin {
		in = &fakeStdin{f: f}
	}
	return io.NopCloser(f.stdout), in, stderr, nil
}

type fakeStdin struct{ f *fakeDocker }

func (s *fakeStdin) Write(p []byte) (int, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	return s.f.stdinSeen.Write(p)
}

func (s *fakeStdin) Close() error {
	s.f.call("CloseStdin")
	s.f.mu.Lock()
	s.f.stdinClose = true
	s.f.mu.Unlock()
	return nil
}

func (f *fakeDocker) StartContainer(_ context.Context, id string) error {
	f.call("StartContainer")
	if f.startErr != nil {
		return f.startErr
	}
	f.containers[id].started = true
	return nil
}

func (f *fakeDocker) WaitContainer(ctx context.Context, id string) (int, error) {
	f.call("WaitContainer")
	if _, ok := ctx.Deadline(); !ok {
		return 0, errors.New("Docker stream requires a deadline")
	}
	if f.onWait != nil {
		f.onWait(ctx)
	}
	if f.waitErr != nil {
		return 0, f.waitErr
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return f.exit, nil
}

func (f *fakeDocker) RemoveContainer(ctx context.Context, id string) error {
	f.call("RemoveContainer")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeCtxDone = append(f.removeCtxDone, ctx.Err() != nil)
	if f.removeContainerErr != nil {
		return f.removeContainerErr
	}
	c, ok := f.containers[id]
	if !ok || c.removed {
		return &engine.StatusError{Code: http.StatusNotFound}
	}
	c.removed = true
	return nil
}

func (f *fakeDocker) VolumeExists(_ context.Context, name string) (bool, error) {
	f.call("VolumeExists %s", name)
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.volumes[name]
	return ok, nil
}

func (f *fakeDocker) CreateVolume(_ context.Context, name string, labels map[string]string) error {
	f.call("CreateVolume %s", name)
	if f.createVolumeErr != nil {
		return f.createVolumeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.volumes[name]; ok {
		return &engine.StatusError{Code: http.StatusConflict}
	}
	f.volumes[name] = labels
	return nil
}

// RemoveVolume refuses a volume that a container which still exists mounts, as Docker does.
func (f *fakeDocker) RemoveVolume(_ context.Context, name string) error {
	f.call("RemoveVolume %s", name)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.containers {
		if !c.removed && strings.Contains(string(c.spec), `"Source":"`+name+`"`) {
			return &engine.StatusError{Code: http.StatusConflict}
		}
	}
	if _, ok := f.volumes[name]; !ok {
		return &engine.StatusError{Code: http.StatusNotFound}
	}
	delete(f.volumes, name)
	return nil
}

func (f *fakeDocker) InventoryContainers(context.Context) ([]engine.InventoryContainer, error) {
	f.call("InventoryContainers")
	return f.inventory, f.inventoryErr
}

func (f *fakeDocker) callNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = strings.Fields(c)[0]
	}
	return out
}

func (f *fakeDocker) count(name string) int {
	n := 0
	for _, c := range f.callNames() {
		if c == name {
			n++
		}
	}
	return n
}

// onlyContainer returns the one container that was created.
func (f *fakeDocker) onlyContainer(t *testing.T) *fakeContainer {
	t.Helper()
	if len(f.containers) != 1 {
		t.Fatalf("%d containers created, want 1: %v", len(f.containers), f.calls)
	}
	for _, c := range f.containers {
		return c
	}
	return nil
}

// assertNoContainerLeft fails when a container was created and not removed.
func (f *fakeDocker) assertNoContainerLeft(t *testing.T) {
	t.Helper()
	for id, c := range f.containers {
		if !c.removed {
			t.Errorf("container %s (%s) was left behind; calls %v", id, c.name, f.calls)
		}
	}
}

func newArchiver(f *fakeDocker) (*Archiver, *[]string) {
	var logged []string
	return &Archiver{
		Engine: f,
		Image:  func(context.Context) (string, error) { return "berthy-helper:0123456789ab", nil },
		Env:    "prod",
		Log:    func(s string) { logged = append(logged, s) },
	}, &logged
}

func spec(t *testing.T, c *fakeContainer) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.spec, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// jsonOf turns a literal into what json.Unmarshal makes of its encoding, so it compares with a decoded spec.
func jsonOf(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err = json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestArchiveStreamsTheVolumeThroughAHardenedContainer(t *testing.T) {
	f := newFakeDocker()
	f.volumes["data"] = nil
	f.stdout = strings.NewReader("the-archive-bytes")
	f.stderr = `{"files":1}`
	a, _ := newArchiver(f)
	var out bytes.Buffer
	n, err := a.Archive(context.Background(), "data", &out)
	if err != nil || n != int64(len("the-archive-bytes")) || out.String() != "the-archive-bytes" {
		t.Fatalf("archive: %d %v %q", n, err, out.String())
	}
	want := []string{"VolumeExists", "CreateContainer", "Attach", "StartContainer", "WaitContainer", "RemoveContainer"}
	if got := f.callNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v, want %v", got, want)
	}
	if f.calls[2] != "Attach stdin=false" {
		t.Errorf("attach %q", f.calls[2])
	}
	c := f.onlyContainer(t)
	if !regexp.MustCompile(`^berthy-archive-prod-[0-9a-f]{8}$`).MatchString(c.name) || !c.started || !c.removed {
		t.Errorf("container %+v", c)
	}
	want2 := jsonOf(t, map[string]any{
		"Image":        "berthy-helper:0123456789ab",
		"Entrypoint":   []string{"/berthy", "volume-archive"},
		"User":         "0:0",
		"AttachStdout": true,
		"AttachStderr": true,
		"Labels":       map[string]string{"berthy.environment": "prod", "berthy.helper": "archive"},
		"HostConfig": map[string]any{
			"NetworkMode":    "none",
			"ReadonlyRootfs": true,
			"CapDrop":        []string{"ALL"},
			"CapAdd":         []string{"DAC_READ_SEARCH"},
			"SecurityOpt":    []string{"no-new-privileges"},
			"Memory":         256 << 20,
			"Mounts": []map[string]any{{
				"Type": "volume", "Source": "data", "Target": "/source", "ReadOnly": true,
				"VolumeOptions": map[string]any{"NoCopy": true},
			}},
		},
	})
	if got := spec(t, c); !reflect.DeepEqual(got, want2) {
		t.Fatalf("spec\n got %v\nwant %v", got, want2)
	}
}

func TestArchiveOfAMissingVolumeCreatesNothing(t *testing.T) {
	// Docker would create a volume that a container mounts and does not find, and the backup would be of nothing.
	f := newFakeDocker()
	a, _ := newArchiver(f)
	_, err := a.Archive(context.Background(), "ghost", io.Discard)
	if err == nil || err.Error() != "volume ghost not found" {
		t.Fatalf("error %v", err)
	}
	if f.count("CreateContainer") != 0 || len(f.volumes) != 0 {
		t.Fatalf("calls %v, volumes %v", f.calls, f.volumes)
	}
}

func TestArchiveRemovesTheContainerOnEveryFailure(t *testing.T) {
	boom := errors.New("boom")
	cases := map[string]func(f *fakeDocker, a *Archiver){
		"helper exits non-zero": func(f *fakeDocker, _ *Archiver) { f.exit = 3; f.stderr = "volume archive failed" },
		"attach fails":          func(f *fakeDocker, _ *Archiver) { f.attachErr = boom },
		"start fails":           func(f *fakeDocker, _ *Archiver) { f.startErr = boom },
		"wait fails":            func(f *fakeDocker, _ *Archiver) { f.waitErr = boom },
		"stream breaks": func(f *fakeDocker, _ *Archiver) {
			f.stdout = io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(boom))
		},
	}
	for name, setup := range cases {
		f := newFakeDocker()
		f.volumes["data"] = nil
		a, _ := newArchiver(f)
		setup(f, a)
		if _, err := a.Archive(context.Background(), "data", io.Discard); err == nil {
			t.Errorf("%s: no error", name)
		}
		if f.count("CreateContainer") != 1 {
			t.Errorf("%s: calls %v", name, f.calls)
		}
		f.assertNoContainerLeft(t)
	}
}

func TestArchiveStopsWhenTheDestinationFails(t *testing.T) {
	f := newFakeDocker()
	f.volumes["data"] = nil
	f.stdout = strings.NewReader("data that cannot be stored")
	a, _ := newArchiver(f)
	boom := errors.New("upload failed")
	n, err := a.Archive(context.Background(), "data", failingWriter{boom})
	if !errors.Is(err, boom) {
		t.Fatalf("error %v", err)
	}
	if n != 0 {
		t.Errorf("%d bytes counted for a destination that took none", n)
	}
	// The helper may still be running: the container is removed anyway, and the helper is not waited for.
	f.assertNoContainerLeft(t)
	if f.count("WaitContainer") != 0 {
		t.Errorf("waited for a helper whose output nobody takes: %v", f.calls)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestArchiveCancelledStillRemovesTheContainer(t *testing.T) {
	f := newFakeDocker()
	f.volumes["data"] = nil
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.onWait = func(context.Context) { cancel() }
	a, _ := newArchiver(f)
	_, err := a.Archive(ctx, "data", io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v", err)
	}
	f.assertNoContainerLeft(t)
	if len(f.removeCtxDone) != 1 || f.removeCtxDone[0] {
		t.Fatalf("the removal ran on the cancelled context: %v", f.removeCtxDone)
	}
}

func TestArchiveErrorsNeverCarryTheHelpersStderr(t *testing.T) {
	f := newFakeDocker()
	f.volumes["data"] = nil
	f.exit = 1
	f.stderr = "open /source/customers/passwords.txt: permission denied"
	a, logged := newArchiver(f)
	_, err := a.Archive(context.Background(), "data", io.Discard)
	if err == nil || strings.Contains(err.Error(), "passwords") || !strings.Contains(err.Error(), "volume archive failed") {
		t.Fatalf("error %v", err)
	}
	if len(*logged) != 1 || !strings.Contains((*logged)[0], "passwords.txt") {
		t.Fatalf("log %v", *logged)
	}
}

func TestArchiveReportsAnImageThatCannotBeMade(t *testing.T) {
	f := newFakeDocker()
	f.volumes["data"] = nil
	a, _ := newArchiver(f)
	a.Image = func(context.Context) (string, error) {
		return "", errors.New("probe helper unavailable: image import failed")
	}
	if _, err := a.Archive(context.Background(), "data", io.Discard); err == nil || !strings.Contains(err.Error(), "image import failed") {
		t.Fatalf("error %v", err)
	}
	if f.count("CreateContainer") != 0 {
		t.Fatal("a container was created without an image")
	}
}

func TestRestoreExtractsIntoANewVolume(t *testing.T) {
	f := newFakeDocker()
	a, _ := newArchiver(f)
	verified := 0
	err := a.Restore(context.Background(), "copy", strings.NewReader("archive-bytes"), map[string]string{"berthy.backup": "b1"}, func() error {
		verified++
		// The checksum is only complete once everything was handed over, and the helper has finished and reported success.
		if f.stdinSeen.String() != "archive-bytes" || !f.stdinClose || f.count("WaitContainer") != 1 {
			t.Errorf("verify ran too early: stdin %q closed %v calls %v", f.stdinSeen.String(), f.stdinClose, f.calls)
		}
		return nil
	})
	if err != nil || verified != 1 {
		t.Fatalf("restore: %v, verified %d", err, verified)
	}
	want := []string{"VolumeExists", "CreateVolume", "CreateContainer", "Attach", "StartContainer", "CloseStdin", "WaitContainer", "RemoveContainer"}
	if got := f.callNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v, want %v", got, want)
	}
	if f.calls[3] != "Attach stdin=true" {
		t.Errorf("attach %q", f.calls[3])
	}
	if labels, ok := f.volumes["copy"]; !ok || labels["berthy.backup"] != "b1" {
		t.Fatalf("volumes %v", f.volumes)
	}
	c := f.onlyContainer(t)
	if !regexp.MustCompile(`^berthy-restore-prod-[0-9a-f]{8}$`).MatchString(c.name) || !c.removed {
		t.Errorf("container %+v", c)
	}
	want2 := jsonOf(t, map[string]any{
		"Image":        "berthy-helper:0123456789ab",
		"Entrypoint":   []string{"/berthy", "volume-extract"},
		"User":         "0:0",
		"AttachStdin":  true,
		"AttachStdout": true,
		"AttachStderr": true,
		"OpenStdin":    true,
		"StdinOnce":    true,
		"Labels":       map[string]string{"berthy.environment": "prod", "berthy.helper": "restore"},
		"HostConfig": map[string]any{
			"NetworkMode":    "none",
			"ReadonlyRootfs": true,
			"CapDrop":        []string{"ALL"},
			"CapAdd":         []string{"CHOWN", "FOWNER", "DAC_OVERRIDE", "FSETID"},
			"SecurityOpt":    []string{"no-new-privileges"},
			"Memory":         256 << 20,
			"Mounts": []map[string]any{{
				"Type": "volume", "Source": "copy", "Target": "/target", "ReadOnly": false,
				"VolumeOptions": map[string]any{"NoCopy": true},
			}},
		},
	})
	if got := spec(t, c); !reflect.DeepEqual(got, want2) {
		t.Fatalf("spec\n got %v\nwant %v", got, want2)
	}
}

func TestRestoreRefusesAnExistingVolumeAndTouchesNothing(t *testing.T) {
	f := newFakeDocker()
	f.volumes["data"] = map[string]string{"owner": "someone"}
	a, _ := newArchiver(f)
	err := a.Restore(context.Background(), "data", strings.NewReader("x"), nil, func() error { t.Error("verify ran"); return nil })
	if err == nil || err.Error() != "volume data already exists; choose an unused name" {
		t.Fatalf("error %v", err)
	}
	if got := f.callNames(); !reflect.DeepEqual(got, []string{"VolumeExists"}) {
		t.Fatalf("calls %v", got)
	}
	if f.volumes["data"]["owner"] != "someone" {
		t.Fatal("the volume changed")
	}
}

func TestRestoreRemovesWhatItCreatedOnEveryFailure(t *testing.T) {
	boom := errors.New("boom")
	cases := map[string]struct {
		setup  func(f *fakeDocker)
		verify func() error
		input  io.Reader
	}{
		"container create fails": {setup: func(f *fakeDocker) { f.createErr = boom }},
		"attach fails":           {setup: func(f *fakeDocker) { f.attachErr = boom }},
		"start fails":            {setup: func(f *fakeDocker) { f.startErr = boom }},
		"helper exits non-zero":  {setup: func(f *fakeDocker) { f.exit = 1; f.stderr = "volume extract failed" }},
		"wait fails":             {setup: func(f *fakeDocker) { f.waitErr = boom }},
		"checksum mismatch":      {verify: func() error { return errors.New("checksum mismatch") }},
		"source breaks":          {input: io.MultiReader(strings.NewReader("part"), iotest.ErrReader(boom))},
	}
	for name, c := range cases {
		f := newFakeDocker()
		f.volumes["precious"] = map[string]string{"keep": "me"}
		if c.setup != nil {
			c.setup(f)
		}
		input := c.input
		if input == nil {
			input = strings.NewReader("archive")
		}
		verify := c.verify
		if verify == nil {
			verify = func() error { return nil }
		}
		a, _ := newArchiver(f)
		err := a.Restore(context.Background(), "fresh", input, nil, verify)
		if err == nil {
			t.Errorf("%s: no error", name)
		}
		if _, ok := f.volumes["fresh"]; ok {
			t.Errorf("%s: the new volume was left behind; calls %v", name, f.calls)
		}
		if _, ok := f.volumes["precious"]; !ok {
			t.Errorf("%s: another volume was removed", name)
		}
		f.assertNoContainerLeft(t)
	}
}

func TestRestoreSkipsTheChecksumWhenTheHelperFailed(t *testing.T) {
	f := newFakeDocker()
	f.exit = 1
	a, _ := newArchiver(f)
	err := a.Restore(context.Background(), "fresh", strings.NewReader("x"), nil, func() error { t.Error("verify ran after a failed extract"); return nil })
	if err == nil || !strings.Contains(err.Error(), "volume restore failed") {
		t.Fatalf("error %v", err)
	}
}

func TestRestoreNeverRemovesAVolumeItDidNotCreate(t *testing.T) {
	// The create fails because the volume appeared in the meantime: it is someone else's, and stays.
	f := newFakeDocker()
	f.createVolumeErr = &engine.StatusError{Code: http.StatusConflict}
	a, _ := newArchiver(f)
	err := a.Restore(context.Background(), "fresh", strings.NewReader("x"), nil, func() error { return nil })
	if err == nil {
		t.Fatal("no error")
	}
	if f.count("RemoveVolume") != 0 || f.count("CreateContainer") != 0 {
		t.Fatalf("calls %v", f.calls)
	}
}

func TestRestoreCancelledStillRemovesContainerAndVolume(t *testing.T) {
	f := newFakeDocker()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.onWait = func(context.Context) { cancel() }
	a, _ := newArchiver(f)
	err := a.Restore(ctx, "fresh", strings.NewReader("x"), nil, func() error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v", err)
	}
	f.assertNoContainerLeft(t)
	if _, ok := f.volumes["fresh"]; ok {
		t.Fatal("the new volume was left behind")
	}
	for _, done := range f.removeCtxDone {
		if done {
			t.Fatal("a removal ran on the cancelled context")
		}
	}
}

func TestRestoreErrorsNeverCarryTheHelpersStderr(t *testing.T) {
	f := newFakeDocker()
	f.exit = 1
	f.stderr = "chown /target/secret-file: operation not permitted"
	a, logged := newArchiver(f)
	err := a.Restore(context.Background(), "fresh", strings.NewReader("x"), nil, func() error { return nil })
	if err == nil || strings.Contains(err.Error(), "secret-file") {
		t.Fatalf("error %v", err)
	}
	if len(*logged) != 1 || !strings.Contains((*logged)[0], "secret-file") {
		t.Fatalf("log %v", *logged)
	}
}

func TestArchiverGivesEveryStreamADeadline(t *testing.T) {
	// The engine's streaming calls refuse a context without one.
	f := newFakeDocker()
	f.volumes["data"] = nil
	a, _ := newArchiver(f)
	a.Image = func(ctx context.Context) (string, error) {
		if _, ok := ctx.Deadline(); !ok {
			return "", errors.New("image import needs a deadline")
		}
		return "berthy-helper:0123456789ab", nil
	}
	if _, err := a.Archive(context.Background(), "data", io.Discard); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if err := a.Restore(ctx, "copy", strings.NewReader("x"), nil, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}
