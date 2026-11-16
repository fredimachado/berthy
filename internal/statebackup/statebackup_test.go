package statebackup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/manifest"
	"berthy/internal/objectstore"
	"berthy/internal/state"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

const (
	testAccess = "AKIATESTACCESS"
	testSecret = "test-secret-key-0123456789"
	testValue  = "plaintext-secret-value"
)

// clock is the test's time. The runner and the scheduler read it through Now.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// rig is an agent's state with one secret, an in-process S3 bucket and a runner that writes to it.
type rig struct {
	t        *testing.T
	store    *state.Store
	stateDir string
	keyPath  string
	key      []byte
	srv      *httptest.Server
	objects  objectstore.Store
	clock    *clock
	m        manifest.Manifest
	runner   *Runner
}

func newRig(t *testing.T, keep int) *rig {
	t.Helper()
	ctx := context.Background()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(stateDir, keyPath, "prod")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err = store.PutSecret(ctx, state.Actor{ID: "ops", Name: "ops"}, state.SecretInfo{Name: "DB_PASSWORD"}, []byte(testValue)); err != nil {
		t.Fatal(err)
	}
	backend := s3mem.New()
	if err = backend.CreateBucket("bkt"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	objects, err := objectstore.New(objectstore.Config{Endpoint: srv.URL, Bucket: "bkt", AccessKey: testAccess, SecretKey: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	g := &rig{t: t, store: store, stateDir: stateDir, keyPath: keyPath, key: key, srv: srv, objects: objects,
		clock: &clock{now: time.Date(2026, 11, 16, 10, 0, 30, 0, time.UTC)}}
	g.m = manifest.Manifest{
		Destinations: map[string]manifest.Destination{"offsite": {Type: "s3", Endpoint: srv.URL, Bucket: "bkt", AccessKeySecret: "S3_ACCESS_KEY", SecretKeySecret: "S3_SECRET_KEY"}},
		StateBackup:  &manifest.StateBackup{Destination: "offsite", Keep: &keep},
	}
	g.runner = g.newRunner(store)
	return g
}

// newRunner returns a runner on store that writes to the rig's bucket through wrap, when given.
func (g *rig) newRunner(store *state.Store, wrap ...func(objectstore.Store) objectstore.Store) *Runner {
	return &Runner{Store: store, Env: "prod", Dir: g.stateDir, Now: g.clock.Now,
		Objects: func(context.Context, manifest.Destination) (objectstore.Store, error) {
			var s objectstore.Store = g.objects
			for _, w := range wrap {
				s = w(s)
			}
			return s, nil
		}}
}

func (g *rig) scheduler(r *Runner, store *state.Store) *Scheduler {
	return &Scheduler{Env: "prod", Store: store, Runner: r, Now: g.clock.Now,
		Manifest: func(context.Context) (manifest.Manifest, error) { return g.m, nil }}
}

// run takes one snapshot now and returns its run record.
func (g *rig) run(trigger string) state.StateBackupRun {
	g.t.Helper()
	p, err := g.runner.Begin(context.Background(), g.m, trigger, time.Time{})
	if err != nil {
		g.t.Fatalf("begin: %v", err)
	}
	return p.Execute(context.Background()).Run
}

// objectsUnder lists the keys in the bucket under prefix, sorted.
func (g *rig) objectsUnder(prefix string) []string {
	g.t.Helper()
	list, err := g.objects.List(context.Background(), prefix)
	if err != nil {
		g.t.Fatal(err)
	}
	var keys []string
	for _, o := range list {
		keys = append(keys, o.Key)
	}
	slices.Sort(keys)
	return keys
}

func (g *rig) read(key string) []byte {
	g.t.Helper()
	rc, _, err := g.objects.Get(context.Background(), key)
	if err != nil {
		g.t.Fatalf("get %s: %v", key, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		g.t.Fatal(err)
	}
	return data
}

// faulty fails the uploads whose key ends with suffix, after reading part of the body, as a dropped connection would.
type faulty struct {
	objectstore.Store
	suffix string
}

func (f faulty) Put(ctx context.Context, key string, r io.Reader, meta map[string]string) (int64, error) {
	if strings.HasSuffix(key, f.suffix) {
		io.CopyN(io.Discard, r, 16)
		return 0, errors.New("upload failed: injected fault")
	}
	return f.Store.Put(ctx, key, r, meta)
}

func TestASnapshotIsUploadedWithItsSidecarAndNoKeyOrPlaintext(t *testing.T) {
	g := newRig(t, 14)
	run := g.run("manual")
	if run.Status != "succeeded" || run.Key != "berthy-state/prod/20261116T100030Z.db" {
		t.Fatalf("run: %+v", run)
	}
	body := g.read(run.Key)
	if bytes.Contains(body, []byte(testValue)) || bytes.Contains(body, g.key) {
		t.Fatal("the snapshot holds the key or a plaintext secret")
	}
	var sc Sidecar
	if err := json.Unmarshal(g.read("berthy-state/prod/20261116T100030Z.json"), &sc); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if sc.Format != SidecarFormat || sc.Environment != "prod" || sc.SchemaVersion != 12 || sc.Size != int64(len(body)) || sc.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("sidecar %+v for %d bytes", sc, len(body))
	}
	if run.Size != sc.Size || run.SHA256 != sc.SHA256 {
		t.Fatalf("the run record disagrees with the sidecar: %+v", run)
	}
	entries, _ := os.ReadDir(g.stateDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "snapshot-") {
			t.Fatalf("the temporary snapshot was left behind: %s", e.Name())
		}
	}
}

func TestAFailedUploadLeavesNoSidecarAndNoRunSuccess(t *testing.T) {
	g := newRig(t, 14)
	g.runner = g.newRunner(g.store, func(s objectstore.Store) objectstore.Store { return faulty{Store: s, suffix: ".db"} })
	run := g.run("manual")
	if run.Status != "failed" || !strings.HasPrefix(run.Detail, "upload failed") {
		t.Fatalf("failed upload recorded as %+v", run)
	}
	if keys := g.objectsUnder("berthy-state/prod/"); len(keys) != 0 {
		t.Fatalf("objects after a failed upload: %v", keys)
	}
	h, err := g.store.StateBackupHistory(context.Background())
	if err != nil || h.Success != nil || h.Last == nil || h.Last.Status != "failed" {
		t.Fatalf("history: %+v %v", h, err)
	}
}

func TestAFailedSidecarRemovesItsSnapshot(t *testing.T) {
	g := newRig(t, 14)
	g.runner = g.newRunner(g.store, func(s objectstore.Store) objectstore.Store { return faulty{Store: s, suffix: ".json"} })
	run := g.run("manual")
	if run.Status != "failed" || !strings.HasPrefix(run.Detail, "sidecar failed") {
		t.Fatalf("failed sidecar recorded as %+v", run)
	}
	if keys := g.objectsUnder("berthy-state/prod/"); len(keys) != 0 {
		t.Fatalf("objects after a failed sidecar: %v", keys)
	}
}

func TestRetentionKeepsTheNewestSnapshotsAndNeverTouchesForeignObjects(t *testing.T) {
	g := newRig(t, 2)
	ctx := context.Background()
	for _, key := range []string{"berthy-state/prod/notes.txt", "berthy-state/prod/junk.json"} {
		if _, err := g.objects.Put(ctx, key, strings.NewReader("not a sidecar"), nil); err != nil {
			t.Fatal(err)
		}
	}
	var stamps []string
	for range 3 {
		run := g.run("manual")
		if run.Status != "succeeded" {
			t.Fatalf("run: %+v", run)
		}
		stamps = append(stamps, strings.TrimSuffix(run.Key, ".db"))
		g.clock.Add(time.Minute)
	}
	want := []string{"berthy-state/prod/junk.json", "berthy-state/prod/notes.txt",
		stamps[1] + ".db", stamps[1] + ".json", stamps[2] + ".db", stamps[2] + ".json"}
	slices.Sort(want)
	if got := g.objectsUnder("berthy-state/prod/"); !slices.Equal(got, want) {
		t.Fatalf("after retention keep 2:\n got %v\nwant %v", got, want)
	}
}

func TestSchedulerRunsEachOccurrenceOnceAndNotAgainAfterARestart(t *testing.T) {
	g := newRig(t, 14)
	ctx := context.Background()
	g.m.StateBackup.Schedule = "* * * * *"
	s := g.scheduler(g.runner, g.store)
	s.Step(ctx)
	s.Wait()
	s.Step(ctx) // the same minute again
	s.Wait()
	g.clock.Add(30 * time.Second) // 10:01:00
	s.Step(ctx)
	s.Wait()
	// The agent restarts within the minute: a new runner and scheduler on the same state find the occurrence recorded.
	restarted := g.scheduler(g.newRunner(g.store), g.store)
	restarted.Step(ctx)
	restarted.Wait()
	runs, err := g.store.StateBackupRuns(ctx, 10)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs: %+v %v", runs, err)
	}
	if runs[0].Trigger != "schedule" || !runs[0].Scheduled.Equal(time.Date(2026, 11, 16, 10, 1, 0, 0, time.UTC)) ||
		!runs[1].Scheduled.Equal(time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("occurrences: %+v", runs)
	}
	audit, err := g.store.AuditLog(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := map[string]int{}
	for _, a := range audit {
		if a.ActorID == ScheduledActor && a.Action == "state_backup" {
			outcomes[a.Outcome]++
		}
	}
	if outcomes["started"] != 2 || outcomes["succeeded"] != 2 {
		t.Fatalf("scheduled runs in the audit log: %v", outcomes)
	}
}

func TestSchedulerLeavesAnUnmatchedMinuteAlone(t *testing.T) {
	g := newRig(t, 14) // the default schedule runs at 03:17 UTC, not at 10:00
	ctx := context.Background()
	s := g.scheduler(g.runner, g.store)
	s.Step(ctx)
	s.Wait()
	if runs, _ := g.store.StateBackupRuns(ctx, 10); len(runs) != 0 {
		t.Fatalf("a run at an unmatched minute: %+v", runs)
	}
}

func TestAStateBackupAlreadyRunningRefusesAnotherAndNotConfiguredRefusesAll(t *testing.T) {
	g := newRig(t, 14)
	ctx := context.Background()
	held, err := g.runner.Begin(ctx, g.m, "manual", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = g.runner.Begin(ctx, g.m, "manual", time.Time{}); !errors.Is(err, ErrRunning) {
		t.Fatalf("second begin while one runs: %v", err)
	}
	if res := held.Execute(ctx); res.Run.Status != "succeeded" {
		t.Fatalf("held run: %+v", res.Run)
	}
	g.m.StateBackup = nil
	if _, err = g.runner.Begin(ctx, g.m, "manual", time.Time{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("begin without the block: %v", err)
	}
}

func TestAScheduledRunThatCannotUseItsStorageIsRecordedAsFailed(t *testing.T) {
	g := newRig(t, 14)
	ctx := context.Background()
	g.m.StateBackup.Schedule = "* * * * *"
	g.runner.Objects = func(context.Context, manifest.Destination) (objectstore.Store, error) { return nil, ErrCredentials }
	s := g.scheduler(g.runner, g.store)
	s.Step(ctx)
	s.Wait()
	h, err := g.store.StateBackupHistory(ctx)
	if err != nil || h.Last == nil || h.Last.Status != "failed" || h.Last.Detail != ErrCredentials.Error() || h.Last.Trigger != "schedule" {
		t.Fatalf("refused scheduled run: %+v %v", h.Last, err)
	}
	if keys := g.objectsUnder("berthy-state/prod/"); len(keys) != 0 {
		t.Fatalf("objects written by a refused run: %v", keys)
	}
}

// unreachable fails every request, as a storage endpoint that is down does.
type unreachable struct{ objectstore.Store }

func (unreachable) Stat(context.Context, string) (objectstore.ObjectInfo, error) {
	return objectstore.ObjectInfo{}, errors.New("object storage unavailable")
}

func TestAScheduledRunWhoseStorageIsUnreachableIsRecordedAsFailed(t *testing.T) {
	g := newRig(t, 14)
	ctx := context.Background()
	g.m.StateBackup.Schedule = "* * * * *"
	g.runner = g.newRunner(g.store, func(s objectstore.Store) objectstore.Store { return unreachable{Store: s} })
	s := g.scheduler(g.runner, g.store)
	s.Step(ctx)
	s.Wait()
	h, err := g.store.StateBackupHistory(ctx)
	if err != nil || h.Last == nil || h.Last.Status != "failed" || h.Last.Detail != ErrStorage.Error() {
		t.Fatalf("unreachable storage: %+v %v", h.Last, err)
	}
}

func TestRestoreRebuildsTheStateFromStorageWithTheKey(t *testing.T) {
	g := newRig(t, 14)
	ctx := context.Background()
	run := g.run("manual")
	out := filepath.Join(t.TempDir(), "restored")
	rep, err := Restore(ctx, g.restoreOptions(run.Key, out, g.keyPath))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if rep.Environment != "prod" || rep.SchemaVersion != 12 || !slices.Equal(rep.Secrets, []string{"DB_PASSWORD"}) || rep.Path != filepath.Join(out, "state.db") {
		t.Fatalf("report: %+v", rep)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(rep.Path); info.Mode().Perm() != 0600 {
			t.Fatalf("restored mode %v", info.Mode().Perm())
		}
	}
	restored, err := state.Open(out, g.keyPath, "prod")
	if err != nil {
		t.Fatalf("the restored state does not open: %v", err)
	}
	defer restored.Close()
	if _, value, err := restored.Secret(ctx, "DB_PASSWORD"); err != nil || string(value) != testValue {
		t.Fatalf("restored secret: %q %v", value, err)
	}
}

func TestRestoreWithTheWrongKeyFailsAndLeavesNothing(t *testing.T) {
	g := newRig(t, 14)
	run := g.run("manual")
	other := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(other, bytes.Repeat([]byte{5}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "restored")
	if _, err := Restore(context.Background(), g.restoreOptions(run.Key, out, other)); !errors.Is(err, state.ErrWrongKey) {
		t.Fatalf("wrong key: %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("the output directory survived a failed restore: %v", err)
	}
}

func TestRestoreRefusesAChecksumMismatch(t *testing.T) {
	g := newRig(t, 14)
	run := g.run("manual")
	if _, err := g.objects.Put(context.Background(), run.Key, bytes.NewReader([]byte("tampered snapshot")), nil); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "restored")
	if _, err := Restore(context.Background(), g.restoreOptions(run.Key, out, g.keyPath)); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("tampered snapshot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "state.db")); !os.IsNotExist(err) {
		t.Fatalf("a mismatched snapshot was kept: %v", err)
	}
}

func TestRestoreRefusesANonEmptyDirectory(t *testing.T) {
	g := newRig(t, 14)
	run := g.run("manual")
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "keep.txt"), []byte("mine"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), g.restoreOptions(run.Key, out, g.keyPath)); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("non-empty directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "state.db")); !os.IsNotExist(err) {
		t.Fatal("a restore wrote into a non-empty directory")
	}
	if data, _ := os.ReadFile(filepath.Join(out, "keep.txt")); string(data) != "mine" {
		t.Fatal("the existing file changed")
	}
}

func TestRestoreNeedsTheSidecarAndADatabaseKey(t *testing.T) {
	g := newRig(t, 14)
	run := g.run("manual")
	if err := g.objects.Delete(context.Background(), strings.TrimSuffix(run.Key, ".db")+".json"); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "restored")
	if _, err := Restore(context.Background(), g.restoreOptions(run.Key, out, g.keyPath)); !errors.Is(err, ErrNoSidecar) {
		t.Fatalf("snapshot without a sidecar: %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("a restore without a sidecar created its directory")
	}
	if _, err := Restore(context.Background(), g.restoreOptions("berthy-state/prod/notes.txt", out, g.keyPath)); err == nil {
		t.Fatal("an object that is not a snapshot was restored")
	}
}

// restoreOptions points a restore at the rig's bucket with its credentials.
func (g *rig) restoreOptions(object, out, keyPath string) RestoreOptions {
	return RestoreOptions{Endpoint: g.srv.URL, Bucket: "bkt", Object: object, Out: out, KeyFile: keyPath, AccessKey: testAccess, SecretKey: testSecret}
}
