// Package statebackup snapshots the agent's SQLite state, uploads each snapshot to object storage beside a sidecar that
// records what was uploaded, and keeps the newest snapshots. The snapshot holds the secrets sealed; the key that opens them is
// never read or uploaded.
package statebackup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"berthy/internal/manifest"
	"berthy/internal/objectstore"
	"berthy/internal/state"
)

// SidecarFormat names the sidecar document and the snapshot it describes.
const SidecarFormat = "berthy-state-sqlite-2"

const (
	snapshotSuffix = ".db"
	sidecarSuffix  = ".json"
	stampLayout    = "20060102T150405Z"
	// maxSidecar bounds the sidecar document that is read back. A real one is a few hundred bytes.
	maxSidecar = 64 << 10
	// finishTimeout bounds recording a run's outcome, which must happen even when the run's context has ended.
	finishTimeout = 15 * time.Second
	// cleanupLimit bounds the removal of a snapshot whose sidecar could not be written, and retention after a run.
	cleanupLimit = 30 * time.Second
)

// Refusal is a reason a request is refused, worded for the caller. It never contains a secret.
type Refusal string

func (r Refusal) Error() string { return string(r) }

var (
	// ErrNotConfigured means the running manifest has no state_backup block.
	ErrNotConfigured = Refusal("state backup is not configured")
	// ErrRunning means a state backup is already running. Runs take one at a time.
	ErrRunning = Refusal("a state backup is already running")
	// ErrCredentials means the destination's credentials cannot be read from the secret store.
	ErrCredentials = Refusal("state backup credentials unavailable")
	// ErrStorage means the destination could not be used with the settings and credentials at hand.
	ErrStorage = Refusal("state backup storage unavailable")
	// ErrChecksumMismatch means a restored snapshot is not what its sidecar recorded.
	ErrChecksumMismatch = Refusal("snapshot does not match its sidecar")
	// ErrNoSidecar means the object has no sidecar, which makes it not restorable.
	ErrNoSidecar = Refusal("snapshot has no sidecar")
	// ErrNotSnapshot means the object name is not the key of a snapshot.
	ErrNotSnapshot = Refusal("object is not a state snapshot")
	// ErrNotEmpty means the directory a restore writes into already holds something.
	ErrNotEmpty = Refusal("output directory is not empty")
	// ErrInvalidSidecar means the sidecar is not a document of this format for an environment.
	ErrInvalidSidecar = Refusal("snapshot sidecar invalid")
)

// Sidecar is the document stored beside each snapshot. It is written only after the snapshot was uploaded. KeyID names the key
// that opens the snapshot: 16 hex characters that identify the key and never reveal it. A restore with another key fails before
// it downloads the snapshot.
type Sidecar struct {
	Format        string    `json:"format"`
	Environment   string    `json:"environment"`
	SchemaVersion int       `json:"schema_version"`
	KeyID         string    `json:"key_id"`
	Size          int64     `json:"size"`
	SHA256        string    `json:"sha256"`
	Created       time.Time `json:"created"`
}

// Schedule returns the cron expression of the state backup, the default when the block leaves it out.
func Schedule(sb manifest.StateBackup) string {
	if sb.Schedule != "" {
		return sb.Schedule
	}
	return manifest.DefaultStateBackupSchedule
}

// Keep returns how many snapshots are retained, the default when the block leaves it out.
func Keep(sb manifest.StateBackup) int {
	if sb.Keep != nil {
		return *sb.Keep
	}
	return manifest.DefaultStateBackupKeep
}

// Prefix returns the object prefix of the snapshots, without a trailing slash. The default depends on the environment.
func Prefix(sb manifest.StateBackup, environment string) string {
	if p := strings.Trim(sb.Prefix, "/"); p != "" {
		return p
	}
	return "berthy-state/" + environment
}

// Result is the outcome of a run that was recorded. The run's Status says whether it worked.
type Result struct {
	Run state.StateBackupRun
}

