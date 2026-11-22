package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/manifest"
	"berthy/internal/objectstore"
	"berthy/internal/state"
)

// memStore is an in-memory object store. A Put reads its whole body, so a failing source fails the upload as it would
// against S3, and a failed upload leaves no object.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	deleted []string
	// failPut makes a Put of a key with this suffix fail after it has read some of the body.
	failPut string
	// failDelete makes a Delete of a key with this suffix fail.
	failDelete string
}

func newMemStore() *memStore { return &memStore{objects: map[string][]byte{}} }

func (s *memStore) Put(ctx context.Context, key string, r io.Reader, _ map[string]string) (int64, error) {
	s.mu.Lock()
	fail := s.failPut != "" && strings.HasSuffix(key, s.failPut)
	s.mu.Unlock()
	if fail {
		io.CopyN(io.Discard, r, 4)
		return 0, errors.New("upload failed: access denied")
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, errors.New("reading the source failed")
	}
	if err = ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	s.objects[key] = b
	s.mu.Unlock()
	return int64(len(b)), nil
}

func (s *memStore) Get(_ context.Context, key string) (io.ReadCloser, objectstore.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[key]
	if !ok {
		return nil, objectstore.ObjectInfo{}, objectstore.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), objectstore.ObjectInfo{Key: key, Size: int64(len(b))}, nil
}

func (s *memStore) Stat(_ context.Context, key string) (objectstore.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[key]
	if !ok {
		return objectstore.ObjectInfo{}, objectstore.ErrNotFound
	}
	return objectstore.ObjectInfo{Key: key, Size: int64(len(b))}, nil
}

func (s *memStore) List(_ context.Context, prefix string) ([]objectstore.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []objectstore.ObjectInfo
	for k, b := range s.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, objectstore.ObjectInfo{Key: k, Size: int64(len(b))})
		}
	}
	slices.SortFunc(out, func(a, b objectstore.ObjectInfo) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}

func (s *memStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failDelete != "" && strings.HasSuffix(key, s.failDelete) {
		return errors.New("object storage unavailable")
	}
	delete(s.objects, key)
	s.deleted = append(s.deleted, key)
	return nil
}

func (s *memStore) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.objects {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func (s *memStore) put(key string, body []byte) {
	s.mu.Lock()
	s.objects[key] = body
	s.mu.Unlock()
}

// fakeJobs is the scheduler's side of a preparation job.
type fakeJobs struct {
	mu       sync.Mutex
	runs     []string
	run      state.JobRun
	err      error
	released int
}

func (f *fakeJobs) RunHeld(_ context.Context, name string) (state.JobRun, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, name)
	if f.err != nil {
		return state.JobRun{}, nil, f.err
	}
	return f.run, func() { f.mu.Lock(); f.released++; f.mu.Unlock() }, nil
}

func (f *fakeJobs) releases() int { f.mu.Lock(); defer f.mu.Unlock(); return f.released }

type rig struct {
	t      *testing.T
	runner *Runner
	docker *fakeDocker
	objs   *memStore
	st     *state.Store
	jobs   *fakeJobs
	m      manifest.Manifest
	clock  time.Time
	logged []string
	logMu  sync.Mutex
}

func openState(t *testing.T) *state.Store { return openStateEnv(t, "prod") }

func openStateEnv(t *testing.T, env string) *state.Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir, key, env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newRig(t *testing.T) *rig {
	t.Helper()
	g := &rig{t: t, docker: newFakeDocker(), objs: newMemStore(), st: openState(t), jobs: &fakeJobs{run: state.JobRun{ID: 1, Status: "succeeded"}}, clock: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)}
	g.docker.volumes["files_uploads"] = nil
	arch, _ := newArchiver(g.docker)
	g.m = manifest.Manifest{
		Project: "demo",
		Apps:    map[string]manifest.App{"files": {Kind: "compose"}},
		Destinations: map[string]manifest.Destination{
			"offsite": {Type: "s3", Bucket: "bkt", AccessKeySecret: "S3_ACCESS_KEY", SecretKeySecret: "S3_SECRET_KEY"},
		},
		Backups: map[string]manifest.Backup{
			"uploads": {App: "files", Volume: "uploads", Destination: "offsite"},
		},
	}
	g.runner = &Runner{
		Store:    g.st,
		Archiver: arch,
		Objects:  func(context.Context, manifest.Destination) (objectstore.Store, error) { return g.objs, nil },
		Jobs:     g.jobs,
		Resolve: func(_ context.Context, _ manifest.Manifest, b manifest.Backup) (string, error) {
			if b.Volume == "uploads" {
				return "files_uploads", nil
			}
			return "", ErrNotMounted
		},
		Project: "demo",
		Env:     "prod",
		Now:     func() time.Time { g.clock = g.clock.Add(time.Minute); return g.clock },
		Log:     func(s string) { g.logMu.Lock(); g.logged = append(g.logged, s); g.logMu.Unlock() },
	}
	return g
}

