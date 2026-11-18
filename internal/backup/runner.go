package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"berthy/internal/api"
	"berthy/internal/jobs"
	"berthy/internal/manifest"
	"berthy/internal/objectstore"
	"berthy/internal/state"
)

// SidecarFormat names the sidecar document and the archive it describes: a gzip-compressed tar of a volume.
const SidecarFormat = "berthy-volume-tar-gzip-1"

const (
	archiveSuffix = ".tar.gz"
	sidecarSuffix = ".json"
	stampLayout   = "20060102T150405Z"
	// maxSidecar bounds the sidecar document that is read back. A real one is a few hundred bytes.
	maxSidecar = 64 << 10
	// finishTimeout bounds recording a run's outcome, which must happen even when the run's context has ended.
	finishTimeout = 15 * time.Second
	// cleanupLimit bounds the removal of an archive whose sidecar could not be written.
	cleanupLimit = 30 * time.Second
)

// Refusal is a reason a request is refused, worded for the caller. It never contains a secret, so it may be shown as it is.
type Refusal string

func (r Refusal) Error() string { return string(r) }

var (
	// ErrBackupRunning means a backup of the same Docker volume is running. Backups of one volume run one at a time.
	ErrBackupRunning = Refusal("a backup of this volume is already running")
	// ErrUnknownBackup means the manifest of the deployed apps has no backup of that name.
	ErrUnknownBackup = Refusal("unknown backup")
	// ErrCredentials means the destination's access key or secret key cannot be read from the secret store.
	ErrCredentials = Refusal("backup credentials unavailable")
	// ErrStorage means the destination could not be reached with the settings and credentials at hand.
	ErrStorage = Refusal("backup storage unavailable")
	// ErrNotFound means no backup in storage has that key, or it has no sidecar, which makes it not restorable.
	ErrNotFound = Refusal("backup not found")
	// ErrInvalidSidecar means the sidecar of a backup is not a document of this version for this environment.
	ErrInvalidSidecar = Refusal("backup sidecar invalid")
	// ErrVolumeName means the name of the volume to restore into is not a Docker volume name.
	ErrVolumeName = Refusal("invalid volume name")
	// ErrVolumeExists means the volume to restore into exists. A restore never writes into existing data.
	ErrVolumeExists = Refusal("target volume already exists; choose an unused name")
	// ErrNotMounted is what a Resolve reports for a volume the app does not mount. The runner words the refusal.
	ErrNotMounted = errors.New("volume is not mounted by the app")
	// ErrChecksumMismatch means what a restore read from storage is not what the backup recorded. The new volume is removed.
	ErrChecksumMismatch = errors.New("checksum mismatch")
)

// refusals are the fixed texts that may be shown for any request, such as the text of a failed restore operation.
var refusals = []Refusal{ErrBackupRunning, ErrUnknownBackup, ErrCredentials, ErrStorage, ErrNotFound, ErrInvalidSidecar, ErrVolumeName, ErrVolumeExists}

// PublicRefusal reports whether s is one of the fixed refusal texts, which hold no names and so may be shown.
func PublicRefusal(s string) bool { return slices.Contains(refusals, Refusal(s)) }

// JobHolder runs the job that prepares a volume for its backup.
type JobHolder interface {
	// RunHeld waits for any active run of the job to finish, runs it once (trigger "backup"), and keeps the job marked
	// running until release is called, so scheduled occurrences in between are recorded as skipped.
	RunHeld(ctx context.Context, name string) (state.JobRun, func(), error)
}

