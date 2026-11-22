package backup

import (
	"bytes"
	"compress/gzip"
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
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"berthy/internal/api"
	"berthy/internal/jobs"
	"berthy/internal/manifest"
	"berthy/internal/objectstore"
	"berthy/internal/scope"
	"berthy/internal/state"
)

// The formats of a backup, which the sidecar names so a restore knows what an object is: a gzip-compressed tar of a volume, and
// the gzip-compressed standard output of a command.
const (
	SidecarFormat        = "berthy-volume-tar-gzip-1"
	CommandSidecarFormat = "berthy-command-gzip-1"
)

const (
	archiveSuffix = ".tar.gz"
	commandSuffix = ".gz"
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
	// ErrCommandBackup means a restore into a volume was asked of a backup that holds the output of a command.
	ErrCommandBackup = Refusal("this is a command backup; restore it with --exec")
	// ErrVolumeBackup means a restore into a command was asked of a backup that holds a volume.
	ErrVolumeBackup = Refusal("this is a volume backup; restore it with --into")
	// ErrCommands means the agent has no way to run the command of a backup or a restore.
	ErrCommands = Refusal("commands unavailable")
	// ErrChecksumMismatch means what a restore read from storage is not what the backup recorded. The new volume is removed.
	ErrChecksumMismatch = errors.New("checksum mismatch")
)

// refusals are the fixed texts that may be shown for any request, such as the text of a failed restore operation.
var refusals = []Refusal{ErrBackupRunning, ErrUnknownBackup, ErrCredentials, ErrStorage, ErrNotFound, ErrInvalidSidecar, ErrVolumeName, ErrVolumeExists, ErrCommandBackup, ErrVolumeBackup, ErrCommands}

// PublicRefusal reports whether s is one of the fixed refusal texts, which hold no names and so may be shown.
func PublicRefusal(s string) bool { return slices.Contains(refusals, Refusal(s)) }

// JobHolder runs the job that prepares a volume for its backup.
type JobHolder interface {
	// RunHeld waits for any active run of the job to finish, runs it once (trigger "backup"), and keeps the job marked
	// running until release is called, so scheduled occurrences in between are recorded as skipped.
	RunHeld(ctx context.Context, name string) (state.JobRun, func(), error)
}

// CommandRunner runs the command of a command backup, and the command a restore feeds. The agent supplies it.
type CommandRunner interface {
	// RunCommand runs job's command as a job is run, with its output going to st.Stdout and its input read from st.Stdin. kind
	// is "backup" or "restore", name the backup's name, and id tells the run from others of its kind. The run is bounded by
	// ctx's deadline. The outcome's Output is the last lines of a failed or timed-out run's standard error, redacted.
	RunCommand(ctx context.Context, m manifest.Manifest, kind, name, id string, job manifest.Job, st jobs.Streams) (jobs.Outcome, error)
}

// CommandError is a command that did not do its work: it exited non-zero, ran past its time limit or could not run. Its text is
// worded for a run's detail, with the end of the command's standard error, which holds the reason.
type CommandError struct {
	Outcome jobs.Outcome
	// Timeout is the time limit the command had.
	Timeout time.Duration
}

// maxStderrDetail bounds the standard error in the text of a CommandError, so a run's detail, which is stored in 512 bytes, keeps
// what the command said last.
const maxStderrDetail = 300

func (e *CommandError) Error() string {
	why := e.Why()
	if e.Outcome.Exit == nil && !e.Outcome.TimedOut {
		return why
	}
	if tail := lastBytes(strings.TrimSpace(e.Outcome.Output), maxStderrDetail); tail != "" {
		why += ": " + tail
	}
	return why
}

// Why is how the command ended, without what it said: "exited with code 2", "timed out after 1h0m0s", or the reason it could
// not run.
func (e *CommandError) Why() string {
	switch {
	case e.Outcome.Exit != nil:
		return fmt.Sprintf("exited with code %d", *e.Outcome.Exit)
	case e.Outcome.TimedOut:
		return "timed out after " + e.Timeout.String()
	}
	return e.Outcome.Detail
}