// Runner takes the snapshots of the state backup. Its fields are set before use.
type Runner struct {
	Store *state.Store
	// Env is the environment the agent serves. The default prefix and the sidecars carry it.
	Env string
	// Dir is a directory the agent owns, in which a snapshot is first written to a private directory of its own.
	Dir string
	// Objects builds the client of a destination. It returns ErrCredentials or ErrStorage, or any other error, which is reported
	// as ErrStorage.
	Objects func(ctx context.Context, d manifest.Destination) (objectstore.Store, error)
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Log receives operational messages. They never contain secrets.
	Log func(string)

	mu   sync.Mutex
	busy bool
}

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

func (r *Runner) claim() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.busy {
		return false
	}
	r.busy = true
	return true
}

func (r *Runner) release() {
	r.mu.Lock()
	r.busy = false
	r.mu.Unlock()
}

// Pending is a snapshot that has been admitted and recorded as running. It holds the runner until Execute returns.
type Pending struct {
	r          *Runner
	run        state.StateBackupRun
	store      objectstore.Store
	prefix     string
	sidecarKey string
	keep       int
}

// ID is the run's ID, which `state-backup` shows.
func (p *Pending) ID() int64 { return p.run.ID }

// Begin checks that a snapshot can be taken and records it as running, so its ID is known before the work starts. It refuses
// without recording anything when the manifest has no state backup or another run is in progress. A refusal of the storage
// settings for a scheduled run is recorded as a failed run, so the check shows it. The caller must call Execute, or the runner
// stays busy.
func (r *Runner) Begin(ctx context.Context, m manifest.Manifest, trigger string, scheduled time.Time) (*Pending, error) {
	if m.StateBackup == nil {
		return nil, ErrNotConfigured
	}
	if !r.claim() {
		return nil, ErrRunning
	}
	p, err := r.begin(ctx, m, *m.StateBackup, trigger, scheduled)
	if err != nil {
		r.release()
	}
	return p, err
}

func (r *Runner) begin(ctx context.Context, m manifest.Manifest, sb manifest.StateBackup, trigger string, scheduled time.Time) (*Pending, error) {
	prefix := Prefix(sb, r.Env)
	started := r.now().Truncate(time.Second)
	dest, ok := m.Destinations[sb.Destination]
	if !ok {
		return nil, ErrNotConfigured
	}
	store, err := r.store(ctx, dest)
	if err != nil {
		return nil, r.refuse(ctx, trigger, scheduled, started, prefix, err)
	}
	// A snapshot is named by its start, and a name that is taken moves on by a second, so no object is overwritten.
	var key, sidecar string
	for {
		stamp := started.Format(stampLayout)
		key, sidecar = prefix+"/"+stamp+snapshotSuffix, prefix+"/"+stamp+sidecarSuffix
		taken, err := exists(ctx, store, key, sidecar)
		if err != nil {
			return nil, r.refuse(ctx, trigger, scheduled, started, prefix, ErrStorage)
		}
		if !taken {
			break
		}
		started = started.Add(time.Second)
	}
	id, err := r.Store.StartStateBackup(ctx, state.StateBackupRun{Trigger: trigger, Scheduled: scheduled, Started: started, Key: key})
	if err != nil {
		return nil, err
	}
	run := state.StateBackupRun{ID: id, Trigger: trigger, Scheduled: scheduled, Started: started, Status: "running", Key: key}
	return &Pending{r: r, run: run, store: store, prefix: prefix, sidecarKey: sidecar, keep: Keep(sb)}, nil
}