// archive makes the volume's helper print this archive.
func (g *rig) archive(body string) { g.docker.stdout = strings.NewReader(body) }

func sum(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func (g *rig) run(name string) Result {
	g.t.Helper()
	res, err := g.runner.Run(context.Background(), g.m, name, "manual")
	if err != nil {
		g.t.Fatalf("run: %v", err)
	}
	return res
}

func (g *rig) lastRun(name string) state.BackupRun {
	g.t.Helper()
	runs, err := g.st.BackupRuns(context.Background(), name, 1)
	if err != nil || len(runs) != 1 {
		g.t.Fatalf("runs: %+v %v", runs, err)
	}
	return runs[0]
}

func (g *rig) sidecarOf(key string) Sidecar {
	g.t.Helper()
	g.objs.mu.Lock()
	b := g.objs.objects[sidecarKeyOf(key)]
	g.objs.mu.Unlock()
	var sc Sidecar
	if err := json.Unmarshal(b, &sc); err != nil {
		g.t.Fatalf("sidecar of %s: %v (%q)", key, err, b)
	}
	return sc
}

func TestRunUploadsTheArchiveThenItsSidecarAndRecordsTheRun(t *testing.T) {
	g := newRig(t)
	g.archive("the-archive-bytes")
	res := g.run("uploads")
	wantKey := "demo/prod/files/uploads/uploads/20261116T100100Z-1.tar.gz"
	if res.Key != wantKey || res.Run.Status != "succeeded" || res.Run.ID != 1 {
		t.Fatalf("result: %+v", res)
	}
	if got := g.objs.keys(); !slices.Equal(got, []string{"demo/prod/files/uploads/uploads/20261116T100100Z-1.json", wantKey}) {
		t.Fatalf("objects: %v", got)
	}
	if got := string(g.objs.objects[wantKey]); got != "the-archive-bytes" {
		t.Fatalf("archive: %q", got)
	}
	sc := g.sidecarOf(wantKey)
	want := Sidecar{Format: "berthy-volume-tar-gzip-1", Project: "demo", Environment: "prod", Backup: "uploads", App: "files", Volume: "uploads", DockerVolume: "files_uploads",
		Size: int64(len("the-archive-bytes")), SHA256: sum("the-archive-bytes"), Started: time.Date(2026, 11, 16, 10, 1, 0, 0, time.UTC), Finished: time.Date(2026, 11, 16, 10, 2, 0, 0, time.UTC), RunID: 1}
	if sc != want {
		t.Fatalf("sidecar\n got %+v\nwant %+v", sc, want)
	}
	rec := g.lastRun("uploads")
	if rec.Status != "succeeded" || rec.Size != sc.Size || rec.SHA256 != sc.SHA256 || rec.ObjectKey != wantKey || rec.Trigger != "manual" || rec.Destination != "offsite" || rec.Finished.IsZero() {
		t.Fatalf("recorded run: %+v", rec)
	}
	g.docker.assertNoContainerLeft(t)
	var spec struct {
		HostConfig struct{ Mounts []struct{ Source string } }
	}
	if err := json.Unmarshal(g.docker.onlyContainer(t).spec, &spec); err != nil || spec.HostConfig.Mounts[0].Source != "files_uploads" {
		t.Fatalf("the helper did not archive the resolved Docker volume: %s", g.docker.onlyContainer(t).spec)
	}
}

func TestRunUsesTheManifestsPrefix(t *testing.T) {
	g := newRig(t)
	b := g.m.Backups["uploads"]
	b.Prefix = "/offsite/uploads/"
	g.m.Backups["uploads"] = b
	g.archive("x")
	res := g.run("uploads")
	if res.Key != "demo/prod/offsite/uploads/uploads/20261116T100100Z-1.tar.gz" {
		t.Fatalf("key %q", res.Key)
	}
	b.Prefix = "../escape"
	g.m.Backups["uploads"] = b
	if _, err := g.runner.Run(context.Background(), g.m, "uploads", "manual"); err == nil || !strings.Contains(err.Error(), "invalid prefix") {
		t.Fatalf("an escaping prefix: %v", err)
	}
}

func TestAFailedUploadLeavesNoSidecarAndIsRecorded(t *testing.T) {
	g := newRig(t)
	g.archive(strings.Repeat("a", 1000))
	g.objs.failPut = ".tar.gz"
	res := g.run("uploads")
	if res.Run.Status != "failed" || res.Run.Detail != "upload failed: access denied" {
		t.Fatalf("result: %+v", res.Run)
	}
	if got := g.objs.keys(); len(got) != 0 {
		t.Fatalf("objects after a failed upload: %v", got)
	}
	rec := g.lastRun("uploads")
	if rec.Status != "failed" || !strings.HasPrefix(rec.Detail, "upload failed") || rec.SHA256 != "" || rec.Finished.IsZero() {
		t.Fatalf("recorded run: %+v", rec)
	}
	g.docker.assertNoContainerLeft(t)
}

func TestAFailedArchiveLeavesNoSidecarAndIsRecorded(t *testing.T) {
	g := newRig(t)
	g.archive("partial")
	g.docker.exit = 1
	res := g.run("uploads")
	if res.Run.Status != "failed" || !strings.HasPrefix(res.Run.Detail, "archive failed") {
		t.Fatalf("result: %+v", res.Run)
	}
	if got := g.objs.keys(); len(got) != 0 {
		t.Fatalf("objects after a failed archive: %v", got)
	}
	rec := g.lastRun("uploads")
	if rec.Status != "failed" || !strings.HasPrefix(rec.Detail, "archive failed") {
		t.Fatalf("recorded run: %+v", rec)
	}
	// The helper's own words stay in the agent's log.
	if strings.Contains(rec.Detail, "helper said") {
		t.Fatalf("detail: %q", rec.Detail)
	}
}

func TestAMissingVolumeIsAnArchiveFailure(t *testing.T) {
	g := newRig(t)
	delete(g.docker.volumes, "files_uploads")
	res := g.run("uploads")
	if res.Run.Status != "failed" || !strings.HasPrefix(res.Run.Detail, "archive failed") || len(g.objs.keys()) != 0 {
		t.Fatalf("result: %+v %v", res.Run, g.objs.keys())
	}
}

func TestAFailedSidecarFailsTheRunAndRemovesTheArchive(t *testing.T) {
	g := newRig(t)
	g.archive("data")
	g.objs.failPut = ".json"
	res := g.run("uploads")
	if res.Run.Status != "failed" || !strings.HasPrefix(res.Run.Detail, "sidecar failed") {
		t.Fatalf("result: %+v", res.Run)
	}
	if got := g.objs.keys(); len(got) != 0 {
		t.Fatalf("an archive without a sidecar was left behind: %v", got)
	}
}

func TestAFailedBeforeJobFailsThePreparationAndNeverArchives(t *testing.T) {
	g := newRig(t)
	b := g.m.Backups["uploads"]
	b.Before = "dump"
	g.m.Backups["uploads"] = b
	g.jobs.run = state.JobRun{ID: 9, Status: "failed", Detail: "exit status 3"}
	res := g.run("uploads")
	if res.Run.Status != "failed" || res.Run.Detail != "preparation failed: exit status 3" {
		t.Fatalf("result: %+v", res.Run)
	}
	if g.docker.count("CreateContainer") != 0 || len(g.objs.keys()) != 0 {
		t.Fatalf("the archive started after a failed preparation: %v %v", g.docker.calls, g.objs.keys())
	}
	if !slices.Equal(g.jobs.runs, []string{"dump"}) || g.jobs.releases() != 1 {
		t.Fatalf("job: runs %v releases %d", g.jobs.runs, g.jobs.releases())
	}
	if rec := g.lastRun("uploads"); rec.Detail != "preparation failed: exit status 3" {
		t.Fatalf("recorded: %+v", rec)
	}
}

func TestAFailedBeforeJobThatKeptItsOutputNamesTheCommandThatShowsIt(t *testing.T) {
	g := newRig(t)
	b := g.m.Backups["uploads"]
	b.Before = "dump"
	g.m.Backups["uploads"] = b
	g.jobs.run = state.JobRun{ID: 9, Status: "failed", Detail: "exit status 3", HasOutput: true}
	res := g.run("uploads")
	if want := "preparation failed: exit status 3: berthy job logs dump shows its output"; res.Run.Detail != want {
		t.Fatalf("detail %q, want %q", res.Run.Detail, want)
	}
}

func TestABeforeJobThatCannotRunFailsThePreparation(t *testing.T) {
	g := newRig(t)
	b := g.m.Backups["uploads"]
	b.Before = "dump"
	g.m.Backups["uploads"] = b
	g.jobs.err = errors.New("unknown job")
	if res := g.run("uploads"); res.Run.Status != "failed" || !strings.HasPrefix(res.Run.Detail, "preparation failed") {
		t.Fatalf("result: %+v", res.Run)
	}
	g.runner.Jobs = nil
	if res := g.run("uploads"); res.Run.Status != "failed" || !strings.HasPrefix(res.Run.Detail, "preparation failed") {
		t.Fatalf("without a scheduler: %+v", res.Run)
	}
}

func TestABeforeJobIsHeldUntilTheArchiveHasEnded(t *testing.T) {
	g := newRig(t)
	b := g.m.Backups["uploads"]
	b.Before = "dump"
	g.m.Backups["uploads"] = b
	g.archive("data")
	if res := g.run("uploads"); res.Run.Status != "succeeded" {
		t.Fatalf("result: %+v", res.Run)
	}
	if !slices.Equal(g.jobs.runs, []string{"dump"}) || g.jobs.releases() != 1 {
		t.Fatalf("job: runs %v releases %d", g.jobs.runs, g.jobs.releases())
	}
}

func TestASecondBackupOfTheSameVolumeIsRefusedWhileOneRuns(t *testing.T) {
	g := newRig(t)
	pr, pw := io.Pipe()
	g.docker.stdout = pr
	done := make(chan Result, 1)
	started := make(chan struct{})
	g.docker.onWait = func(context.Context) {}
	p, err := g.runner.Begin(context.Background(), g.m, "uploads", "manual")
	if err != nil {
		t.Fatal(err)
	}
	go func() { close(started); done <- p.Execute(context.Background()) }()
	<-started
	if _, err = g.runner.Run(context.Background(), g.m, "uploads", "schedule"); !errors.Is(err, ErrBackupRunning) {
		t.Fatalf("second run: %v", err)
	}
	if runs, _ := g.st.BackupRuns(context.Background(), "uploads", 10); len(runs) != 1 {
		t.Fatalf("a refused run was recorded: %+v", runs)
	}
	pw.Write([]byte("data"))
	pw.Close()
	if res := <-done; res.Run.Status != "succeeded" {
		t.Fatalf("first run: %+v", res.Run)
	}
	g.archive("again")
	if res := g.run("uploads"); res.Run.Status != "succeeded" {
		t.Fatalf("run after the first ended: %+v", res.Run)
	}
}

func TestAnUnresolvedVolumeAndOtherRefusalsRecordNothing(t *testing.T) {
	g := newRig(t)
	g.m.Backups["lost"] = manifest.Backup{App: "files", Volume: "ghost", Destination: "offsite"}
	_, err := g.runner.Run(context.Background(), g.m, "lost", "manual")
	if err == nil || err.Error() != "backup lost: volume ghost is not mounted by app files" {
		t.Fatalf("unresolved volume: %v", err)
	}
	if _, err = g.runner.Run(context.Background(), g.m, "nope", "manual"); !errors.Is(err, ErrUnknownBackup) {
		t.Fatalf("unknown backup: %v", err)
	}
	g.runner.Objects = func(context.Context, manifest.Destination) (objectstore.Store, error) { return nil, ErrCredentials }
	if _, err = g.runner.Run(context.Background(), g.m, "uploads", "manual"); !errors.Is(err, ErrCredentials) {
		t.Fatalf("credentials: %v", err)
	}
	g.runner.Objects = func(context.Context, manifest.Destination) (objectstore.Store, error) {
		return nil, errors.New("secret-bearing detail")
	}
	if _, err = g.runner.Run(context.Background(), g.m, "uploads", "manual"); !errors.Is(err, ErrStorage) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("storage: %v", err)
	}
	if runs, _ := g.st.BackupRuns(context.Background(), "", 10); len(runs) != 0 {
		t.Fatalf("refusals were recorded: %+v", runs)
	}
}