// lastBytes is the last n bytes of s, cut at a character, with a mark where it was cut. It is s when it fits.
func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return "..." + s
}

// Runner backs up the volumes the manifest names, lists what is in storage and restores it. Its fields are set before use.
type Runner struct {
	Store    *state.Store
	Archiver *Archiver
	// Objects builds the client of a destination, with its credentials. It returns ErrCredentials when they are missing.
	Objects func(ctx context.Context, d manifest.Destination) (objectstore.Store, error)
	// Jobs runs a backup's `before` job. It is nil when the agent has no scheduler.
	Jobs JobHolder
	// Commands runs the command of a command backup and of a restore into a command. It is nil when the agent cannot.
	Commands CommandRunner
	// Resolve returns the Docker name of the volume a backup names, or an error that wraps ErrNotMounted.
	Resolve      func(ctx context.Context, m manifest.Manifest, b manifest.Backup) (dockerVolume string, err error)
	Project, Env string
	Now          func() time.Time
	Log          func(string)

	mu     sync.Mutex
	active map[string]bool // Docker volume, or command backup's name -> a backup of it is running; one at a time
}

// Sidecar is the document stored beside each archive. It is written only after the archive was uploaded, so a backup that
// has one is complete. Format says what the archive is. A command backup has no volume.
type Sidecar struct {
	Format       string    `json:"format"`
	Project      string    `json:"project"`
	Environment  string    `json:"environment"`
	Backup       string    `json:"backup"`
	App          string    `json:"app"`
	Volume       string    `json:"volume,omitempty"`
	DockerVolume string    `json:"docker_volume,omitempty"`
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

// objectDir is the prefix of a backup's objects, without a trailing slash: <project>/<environment>/, then the manifest's
// prefix or <app>/<volume>, then the backup's name.
func objectDir(project, env, name string, b manifest.Backup) (string, error) {
	base := strings.Trim(b.Prefix, "/")
	if base == "" {
		base = b.App + "/" + b.Volume
		if b.Volume == "" {
			base = b.App + "/command"
		}
	}
	dir := project + "/" + env + "/" + base + "/" + name
	for _, seg := range strings.Split(dir, "/") {
		if seg == "." || seg == ".." || !dirSegment.MatchString(seg) {
			return "", Refusal(fmt.Sprintf("backup %s: invalid prefix", name))
		}
	}
	return dir, nil
}

func archiveKey(dir string, started time.Time, runID int64, suffix string) string {
	return fmt.Sprintf("%s/%s-%d%s", dir, started.UTC().Format(stampLayout), runID, suffix)
}

// suffixOf is the suffix of the archive of a backup of this format.
func suffixOf(format string) string {
	if format == CommandSidecarFormat {
		return commandSuffix
	}
	return archiveSuffix
}

// sidecarKeyOf is the key of the sidecar of the archive with this key, whichever the format of the backup. It is the archive's
// own key when that has neither suffix.
func sidecarKeyOf(archive string) string {
	if strings.HasSuffix(archive, archiveSuffix) {
		return strings.TrimSuffix(archive, archiveSuffix) + sidecarSuffix
	}
	return strings.TrimSuffix(archive, commandSuffix) + sidecarSuffix
}
func archiveKeyOf(sidecar, format string) string {
	return strings.TrimSuffix(sidecar, sidecarSuffix) + suffixOf(format)
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
	r      *Runner
	m      manifest.Manifest
	run    state.BackupRun
	backup manifest.Backup
	store  objectstore.Store
	// claim is what the run holds so only one runs at a time: the Docker volume, or for a command backup its name.
	claim        string
	dockerVolume string // empty for a command backup
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
	dir, err := objectDir(r.Project, r.Env, name, b)
	if err != nil {
		return nil, err
	}
	var dockerVolume, claim string
	suffix := commandSuffix
	if len(b.Command) > 0 {
		if r.Commands == nil {
			return nil, ErrCommands
		}
		claim = "command:" + name
	} else {
		suffix = archiveSuffix
		dockerVolume, err = r.Resolve(ctx, m, b)
		switch {
		case errors.Is(err, ErrNotMounted):
			return nil, Refusal(fmt.Sprintf("backup %s: volume %s is not mounted by app %s", name, b.Volume, b.App))
		case err != nil:
			return nil, Refusal(fmt.Sprintf("backup %s: volume lookup failed", name))
		}
		claim = dockerVolume
	}
	store, err := r.store(ctx, m, name, b)
	if err != nil {
		return nil, err
	}
	if !r.claim(claim) {
		return nil, ErrBackupRunning
	}
	// The final key names the run's own ID, so the run is recorded under a provisional key and renamed at once.
	var token [8]byte
	if _, err = rand.Read(token[:]); err != nil {
		r.unclaim(claim)
		return nil, err
	}
	started := r.now()
	id, err := r.Store.StartBackup(ctx, state.BackupRun{
		Backup: name, App: b.App, Volume: b.Volume, Destination: b.Destination, Trigger: trigger, Scheduled: scheduled, Started: started,
		ObjectKey: dir + "/starting-" + hex.EncodeToString(token[:]),
	})
	if err != nil {
		r.unclaim(claim)
		return nil, err
	}
	key := archiveKey(dir, started, id, suffix)
	if err = r.Store.SetBackupObjectKey(ctx, id, key); err != nil {
		r.finish(id, "failed", 0, "", "state unavailable")
		r.unclaim(claim)
		return nil, err
	}
	run := state.BackupRun{ID: id, Backup: name, App: b.App, Volume: b.Volume, Destination: b.Destination, ObjectKey: key, Trigger: trigger, Scheduled: scheduled, Started: started, Status: "running"}
	return &Pending{r: r, m: m, run: run, backup: b, store: store, claim: claim, dockerVolume: dockerVolume, dir: dir, key: key}, nil
}

func (r *Runner) claim(what string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active[what] {
		return false
	}
	if r.active == nil {
		r.active = map[string]bool{}
	}
	r.active[what] = true
	return true
}

func (r *Runner) unclaim(what string) {
	r.mu.Lock()
	delete(r.active, what)
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
	defer p.r.unclaim(p.claim)
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
	format, transfer := SidecarFormat, p.transfer
	if len(p.backup.Command) > 0 {
		format, transfer = CommandSidecarFormat, p.transferCommand
	}
	size, sum, stage, err := transfer(ctx)
	release()
	if err != nil {
		return fail(stage, err)
	}
	finished := p.r.now()
	sidecar := Sidecar{Format: format, Project: p.r.Project, Environment: p.r.Env, Backup: run.Backup, App: run.App, Volume: run.Volume, DockerVolume: p.dockerVolume,
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

// countingWriter counts what is written through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

// transferCommand runs the backup's command and streams its output into storage through a pipe: compressed, and hashed on the
// way, so nothing is written to the server's disk. Whichever side fails first ends the other, and its stage is the one reported.
// The pipe is closed with the command's failure, which aborts the upload, so a command that exits non-zero, writes nothing or
// runs out of time leaves no object.
func (p *Pending) transferCommand(ctx context.Context) (size int64, sum, stage string, err error) {
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	hash := sha256.New()
	hw := &hashingWriter{w: pw, h: hash}
	gz := gzip.NewWriter(hw)
	stdout := &countingWriter{w: gz}
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
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		limit := p.backup.TimeoutDuration()
		cctx, ccancel := context.WithTimeout(tctx, limit)
		defer ccancel()
		out, cerr := p.r.Commands.RunCommand(cctx, p.m, "backup", p.run.Backup, strconv.FormatInt(p.run.ID, 10), p.backup.Job(), jobs.Streams{Stdout: stdout})
		if cerr != nil && tctx.Err() == nil && errors.Is(cctx.Err(), context.DeadlineExceeded) {
			out.TimedOut, cerr = true, nil
		}
		var failure error
		stage := "command failed"
		switch {
		case tctx.Err() != nil:
			// The upload failed or the agent is stopping; either is recorded by the one that saw it first.
			failure = tctx.Err()
		case cerr != nil && cctx.Err() == nil:
			failure = cerr
		case out.Exit == nil || *out.Exit != 0 || out.TimedOut:
			failure = &CommandError{Outcome: out, Timeout: limit}
		case stdout.n == 0:
			failure = errors.New("wrote no output")
		default:
			if cerr = gz.Close(); cerr != nil {
				stage, failure = "compress failed", cerr
			}
		}
		if failure != nil {
			record(stage, failure)
		}
		pw.CloseWithError(failure)
	}()
	uploaded, perr := p.store.Put(tctx, p.key, pr, nil)
	if perr != nil {
		record("upload failed", perr)
		pr.CloseWithError(perr)
		cancel()
	}
	<-finished
	mu.Lock()
	defer mu.Unlock()
	if firstErr != nil {
		return 0, "", firstStage, firstErr
	}
	if uploaded != hw.n {
		return 0, "", "upload failed", errors.New("stored size differs from the output")
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
		out = append(out, stored{sidecarKey: o.Key, archiveKey: archiveKeyOf(o.Key, sc.Format), Sidecar: sc})
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
	if len(data) > maxSidecar || json.Unmarshal(data, &sc) != nil || (sc.Format != SidecarFormat && sc.Format != CommandSidecarFormat) || r.Project == "" || sc.Project != r.Project || sc.Environment != r.Env ||
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
		dir, err := objectDir(r.Project, r.Env, n, b)
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
			out = append(out, api.BackupEntry{Key: s.archiveKey, Format: s.Format, Backup: s.Backup, App: s.App, Volume: s.Volume, Size: s.Size, SHA256: s.SHA256, Finished: s.Finished})
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

// Check validates a restore into a volume without changing anything: the key names an archive in storage whose sidecar exists and parses,
// and the volume to restore into has a valid name and does not exist.
func (r *Runner) Check(ctx context.Context, m manifest.Manifest, key, into string) (Sidecar, error) {
	loc, err := r.locate(ctx, m, key)
	if err != nil {
		return Sidecar{}, err
	}
	if loc.sidecar.Format != SidecarFormat {
		return Sidecar{}, ErrCommandBackup
	}
	if err = r.checkTarget(ctx, into); err != nil {
		return Sidecar{}, err
	}
	return loc.sidecar, nil
}

// CheckCommand validates a restore into a command without changing anything: the key names a command backup in storage whose
// sidecar exists and parses.
func (r *Runner) CheckCommand(ctx context.Context, m manifest.Manifest, key string) (Sidecar, error) {
	loc, err := r.locate(ctx, m, key)
	if err != nil {
		return Sidecar{}, err
	}
	if loc.sidecar.Format != CommandSidecarFormat {
		return Sidecar{}, ErrVolumeBackup
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
	if !strings.HasSuffix(key, commandSuffix) || sidecar == key {
		return located{}, ErrNotFound
	}
	for _, name := range slices.Sorted(maps.Keys(m.Backups)) {
		b := m.Backups[name]
		dir, err := objectDir(r.Project, r.Env, name, b)
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
		case archiveKeyOf(sidecar, sc.Format) != key:
			// The archive is not the kind of object its sidecar describes.
			return located{}, ErrNotFound
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
	if loc.sidecar.Format != SidecarFormat {
		return ErrCommandBackup
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
	labels := (scope.ID{Project: r.Project, Environment: r.Env}).Labels()
	labels["berthy.restored-from"], labels[RestoreOperationLabel] = key, operationID
	return r.Archiver.Restore(ctx, intoVolume, io.TeeReader(body, counted), labels, verify)
}

// RestoreCommand streams the command backup with this key, decompressed, into the input of a command, which job names. It
// refuses a key without a sidecar and a backup of a volume. The backup is read twice: first to the end, to check its size and
// SHA-256 against the sidecar, and only then again into the command, so a backup that is not what was stored never reaches the
// command, whatever that command does with its input. The second read is checked too. A command that exits non-zero, runs out of
// time or cannot run is a *CommandError, with the outcome. id tells the run from others of its kind.
func (r *Runner) RestoreCommand(ctx context.Context, m manifest.Manifest, key string, job manifest.Job, id string) (jobs.Outcome, error) {
	loc, err := r.locate(ctx, m, key)
	if err != nil {
		return jobs.Outcome{}, err
	}
	if loc.sidecar.Format != CommandSidecarFormat {
		return jobs.Outcome{}, ErrVolumeBackup
	}
	if r.Commands == nil {
		return jobs.Outcome{}, ErrCommands
	}
	if err = r.verify(ctx, loc, key); err != nil {
		return jobs.Outcome{}, err
	}
	body, _, err := loc.store.Get(ctx, key)
	if errors.Is(err, objectstore.ErrNotFound) {
		return jobs.Outcome{}, ErrNotFound
	}
	if err != nil {
		r.logf("restore of %s: reading the backup failed: %v", key, err)
		return jobs.Outcome{}, ErrStorage
	}
	defer body.Close()
	hash := sha256.New()
	counted := &countingHash{h: hash}
	raw := io.TeeReader(body, counted)
	input, err := gzip.NewReader(raw)
	if err != nil {
		return jobs.Outcome{}, ErrChecksumMismatch
	}
	limit := job.TimeoutDuration()
	cctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	out, err := r.Commands.RunCommand(cctx, m, "restore", loc.sidecar.Backup, id, job, jobs.Streams{Stdin: input})
	if err != nil && ctx.Err() == nil && errors.Is(cctx.Err(), context.DeadlineExceeded) {
		out.TimedOut, err = true, nil
	}
	switch {
	case err != nil && ctx.Err() != nil:
		return out, ctx.Err()
	case err != nil:
		r.logf("restore of %s: the command could not be run: %v", key, err)
		return out, Refusal("restore command could not be run")
	case out.Exit == nil || *out.Exit != 0 || out.TimedOut:
		return out, &CommandError{Outcome: out, Timeout: limit}
	}
	// What the command left unread, which a gzip stream can have after its end, belongs to the digest too.
	if _, err = io.Copy(io.Discard, raw); err != nil {
		return out, ErrStorage
	}
	if counted.n != loc.sidecar.Size || hex.EncodeToString(hash.Sum(nil)) != loc.sidecar.SHA256 {
		return out, ErrChecksumMismatch
	}
	return out, nil
}

// verify reads the object to its end and compares its size and SHA-256 with the sidecar's, keeping nothing of it.
func (r *Runner) verify(ctx context.Context, loc located, key string) error {
	body, _, err := loc.store.Get(ctx, key)
	if errors.Is(err, objectstore.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		r.logf("restore of %s: reading the backup failed: %v", key, err)
		return ErrStorage
	}
	defer body.Close()
	hash := sha256.New()
	counted := &countingHash{h: hash}
	if _, err = io.Copy(io.Discard, io.TeeReader(body, counted)); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.logf("restore of %s: reading the backup failed: %v", key, err)
		return ErrStorage
	}
	if counted.n != loc.sidecar.Size || hex.EncodeToString(hash.Sum(nil)) != loc.sidecar.SHA256 {
		return ErrChecksumMismatch
	}
	return nil
}