// Runner backs up the volumes the manifest names, lists what is in storage and restores it. Its fields are set before use.
type Runner struct {
	Store    *state.Store
	Archiver *Archiver
	// Objects builds the client of a destination, with its credentials. It returns ErrCredentials when they are missing.
	Objects func(ctx context.Context, d manifest.Destination) (objectstore.Store, error)
	// Jobs runs a backup's `before` job. It is nil when the agent has no scheduler.
	Jobs JobHolder
	// Resolve returns the Docker name of the volume a backup names, or an error that wraps ErrNotMounted.
	Resolve func(ctx context.Context, m manifest.Manifest, b manifest.Backup) (dockerVolume string, err error)
	Env     string
	Now     func() time.Time
	Log     func(string)

	mu     sync.Mutex
	active map[string]bool // Docker volume -> a backup of it is running; one at a time per volume
}

// Sidecar is the document stored beside each archive. It is written only after the archive was uploaded, so a backup that
// has one is complete.
type Sidecar struct {
	Format       string    `json:"format"`
	Environment  string    `json:"environment"`
	Backup       string    `json:"backup"`
	App          string    `json:"app"`
	Volume       string    `json:"volume"`
	DockerVolume string    `json:"docker_volume"`
	Size         int64     `json:"size"`
	SHA256       string    `json:"sha256"`
	Started      time.Time `json:"started"`
	Finished     time.Time `json:"finished"`
	RunID        int64     `json:"run_id"`
}

// Result is the outcome of a run that was recorded. Key is the archive's object key. The run's Status says whether it worked.
type Result struct {
	Run state.BackupRun
	Key string
}