func TestAnEndedContextRecordsTheAgentStopping(t *testing.T) {
	g := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	g.docker.stdout = io.MultiReader(strings.NewReader("part"), blockedReader{ctx})
	p, err := g.runner.Begin(ctx, g.m, "uploads", "manual")
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(50*time.Millisecond, cancel)
	res := p.Execute(ctx)
	if res.Run.Status != "failed" || res.Run.Detail != "agent stopped during the backup" {
		t.Fatalf("result: %+v", res.Run)
	}
	if rec := g.lastRun("uploads"); rec.Status != "failed" || rec.Detail != "agent stopped during the backup" {
		t.Fatalf("recorded: %+v", rec)
	}
	if len(g.objs.keys()) != 0 {
		t.Fatalf("objects: %v", g.objs.keys())
	}
}

// blockedReader blocks until its context ends, then fails.
type blockedReader struct{ ctx context.Context }

func (b blockedReader) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }

func TestRetentionKeepsTheNewestAndDeletesArchiveBeforeSidecar(t *testing.T) {
	g := newRig(t)
	b := g.m.Backups["uploads"]
	b.Retention = &manifest.Retention{Keep: float64(2)}
	g.m.Backups["uploads"] = b
	// Things retention must never touch: an archive without a sidecar, another backup's objects, and a stranger's file.
	g.objs.put("demo/prod/files/uploads/uploads/20250101T000000Z-99.tar.gz", []byte("orphan"))
	g.objs.put("demo/prod/files/uploads/uploads/notes.txt", []byte("mine"))
	g.objs.put("demo/prod/files/uploads/other/20250101T000000Z-1.tar.gz", []byte("other"))
	g.objs.put("demo/prod/files/uploads/other/20250101T000000Z-1.json", []byte(`{"format":"berthy-volume-tar-gzip-1"}`))
	var keys []string
	for _, body := range []string{"one", "two", "three"} {
		g.archive(body)
		keys = append(keys, g.run("uploads").Key)
	}
	got := g.objs.keys()
	for _, gone := range []string{keys[0], sidecarKeyOf(keys[0])} {
		if slices.Contains(got, gone) {
			t.Errorf("%s was kept", gone)
		}
	}
	for _, kept := range []string{keys[1], sidecarKeyOf(keys[1]), keys[2], sidecarKeyOf(keys[2]), "demo/prod/files/uploads/uploads/20250101T000000Z-99.tar.gz", "demo/prod/files/uploads/uploads/notes.txt", "demo/prod/files/uploads/other/20250101T000000Z-1.tar.gz", "demo/prod/files/uploads/other/20250101T000000Z-1.json"} {
		if !slices.Contains(got, kept) {
			t.Errorf("%s was deleted", kept)
		}
	}
	if !slices.Equal(g.objs.deleted, []string{keys[0], sidecarKeyOf(keys[0])}) {
		t.Fatalf("deletions in order: %v", g.objs.deleted)
	}
}