// refuse returns cause, and for a scheduled run first records it as a failed run, so the state backup's check shows that the
// occurrence did not happen. A manual run is refused without a record, because its caller is waiting for the answer.
func (r *Runner) refuse(ctx context.Context, trigger string, scheduled, started time.Time, prefix string, cause error) error {
	if trigger == "manual" {
		return cause
	}
	id, err := r.Store.StartStateBackup(ctx, state.StateBackupRun{Trigger: trigger, Scheduled: scheduled, Started: started, Key: prefix + "/" + started.Format(stampLayout) + snapshotSuffix})
	if err != nil {
		return cause
	}
	if err = r.Store.FinishStateBackup(ctx, id, "failed", 0, "", cause.Error()); err != nil {
		r.logf("state backup: cannot record the refused run %d: %v", id, err)
	}
	return cause
}

func (r *Runner) store(ctx context.Context, dest manifest.Destination) (objectstore.Store, error) {
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

// exists reports whether any of the keys is in storage.
func exists(ctx context.Context, store objectstore.Store, keys ...string) (bool, error) {
	for _, k := range keys {
		_, err := store.Stat(ctx, k)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, objectstore.ErrNotFound):
		default:
			return false, err
		}
	}
	return false, nil
}

// Execute takes the snapshot: it writes a copy of the database to a private directory, uploads it, writes the sidecar,
// records the outcome and applies retention. A failure at any step leaves no sidecar and no success. The temporary copy is
// always removed.
func (p *Pending) Execute(ctx context.Context) Result {
	defer p.r.release()
	run := p.run
	fail := func(stage string, cause error) Result {
		detail := stage
		switch {
		case ctx.Err() != nil:
			detail = state.StoppedStateBackupDetail
		case cause != nil && strings.HasPrefix(cause.Error(), stage):
			detail = cause.Error()
		case cause != nil:
			detail += ": " + cause.Error()
		}
		p.r.finish(run.ID, "failed", 0, "", detail)
		run.Status, run.Detail, run.Finished = "failed", detail, p.r.now()
		return Result{Run: run}
	}
	dir, err := os.MkdirTemp(p.r.Dir, "snapshot-")
	if err != nil {
		return fail("snapshot failed", err)
	}
	defer os.RemoveAll(dir)
	schema, err := p.r.Store.SchemaVersion(ctx)
	if err != nil {
		return fail("snapshot failed", err)
	}
	path := filepath.Join(dir, "state.db")
	keyID, err := p.r.Store.Snapshot(ctx, path)
	if err != nil {
		return fail("snapshot failed", err)
	}
	size, sum, stage, err := p.upload(ctx, path)
	if err != nil {
		return fail(stage, err)
	}
	finished := p.r.now()
	side := Sidecar{Format: SidecarFormat, Environment: p.r.Env, SchemaVersion: schema, KeyID: keyID, Size: size, SHA256: sum, Created: finished}
	if err = p.writeSidecar(ctx, side); err != nil {
		p.discardSnapshot()
		return fail("sidecar failed", err)
	}
	run.Status, run.Size, run.SHA256, run.Finished = "succeeded", size, sum, finished
	p.r.finish(run.ID, "succeeded", size, sum, "")
	// The snapshot opens with the current key alone, so a key kept from before a rotation is no longer needed to restore.
	if err = p.r.Store.ForgetPreviousKey(context.WithoutCancel(ctx), keyID); err != nil {
		p.r.logf("state backup: the previous key was not removed: %v", err)
	}
	p.retain(ctx)
	return Result{Run: run}
}

// upload streams the snapshot into storage, hashing it on the way, and returns its size and SHA-256.
func (p *Pending) upload(ctx context.Context, path string) (size int64, sum, stage string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", "snapshot failed", err
	}
	defer f.Close()
	hash := sha256.New()
	uploaded, err := p.store.Put(ctx, p.run.Key, io.TeeReader(f, hash), nil)
	if err != nil {
		return 0, "", "upload failed", err
	}
	info, err := f.Stat()
	if err != nil {
		return 0, "", "upload failed", err
	}
	if uploaded != info.Size() {
		return 0, "", "upload failed", errors.New("stored size differs from the snapshot")
	}
	return uploaded, hex.EncodeToString(hash.Sum(nil)), "", nil
}