var (
	sha256Hex  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	dirSegment = regexp.MustCompile(`^[A-Za-z0-9._@+=-]+$`)
	volumeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$`)
)

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(fmt.Sprintf(format, args...))
	}
}

// objectDir is the prefix of a backup's objects, without a trailing slash: the manifest's prefix or <app>/<volume>, then the
// backup's name.
func objectDir(name string, b manifest.Backup) (string, error) {
	base := strings.Trim(b.Prefix, "/")
	if base == "" {
		base = b.App + "/" + b.Volume
	}
	for _, seg := range strings.Split(base+"/"+name, "/") {
		if seg == "." || seg == ".." || !dirSegment.MatchString(seg) {
			return "", Refusal(fmt.Sprintf("backup %s: invalid prefix", name))
		}
	}
	return base + "/" + name, nil
}

func archiveKey(dir string, started time.Time, runID int64) string {
	return fmt.Sprintf("%s/%s-%d%s", dir, started.UTC().Format(stampLayout), runID, archiveSuffix)
}

func sidecarKeyOf(archive string) string {
	return strings.TrimSuffix(archive, archiveSuffix) + sidecarSuffix
}
func archiveKeyOf(sidecar string) string {
	return strings.TrimSuffix(sidecar, sidecarSuffix) + archiveSuffix
}

// store builds the client of the backup's destination. Whatever goes wrong without being a refusal is reported as storage
// being unavailable, so no detail of the configuration leaks.
func (r *Runner) store(ctx context.Context, m manifest.Manifest, name string, b manifest.Backup) (objectstore.Store, error) {
	dest, ok := m.Destinations[b.Destination]
	if !ok {
		return nil, Refusal(fmt.Sprintf("backup %s: unknown destination", name))
	}
	store, err := r.Objects(ctx, dest)
	var refusal Refusal
	switch {
	case errors.As(err, &refusal):
		return nil, refusal
	case err != nil:
		return nil, ErrStorage
	}
	return store, nil
}

// Pending is a backup that has been admitted and recorded as running. It holds its volume until Execute returns.
type Pending struct {
	r            *Runner
	run          state.BackupRun
	backup       manifest.Backup
	store        objectstore.Store
	dockerVolume string
	dir          string
	key          string
}

// ID is the run's ID, which `backup run` and `backup runs` show.
func (p *Pending) ID() int64 { return p.run.ID }

// Run performs one backup and returns when it has ended. trigger is "schedule", "manual" or "hook". An error means no run
// was recorded: the backup is unknown or cannot start, or a backup of its volume is running. Once a run is recorded the
// outcome is in Result.Run, a failed run included.
func (r *Runner) Run(ctx context.Context, m manifest.Manifest, name, trigger string) (Result, error) {
	p, err := r.Begin(ctx, m, name, trigger)
	if err != nil {
		return Result{}, err
	}
	return p.Execute(ctx), nil
}

// Begin checks that the backup can run and records it as running, so its ID is known before the work starts. It refuses
// without recording anything when the backup is unknown, its volume cannot be resolved, its destination cannot be reached
// with the credentials at hand, or a backup of the same Docker volume is running. The caller must call Execute.
func (r *Runner) Begin(ctx context.Context, m manifest.Manifest, name, trigger string) (*Pending, error) {
	return r.BeginAt(ctx, m, name, trigger, time.Time{})
}

// BeginAt is Begin for a run that belongs to a cron occurrence: scheduled is the UTC minute of a "schedule" run, and is zero
// for every other trigger. The occurrence is recorded once, so a second BeginAt for it returns state.ErrConflict, even from
// another process. A refusal after which the backup is known and cannot run, such as credentials that are gone, is recorded
// as a failed run unless the trigger is "manual", because nobody is waiting to read it: the backup's check shows it.
func (r *Runner) BeginAt(ctx context.Context, m manifest.Manifest, name, trigger string, scheduled time.Time) (*Pending, error) {
	p, err := r.begin(ctx, m, name, trigger, scheduled)
	var refusal Refusal
	if err != nil && trigger != "manual" && errors.As(err, &refusal) && !errors.Is(err, ErrBackupRunning) && !errors.Is(err, ErrUnknownBackup) {
		if recErr := r.recordRefused(ctx, m.Backups[name], name, trigger, scheduled, refusal); recErr != nil {
			return nil, recErr
		}
	}
	return p, err
}

// recordRefused records a run that could not start, already failed, so it shows in the runs and in the backup's check.
func (r *Runner) recordRefused(ctx context.Context, b manifest.Backup, name, trigger string, scheduled time.Time, why Refusal) error {
	var token [8]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	id, err := r.Store.StartBackup(ctx, state.BackupRun{Backup: name, App: b.App, Volume: b.Volume, Destination: b.Destination, Trigger: trigger,
		Scheduled: scheduled, Started: r.now(), ObjectKey: name + "/refused-" + hex.EncodeToString(token[:])})
	if err != nil {
		return err
	}
	r.finish(id, "failed", 0, "", why.Error())
	return nil
}

func (r *Runner) begin(ctx context.Context, m manifest.Manifest, name, trigger string, scheduled time.Time) (*Pending, error) {
	b, ok := m.Backups[name]
	if !ok {
		return nil, ErrUnknownBackup
	}
	dir, err := objectDir(name, b)
	if err != nil {
		return nil, err
	}
	dockerVolume, err := r.Resolve(ctx, m, b)
	switch {
	case errors.Is(err, ErrNotMounted):
		return nil, Refusal(fmt.Sprintf("backup %s: volume %s is not mounted by app %s", name, b.Volume, b.App))
	case err != nil:
		return nil, Refusal(fmt.Sprintf("backup %s: volume lookup failed", name))
	}
	store, err := r.store(ctx, m, name, b)
	if err != nil {
		return nil, err
	}
	if !r.claim(dockerVolume) {
		return nil, ErrBackupRunning
	}
	// The final key names the run's own ID, so the run is recorded under a provisional key and renamed at once.
	var token [8]byte
	if _, err = rand.Read(token[:]); err != nil {
		r.unclaim(dockerVolume)
		return nil, err
	}
	started := r.now()
	id, err := r.Store.StartBackup(ctx, state.BackupRun{
		Backup: name, App: b.App, Volume: b.Volume, Destination: b.Destination, Trigger: trigger, Scheduled: scheduled, Started: started,
		ObjectKey: dir + "/starting-" + hex.EncodeToString(token[:]),
	})
	if err != nil {
		r.unclaim(dockerVolume)
		return nil, err
	}
	key := archiveKey(dir, started, id)
	if err = r.Store.SetBackupObjectKey(ctx, id, key); err != nil {
		r.finish(id, "failed", 0, "", "state unavailable")
		r.unclaim(dockerVolume)
		return nil, err
	}
	run := state.BackupRun{ID: id, Backup: name, App: b.App, Volume: b.Volume, Destination: b.Destination, ObjectKey: key, Trigger: trigger, Scheduled: scheduled, Started: started, Status: "running"}
	return &Pending{r: r, run: run, backup: b, store: store, dockerVolume: dockerVolume, dir: dir, key: key}, nil
}

func (r *Runner) claim(volume string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active[volume] {
		return false
	}
	if r.active == nil {
		r.active = map[string]bool{}
	}
	r.active[volume] = true
	return true
}

func (r *Runner) unclaim(volume string) {
	r.mu.Lock()
	delete(r.active, volume)
	r.mu.Unlock()
}

// finish records the outcome of a run, even when the agent is stopping.
func (r *Runner) finish(id int64, status string, size int64, sum, detail string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), finishTimeout)
	defer cancel()
	if err := r.Store.FinishBackup(ctx, id, status, size, sum, detail); err != nil {
		r.logf("backup: cannot record the result of run %d: %v", id, err)
		return false
	}
	return true
}

// Execute does the backup: it runs the `before` job, streams the volume's archive to storage, writes the sidecar, records
// the outcome and applies retention. A failure at any step leaves no sidecar, and the run is recorded as failed with a
// detail that names the step. A ctx that ends mid-run records "agent stopped during the backup".
func (p *Pending) Execute(ctx context.Context) Result {
	defer p.r.unclaim(p.dockerVolume)
	run := p.run
	fail := func(stage string, cause error) Result {
		detail := stage
		switch {
		case ctx.Err() != nil:
			detail = state.StoppedBackupDetail
		case cause != nil && strings.HasPrefix(cause.Error(), stage):
			// The object client words its own failures as "upload failed: ...".
			detail = cause.Error()
		case cause != nil:
			detail += ": " + cause.Error()
		}
		p.r.finish(run.ID, "failed", 0, "", detail)
		run.Status, run.Detail, run.Finished = "failed", detail, p.r.now()
		return Result{Run: run, Key: p.key}
	}
	release := func() {}
	if p.backup.Before != "" {
		rel, err := p.prepare(ctx)
		if err != nil {
			return fail("preparation failed", err)
		}
		release = rel
	}
	size, sum, stage, err := p.transfer(ctx)
	release()
	if err != nil {
		return fail(stage, err)
	}
	finished := p.r.now()
	sidecar := Sidecar{Format: SidecarFormat, Environment: p.r.Env, Backup: run.Backup, App: run.App, Volume: run.Volume, DockerVolume: p.dockerVolume,
		Size: size, SHA256: sum, Started: run.Started, Finished: finished, RunID: run.ID}
	if err = p.writeSidecar(ctx, sidecar); err != nil {
		p.discardArchive()
		return fail("sidecar failed", err)
	}
	run.Status, run.Size, run.SHA256, run.Finished = "succeeded", size, sum, finished
	p.r.finish(run.ID, "succeeded", size, sum, "")
	p.r.retain(ctx, p.store, run.Backup, p.dir, p.backup)
	return Result{Run: run, Key: p.key}
}

// prepare runs the backup's `before` job and holds it. A job that does not succeed fails the preparation, and releases the job.
func (p *Pending) prepare(ctx context.Context) (func(), error) {
	if p.r.Jobs == nil {
		return nil, errors.New("jobs unavailable")
	}
	job, release, err := p.r.Jobs.RunHeld(ctx, p.backup.Before)
	if err != nil {
		if ctx.Err() == nil {
			p.r.logf("backup %s: preparation job %s could not run: %v", p.run.Backup, p.backup.Before, err)
		}
		return nil, errors.New("job could not run")
	}
	if job.Status != "succeeded" {
		release()
		detail := job.Detail
		if detail == "" {
			detail = job.Status
		}
		return nil, errors.New(detail + jobs.LogsHint(p.backup.Before, job))
	}
	return release, nil
}

// hashingWriter passes writes on, and counts and hashes what was accepted.
type hashingWriter struct {
	w io.Writer
	h io.Writer
	n int64
}

func (w *hashingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.h.Write(p[:n])
	w.n += int64(n)
	return n, err
}

// transfer streams the volume's archive into storage through a pipe, hashing it on the way. Whichever side fails first
// ends the other, and its stage is the one reported.
func (p *Pending) transfer(ctx context.Context) (size int64, sum, stage string, err error) {
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	hash := sha256.New()
	hw := &hashingWriter{w: pw, h: hash}
	var mu sync.Mutex
	var firstStage string
	var firstErr error
	record := func(stage string, err error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstStage, firstErr = stage, err
		}
	}
	archived := make(chan struct{})
	go func() {
		defer close(archived)
		_, aerr := p.r.Archiver.Archive(tctx, p.dockerVolume, hw)
		if aerr != nil {
			record("archive failed", aerr)
		}
		pw.CloseWithError(aerr)
	}()
	uploaded, perr := p.store.Put(tctx, p.key, pr, nil)
	if perr != nil {
		record("upload failed", perr)
		pr.CloseWithError(perr)
		cancel()
	}
	<-archived
	mu.Lock()
	defer mu.Unlock()
	if firstErr != nil {
		return 0, "", firstStage, firstErr
	}
	if uploaded != hw.n {
		return 0, "", "upload failed", errors.New("stored size differs from the archive")
	}
	return hw.n, hex.EncodeToString(hash.Sum(nil)), "", nil
}

func (p *Pending) writeSidecar(ctx context.Context, s Sidecar) error {
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = p.store.Put(ctx, sidecarKeyOf(p.key), bytes.NewReader(body), nil)
	return err
}

// discardArchive removes the archive of a backup whose sidecar could not be written. Without a sidecar it is never listed
// and never restorable, so this only gives the space back.
func (p *Pending) discardArchive() {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupLimit)
	defer cancel()
	if err := p.store.Delete(ctx, p.key); err != nil {
		p.r.logf("backup %s: archive %s was not removed after its sidecar failed: %v", p.run.Backup, p.key, err)
	}
}

// keepCount is how many successful backups a backup retains; zero keeps all of them.
func keepCount(b manifest.Backup) int {
	if b.Retention == nil {
		return 0
	}
	switch v := b.Retention.Keep.(type) {
	case int:
		return max(v, 0)
	case int64:
		return int(max(v, 0))
	case float64:
		return int(max(v, 0))
	}
	return 0
}

// stored is a backup of storage, read from its sidecar.
type stored struct {
	sidecarKey string
	archiveKey string
	Sidecar
}

// sidecars reads the sidecars that name this backup in its own directory, newest first. Objects that are not a valid sidecar
// of this backup and environment are skipped, so nothing else in the bucket is ever mistaken for a backup of this one.
func (r *Runner) sidecars(ctx context.Context, store objectstore.Store, name, dir string) ([]stored, error) {
	objects, err := store.List(ctx, dir+"/")
	if err != nil {
		return nil, err
	}
	var out []stored
	for _, o := range objects {
		rest := strings.TrimPrefix(o.Key, dir+"/")
		if strings.Contains(rest, "/") || !strings.HasSuffix(rest, sidecarSuffix) || o.Size > maxSidecar {
			continue
		}
		sc, err := r.readSidecar(ctx, store, o.Key)
		switch {
		case errors.Is(err, objectstore.ErrNotFound), errors.Is(err, ErrInvalidSidecar):
			continue
		case err != nil:
			return nil, err
		case sc.Backup != name:
			continue
		}
		out = append(out, stored{sidecarKey: o.Key, archiveKey: archiveKeyOf(o.Key), Sidecar: sc})
	}
	slices.SortFunc(out, func(a, b stored) int {
		if c := b.Finished.Compare(a.Finished); c != 0 {
			return c
		}
		return strings.Compare(b.sidecarKey, a.sidecarKey)
	})
	return out, nil
}

// readSidecar reads and checks one sidecar. A document that is not of this format and environment is ErrInvalidSidecar.
func (r *Runner) readSidecar(ctx context.Context, store objectstore.Store, key string) (Sidecar, error) {
	rc, _, err := store.Get(ctx, key)
	if err != nil {
		return Sidecar{}, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxSidecar+1))
	if err != nil {
		return Sidecar{}, err
	}
	var sc Sidecar
	if len(data) > maxSidecar || json.Unmarshal(data, &sc) != nil || sc.Format != SidecarFormat || sc.Environment != r.Env ||
		sc.Backup == "" || sc.Size < 0 || !sha256Hex.MatchString(sc.SHA256) {
		return Sidecar{}, ErrInvalidSidecar
	}
	return sc, nil
}

// retain deletes the backups of storage beyond the number the backup keeps: the oldest successful ones, archive first and
// then sidecar. Only what has a sidecar of this backup is touched. A failure is logged and never fails the run.
func (r *Runner) retain(ctx context.Context, store objectstore.Store, name, dir string, b manifest.Backup) {
	keep := keepCount(b)
	if keep == 0 {
		return
	}
	// The run has ended; retention is still done when the agent is stopping, within a bound.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupLimit)
	defer cancel()
	all, err := r.sidecars(ctx, store, name, dir)
	if err != nil {
		r.logf("backup %s: retention could not list storage: %v", name, err)
		return
	}
	if len(all) <= keep {
		return
	}
	for _, old := range all[keep:] {
		if err = store.Delete(ctx, old.archiveKey); err != nil {
			r.logf("backup %s: retention could not delete %s: %v", name, old.archiveKey, err)
			continue
		}
		if err = store.Delete(ctx, old.sidecarKey); err != nil {
			r.logf("backup %s: retention could not delete %s: %v", name, old.sidecarKey, err)
		}
	}
}

// List returns the backups in storage for every backup of the manifest, or only name when it is not empty, newest first. It
// reads sidecars: an archive without one is not listed.
func (r *Runner) List(ctx context.Context, m manifest.Manifest, name string) ([]api.BackupEntry, error) {
	if name != "" {
		if _, ok := m.Backups[name]; !ok {
			return nil, ErrUnknownBackup
		}
	}
	out := []api.BackupEntry{}
	for _, n := range slices.Sorted(maps.Keys(m.Backups)) {
		if name != "" && n != name {
			continue
		}
		b := m.Backups[n]
		dir, err := objectDir(n, b)
		if err != nil {
			return nil, err
		}
		store, err := r.store(ctx, m, n, b)
		if err != nil {
			return nil, err
		}
		list, err := r.sidecars(ctx, store, n, dir)
		if err != nil {
			r.logf("backup %s: listing storage failed: %v", n, err)
			return nil, ErrStorage
		}
		for _, s := range list {
			out = append(out, api.BackupEntry{Key: s.archiveKey, Backup: s.Backup, App: s.App, Volume: s.Volume, Size: s.Size, SHA256: s.SHA256, Finished: s.Finished})
		}
	}
	slices.SortStableFunc(out, func(a, b api.BackupEntry) int { return b.Finished.Compare(a.Finished) })
	return out, nil
}

// located is a backup found in storage by its key.
type located struct {
	store   objectstore.Store
	sidecar Sidecar
}

// Check validates a restore without changing anything: the key names an archive in storage whose sidecar exists and parses,
// and the volume to restore into has a valid name and does not exist.
func (r *Runner) Check(ctx context.Context, m manifest.Manifest, key, into string) (Sidecar, error) {
	loc, err := r.locate(ctx, m, key)
	if err != nil {
		return Sidecar{}, err
	}
	if err = r.checkTarget(ctx, into); err != nil {
		return Sidecar{}, err
	}
	return loc.sidecar, nil
}

func (r *Runner) checkTarget(ctx context.Context, into string) error {
	if !volumeName.MatchString(into) {
		return ErrVolumeName
	}
	exists, err := r.Archiver.Engine.VolumeExists(ctx, into)
	if err != nil {
		return Refusal("volume lookup failed")
	}
	if exists {
		return ErrVolumeExists
	}
	return nil
}

// locate finds the backup whose archive has this key: the key must sit directly in the directory of one of the manifest's
// backups and its sidecar must be there and valid for that backup.
func (r *Runner) locate(ctx context.Context, m manifest.Manifest, key string) (located, error) {
	sidecar := sidecarKeyOf(key)
	if !strings.HasSuffix(key, archiveSuffix) || sidecar == key {
		return located{}, ErrNotFound
	}
	for _, name := range slices.Sorted(maps.Keys(m.Backups)) {
		b := m.Backups[name]
		dir, err := objectDir(name, b)
		if err != nil {
			continue
		}
		rest := strings.TrimPrefix(key, dir+"/")
		if rest == key || strings.Contains(rest, "/") {
			continue
		}
		store, err := r.store(ctx, m, name, b)
		if err != nil {
			return located{}, err
		}
		sc, err := r.readSidecar(ctx, store, sidecar)
		switch {
		case errors.Is(err, objectstore.ErrNotFound):
			continue
		case errors.Is(err, ErrInvalidSidecar):
			return located{}, ErrInvalidSidecar
		case err != nil:
			r.logf("backup %s: reading the sidecar of %s failed: %v", name, key, err)
			return located{}, ErrStorage
		case sc.Backup != name:
			return located{}, ErrInvalidSidecar
		}
		return located{store: store, sidecar: sc}, nil
	}
	return located{}, ErrNotFound
}

// countingHash counts and hashes the bytes written to it.
type countingHash struct {
	h io.Writer
	n int64
}

func (c *countingHash) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return c.h.Write(p)
}

// RestoreOperationLabel names the operation that created a restored volume. A recovery removes a volume that carries the ID of
// the interrupted operation, and no other volume.
const RestoreOperationLabel = "berthy.restore-operation"

// Restore fills a new volume from the backup with this key. The volume is labelled with operationID. It refuses a key without a sidecar, and a volume that exists. The
// archive is streamed from storage through a hash, and once the extraction has ended the digest and size are compared with
// the sidecar's: a mismatch is ErrChecksumMismatch and the new volume is removed.
func (r *Runner) Restore(ctx context.Context, m manifest.Manifest, key, intoVolume, operationID string) error {
	loc, err := r.locate(ctx, m, key)
	if err != nil {
		return err
	}
	if err = r.checkTarget(ctx, intoVolume); err != nil {
		return err
	}
	body, _, err := loc.store.Get(ctx, key)
	if errors.Is(err, objectstore.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		r.logf("restore of %s: reading the archive failed: %v", key, err)
		return ErrStorage
	}
	defer body.Close()
	hash := sha256.New()
	counted := &countingHash{h: hash}
	verify := func() error {
		if counted.n != loc.sidecar.Size || hex.EncodeToString(hash.Sum(nil)) != loc.sidecar.SHA256 {
			return ErrChecksumMismatch
		}
		return nil
	}
	labels := map[string]string{"berthy.environment": r.Env, "berthy.restored-from": key, RestoreOperationLabel: operationID}
	return r.Archiver.Restore(ctx, intoVolume, io.TeeReader(body, counted), labels, verify)
}