func TestRetentionAllDeletesNothingAndAFailedDeleteDoesNotFailTheRun(t *testing.T) {
	g := newRig(t)
	for _, body := range []string{"one", "two", "three"} {
		g.archive(body)
		g.run("uploads")
	}
	if len(g.objs.keys()) != 6 || len(g.objs.deleted) != 0 {
		t.Fatalf("keep all: %v deleted %v", g.objs.keys(), g.objs.deleted)
	}
	b := g.m.Backups["uploads"]
	b.Retention = &manifest.Retention{Keep: 1}
	g.m.Backups["uploads"] = b
	g.objs.failDelete = ".tar.gz"
	g.archive("four")
	if res := g.run("uploads"); res.Run.Status != "succeeded" {
		t.Fatalf("a failed retention failed the run: %+v", res.Run)
	}
	if len(g.objs.keys()) != 8 {
		t.Fatalf("sidecars were deleted although their archives were not: %v", g.objs.keys())
	}
}

func TestListShowsOnlyBackupsWithASidecarNewestFirst(t *testing.T) {
	g := newRig(t)
	g.m.Backups["docs"] = manifest.Backup{App: "files", Volume: "uploads", Destination: "offsite", Prefix: "docs"}
	g.archive("one")
	first := g.run("uploads")
	g.archive("twotwo")
	second := g.run("uploads")
	g.archive("docs-archive")
	third := g.run("docs")
	g.objs.put("demo/prod/files/uploads/uploads/20250101T000000Z-99.tar.gz", []byte("orphan"))
	g.objs.put("demo/prod/files/uploads/uploads/20250101T000000Z-98.json", []byte("not json"))
	list, err := g.runner.List(context.Background(), g.m, "")
	if err != nil || len(list) != 3 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if list[0].Key != third.Key || list[1].Key != second.Key || list[2].Key != first.Key {
		t.Fatalf("order: %+v", list)
	}
	e := list[1]
	if e.Backup != "uploads" || e.App != "files" || e.Volume != "uploads" || e.Size != 6 || e.SHA256 != sum("twotwo") || e.Finished.IsZero() {
		t.Fatalf("entry: %+v", e)
	}
	only, err := g.runner.List(context.Background(), g.m, "uploads")
	if err != nil || len(only) != 2 || only[0].Key != second.Key {
		t.Fatalf("filtered: %+v %v", only, err)
	}
	if _, err = g.runner.List(context.Background(), g.m, "nope"); !errors.Is(err, ErrUnknownBackup) {
		t.Fatalf("unknown filter: %v", err)
	}
}