func (p *Pending) writeSidecar(ctx context.Context, s Sidecar) error {
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = p.store.Put(ctx, p.sidecarKey, bytes.NewReader(body), nil)
	return err
}

// discardSnapshot removes a snapshot whose sidecar could not be written. Without a sidecar it is never listed or restorable.
func (p *Pending) discardSnapshot() {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupLimit)
	defer cancel()
	if err := p.store.Delete(ctx, p.run.Key); err != nil {
		p.r.logf("state backup: snapshot %s was not removed after its sidecar failed: %v", p.run.Key, err)
	}
}

// RemoveLeftovers removes the snapshot directories that a previous process left in Dir. It returns how many it removed.
func (r *Runner) RemoveLeftovers() (int, error) {
	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "snapshot-") {
			if err = os.RemoveAll(filepath.Join(r.Dir, e.Name())); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// finish records the outcome of a run, even when the agent is stopping.
func (r *Runner) finish(id int64, status string, size int64, sum, detail string) {
	ctx, cancel := context.WithTimeout(context.Background(), finishTimeout)
	defer cancel()
	if err := r.Store.FinishStateBackup(ctx, id, status, size, sum, detail); err != nil {
		r.logf("state backup: cannot record the result of run %d: %v", id, err)
	}
}

// stored is a snapshot of storage, read from its sidecar.
type stored struct {
	snapshotKey, sidecarKey string
	Sidecar
}

// sidecars reads the valid sidecars of this environment under prefix, newest first. Anything else in the prefix is skipped, so
// retention never touches an object that is not a snapshot of this state.
func (r *Runner) sidecars(ctx context.Context, store objectstore.Store, prefix string) ([]stored, error) {
	objects, err := store.List(ctx, prefix+"/")
	if err != nil {
		return nil, err
	}
	var out []stored
	for _, o := range objects {
		rest := strings.TrimPrefix(o.Key, prefix+"/")
		if strings.Contains(rest, "/") || !strings.HasSuffix(rest, sidecarSuffix) || o.Size > maxSidecar {
			continue
		}
		sc, err := r.readSidecar(ctx, store, o.Key)
		switch {
		case errors.Is(err, objectstore.ErrNotFound), errors.Is(err, ErrInvalidSidecar):
			continue
		case err != nil:
			return nil, err
		}
		out = append(out, stored{snapshotKey: strings.TrimSuffix(o.Key, sidecarSuffix) + snapshotSuffix, sidecarKey: o.Key, Sidecar: sc})
	}
	// The keys start with a fixed-width UTC stamp, so the newest is the largest key.
	slices.SortFunc(out, func(a, b stored) int { return strings.Compare(b.sidecarKey, a.sidecarKey) })
	return out, nil
}

// readSidecar reads and checks one sidecar. A document of another format or environment is ErrInvalidSidecar.
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
		sc.Size < 0 || !sha256Hex.MatchString(sc.SHA256) || !keyIDHex.MatchString(sc.KeyID) {
		return Sidecar{}, ErrInvalidSidecar
	}
	return sc, nil
}

var (
	sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)
	keyIDHex  = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// retain deletes the snapshots beyond the number kept, the oldest first, snapshot before sidecar. A failure is logged and never
// fails the run.
func (p *Pending) retain(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupLimit)
	defer cancel()
	all, err := p.r.sidecars(ctx, p.store, p.prefix)
	if err != nil {
		p.r.logf("state backup: retention could not list storage: %v", err)
		return
	}
	for _, old := range all[min(p.keep, len(all)):] {
		if err = p.store.Delete(ctx, old.snapshotKey); err != nil {
			p.r.logf("state backup: retention could not delete %s: %v", old.snapshotKey, err)
			continue
		}
		if err = p.store.Delete(ctx, old.sidecarKey); err != nil {
			p.r.logf("state backup: retention could not delete %s: %v", old.sidecarKey, err)
		}
	}
}