func TestListSkipsSidecarsOfAnotherEnvironmentOrFormat(t *testing.T) {
	g := newRig(t)
	g.archive("one")
	res := g.run("uploads")
	other := g.sidecarOf(res.Key)
	other.Environment = "staging"
	b, _ := json.Marshal(other)
	g.objs.put("demo/prod/files/uploads/uploads/20260101T000000Z-7.json", b)
	other.Environment, other.Format = "prod", "future-format"
	b, _ = json.Marshal(other)
	g.objs.put("demo/prod/files/uploads/uploads/20260101T000000Z-8.json", b)
	list, err := g.runner.List(context.Background(), g.m, "")
	if err != nil || len(list) != 1 || list[0].Key != res.Key {
		t.Fatalf("list: %+v %v", list, err)
	}
}

func TestRestoreFillsANewVolumeFromTheArchive(t *testing.T) {
	g := newRig(t)
	g.archive("the-archive-bytes")
	res := g.run("uploads")
	g.docker = fresh(g, "restore")
	if err := g.runner.Restore(context.Background(), g.m, res.Key, "restored-1", "op-1"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := g.docker.stdinSeen.String(); got != "the-archive-bytes" {
		t.Fatalf("the helper was fed %q", got)
	}
	labels, ok := g.docker.volumes["restored-1"]
	if !ok || labels["berthy.environment"] != "prod" || labels["berthy.restored-from"] != res.Key || labels[RestoreOperationLabel] != "op-1" {
		t.Fatalf("volume %v %v", ok, labels)
	}
	g.docker.assertNoContainerLeft(t)
}

// fresh gives the runner a new fake Docker, as a restore sees one after the backup's helper has gone.
func fresh(g *rig, _ string) *fakeDocker {
	d := newFakeDocker()
	arch, _ := newArchiver(d)
	g.runner.Archiver = arch
	return d
}

func TestRestoredVolumeCarriesTheProjectLabel(t *testing.T) {
	g := newRig(t)
	g.runner.Project = "shop"
	g.archive("the-archive-bytes")
	res := g.run("uploads")
	g.docker = fresh(g, "restore")
	if err := g.runner.Restore(context.Background(), g.m, res.Key, "restored-1", "op-1"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if labels := g.docker.volumes["restored-1"]; labels["berthy.project"] != "shop" || labels["berthy.environment"] != "prod" {
		t.Fatalf("volume labels %v", labels)
	}
}

func TestRestoreRemovesTheNewVolumeWhenTheChecksumDiffers(t *testing.T) {
	g := newRig(t)
	g.archive("the-archive-bytes")
	res := g.run("uploads")
	g.objs.put(res.Key, []byte("tampered-archive!!"))
	g.docker = fresh(g, "")
	err := g.runner.Restore(context.Background(), g.m, res.Key, "restored-1", "op-1")
	if !errors.Is(err, ErrChecksumMismatch) || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("restore: %v", err)
	}
	if _, ok := g.docker.volumes["restored-1"]; ok {
		t.Fatal("the new volume was kept after a checksum mismatch")
	}
	g.docker.assertNoContainerLeft(t)
	// A truncated archive differs in size, whatever its prefix hashes to.
	g.objs.put(res.Key, []byte("the-archive"))
	if err = g.runner.Restore(context.Background(), g.m, res.Key, "restored-2", "op-2"); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("truncated restore: %v", err)
	}
}

func TestRestoreRefusesAKeyWithoutASidecar(t *testing.T) {
	g := newRig(t)
	g.archive("data")
	res := g.run("uploads")
	g.objs.mu.Lock()
	delete(g.objs.objects, sidecarKeyOf(res.Key))
	g.objs.mu.Unlock()
	g.docker = fresh(g, "")
	if err := g.runner.Restore(context.Background(), g.m, res.Key, "restored-1", "op-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restore without a sidecar: %v", err)
	}
	if len(g.docker.calls) != 0 {
		t.Fatalf("Docker was used for a refused restore: %v", g.docker.calls)
	}
	for _, key := range []string{"demo/prod/files/uploads/uploads/nothing.tar.gz", "elsewhere/20260101T000000Z-1.tar.gz", "demo/prod/files/uploads/uploads/20260101T000000Z-1.json", "demo/prod/files/uploads/uploads/../x.tar.gz"} {
		if err := g.runner.Restore(context.Background(), g.m, key, "restored-1", "op-1"); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", key, err)
		}
	}
}

func TestRestoreRefusesAnExistingOrInvalidVolume(t *testing.T) {
	g := newRig(t)
	g.archive("data")
	res := g.run("uploads")
	g.docker = fresh(g, "")
	g.docker.volumes["taken"] = nil
	if err := g.runner.Restore(context.Background(), g.m, res.Key, "taken", "op-1"); !errors.Is(err, ErrVolumeExists) {
		t.Fatalf("existing volume: %v", err)
	}
	if err := g.runner.Restore(context.Background(), g.m, res.Key, "bad/name", "op-1"); !errors.Is(err, ErrVolumeName) {
		t.Fatalf("invalid name: %v", err)
	}
	if g.docker.count("CreateVolume") != 0 {
		t.Fatalf("calls: %v", g.docker.calls)
	}
	// A dry run checks the same things without touching Docker's state.
	sc, err := g.runner.Check(context.Background(), g.m, res.Key, "fine")
	if err != nil || sc.Backup != "uploads" || sc.SHA256 != sum("data") {
		t.Fatalf("check: %+v %v", sc, err)
	}
	if _, err = g.runner.Check(context.Background(), g.m, res.Key, "taken"); !errors.Is(err, ErrVolumeExists) {
		t.Fatalf("check of an existing volume: %v", err)
	}
	if g.docker.count("CreateVolume") != 0 {
		t.Fatalf("a check created a volume: %v", g.docker.calls)
	}
}

func TestRestoreIsRefusedForASidecarOfAnotherBackup(t *testing.T) {
	g := newRig(t)
	g.archive("data")
	res := g.run("uploads")
	sc := g.sidecarOf(res.Key)
	sc.Backup = "other"
	b, _ := json.Marshal(sc)
	g.objs.put(sidecarKeyOf(res.Key), b)
	if err := g.runner.Restore(context.Background(), g.m, res.Key, "restored-1", "op-1"); !errors.Is(err, ErrInvalidSidecar) {
		t.Fatalf("restore: %v", err)
	}
}

func TestStorageErrorsReachCallersAsFixedRefusals(t *testing.T) {
	g := newRig(t)
	g.runner.Objects = func(context.Context, manifest.Destination) (objectstore.Store, error) {
		return failingStore{newMemStore()}, nil
	}
	if _, err := g.runner.List(context.Background(), g.m, ""); !errors.Is(err, ErrStorage) {
		t.Fatalf("list: %v", err)
	}
	var refusal Refusal
	if err := g.runner.Restore(context.Background(), g.m, "demo/prod/files/uploads/uploads/20260101T000000Z-1.tar.gz", "x", "op-1"); !errors.As(err, &refusal) {
		t.Fatalf("restore: %v", err)
	}
	if !PublicRefusal(ErrNotFound.Error()) || PublicRefusal("backup lost: volume ghost is not mounted by app files") {
		t.Fatal("PublicRefusal accepts the wrong texts")
	}
}

type failingStore struct{ *memStore }

func (failingStore) List(context.Context, string) ([]objectstore.ObjectInfo, error) {
	return nil, errors.New("https://s3.example/bucket?X-Amz-Signature=secret")
}
func (failingStore) Get(context.Context, string) (io.ReadCloser, objectstore.ObjectInfo, error) {
	return nil, objectstore.ObjectInfo{}, errors.New("https://s3.example/bucket?X-Amz-Signature=secret")
}

func TestAnArchiveOfAnotherEnvironmentOfTheProjectIsNotPruned(t *testing.T) {
	g := newRig(t)
	b := g.m.Backups["uploads"]
	b.Retention = &manifest.Retention{Keep: float64(1)}
	g.m.Backups["uploads"] = b
	g.archive("one")
	first := g.run("uploads")
	other := g.sidecarOf(first.Key)
	other.Environment = "stage"
	body, err := json.Marshal(other)
	if err != nil {
		t.Fatal(err)
	}
	foreignArchive := strings.TrimSuffix(first.Key, ".tar.gz") + "-stage.tar.gz"
	foreignSidecar := strings.TrimSuffix(foreignArchive, ".tar.gz") + ".json"
	g.objs.put(foreignArchive, []byte("stage-archive"))
	g.objs.put(foreignSidecar, body)
	g.archive("two")
	second := g.run("uploads")
	if second.Run.Status != "succeeded" {
		t.Fatalf("second: %+v", second.Run)
	}
	got := g.objs.keys()
	if !slices.Contains(got, foreignArchive) || !slices.Contains(got, foreignSidecar) {
		t.Fatalf("another environment's archive was pruned: %v", got)
	}
	if slices.Contains(got, first.Key) || slices.Contains(got, sidecarKeyOf(first.Key)) {
		t.Fatalf("our older archive was kept: %v", got)
	}
}

func TestTwoEnvironmentsWithOnePrefixNeverWriteOneArchiveKey(t *testing.T) {
	g := newRig(t)
	fixed := time.Date(2026, 11, 16, 10, 1, 0, 0, time.UTC)
	b := g.m.Backups["uploads"]
	b.Prefix = "shared"
	g.m.Backups["uploads"] = b
	g.archive("alpha-body")
	g.runner.Now = func() time.Time { return fixed }
	first, err := g.runner.Run(context.Background(), g.m, "uploads", "manual")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	stage := openStateEnv(t, "stage")
	arch, _ := newArchiver(g.docker)
	other := &Runner{
		Store: stage, Archiver: arch, Objects: g.runner.Objects, Jobs: g.jobs, Resolve: g.runner.Resolve,
		Project: "demo", Env: "stage", Now: func() time.Time { return fixed },
	}
	g.archive("stage-body")
	second, err := other.Run(context.Background(), g.m, "uploads", "manual")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.Key == second.Key {
		t.Fatalf("both environments wrote the same key %q", first.Key)
	}
	wantA, wantB := "demo/prod/shared/uploads/20261116T100100Z-1.tar.gz", "demo/stage/shared/uploads/20261116T100100Z-1.tar.gz"
	if first.Key != wantA || second.Key != wantB {
		t.Fatalf("keys\n got %q and %q\nwant %q and %q", first.Key, second.Key, wantA, wantB)
	}
}
