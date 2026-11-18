package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"berthy/internal/api"
	"berthy/internal/backup"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/jobs"
	"berthy/internal/manifest"
	"berthy/internal/objectstore"
	"berthy/internal/state"
	"berthy/internal/statebackup"
)

// Backups is the agent's volume backups: the runner, the credentials and volume resolution it needs, and the runs started
// through the API, which continue after their request ends. Configuration comes from the running manifest restricted to
// the apps that are deployed.
type Backups struct {
	Runner *backup.Runner
	// Scheduler runs the backups that have a schedule, from Start on.
	Scheduler *backup.Scheduler
	// State takes the snapshots of the state backup, and StateScheduler runs them on its schedule, from Start on.
	State          *statebackup.Runner
	StateScheduler *statebackup.Scheduler

	store *state.Store
	log   func(string)
	// newStore builds an object store client. It is objectstore.New except in tests.
	newStore func(objectstore.Config) (objectstore.Store, error)

	mu      sync.Mutex
	base    context.Context // Start's context, which started runs end with
	wg      sync.WaitGroup  // the runs in progress, and the scheduler
	started sync.Once
}

// NewBackups returns the backups of an agent with state and Docker, or nil for an agent without either. sched runs a backup's
// before job and may be nil. image provides the helper image of the archive and restore containers. It also gives the
// controller the means to restore, so the restore operation is available. log receives operational messages, which never
// contain secrets.
func NewBackups(c Config, e *engine.Engine, d *deploy.Controller, sched *jobs.Scheduler, image func(context.Context) (string, error), log func(string)) *Backups {
	if e == nil || d == nil || d.Store == nil {
		return nil
	}
	var holder backup.JobHolder
	if sched != nil {
		holder = sched
	}
	arch := &backup.Archiver{Engine: e, Image: image, Env: c.Environment, Log: log}
	return newBackups(c.Environment, d, arch, holder, log)
}

func newBackups(env string, d *deploy.Controller, arch *backup.Archiver, holder backup.JobHolder, log func(string)) *Backups {
	b := &Backups{store: d.Store, log: log, newStore: objectstore.New}
	b.Runner = &backup.Runner{
		Store:    d.Store,
		Archiver: arch,
		Objects:  b.objects,
		Jobs:     holder,
		Resolve: func(ctx context.Context, m manifest.Manifest, bk manifest.Backup) (string, error) {
			return resolveVolume(ctx, d, m, bk)
		},
		Env: env,
		Log: log,
	}
	d.Restorer = restorer{b: b, store: d.Store}
	d.Backups = b
	b.Scheduler = &backup.Scheduler{
		Env:    env,
		Store:  d.Store,
		Runner: b.Runner,
		Manifest: func(ctx context.Context) (manifest.Manifest, error) {
			m, err := runningManifest(ctx, d.Store)
			return m, err
		},
		Log: log,
	}
	b.State = &statebackup.Runner{Store: d.Store, Env: env, Dir: d.StateDir, Objects: b.stateObjects, Log: log}
	b.StateScheduler = &statebackup.Scheduler{
		Env:      env,
		Store:    d.Store,
		Runner:   b.State,
		Manifest: func(ctx context.Context) (manifest.Manifest, error) { return recordedManifest(ctx, d.Store) },
		Log:      log,
	}
	return b
}

// objects builds the client of a destination from its credentials in the secret store. A secret that is missing, belongs to
// a registry or is empty makes the credentials unavailable.
func (b *Backups) objects(ctx context.Context, dest manifest.Destination) (objectstore.Store, error) {
	access, secret, ok := b.credentials(ctx, dest)
	if !ok {
		return nil, backup.ErrCredentials
	}
	return b.newStore(objectstore.Config{Endpoint: dest.Endpoint, Region: dest.Region, Bucket: dest.Bucket, AccessKey: access, SecretKey: secret})
}

// stateObjects is objects for the state backup, which words its refusals its own way.
func (b *Backups) stateObjects(ctx context.Context, dest manifest.Destination) (objectstore.Store, error) {
	access, secret, ok := b.credentials(ctx, dest)
	if !ok {
		return nil, statebackup.ErrCredentials
	}
	store, err := b.newStore(objectstore.Config{Endpoint: dest.Endpoint, Region: dest.Region, Bucket: dest.Bucket, AccessKey: access, SecretKey: secret})
	if err != nil {
		return nil, statebackup.ErrStorage
	}
	return store, nil
}

// credentials reads the access key and the secret key of a destination. ok is false when either is missing, belongs to a
// registry or is empty.
func (b *Backups) credentials(ctx context.Context, dest manifest.Destination) (access, secret string, ok bool) {
	read := func(name string) (string, bool) {
		info, value, err := b.store.Secret(ctx, name)
		if err != nil || info.Registry != "" || len(value) == 0 {
			return "", false
		}
		return string(value), true
	}
	if access, ok = read(dest.AccessKeySecret); !ok {
		return "", "", false
	}
	secret, ok = read(dest.SecretKeySecret)
	return access, secret, ok
}

// resolveVolume returns the Docker name of the volume a backup names. For an image app it is the source of a named-volume
// mount of one of its roles. For a Compose app it is the volume's key in the Compose file, resolved by Compose from the
// project's stored files. A volume the app does not mount is backup.ErrNotMounted.
func resolveVolume(ctx context.Context, d *deploy.Controller, m manifest.Manifest, b manifest.Backup) (string, error) {
	app, ok := m.Apps[b.App]
	if !ok {
		return "", backup.ErrNotMounted
	}
	switch app.Kind {
	case "image":
		for _, role := range app.Roles {
			for _, v := range role.Volumes {
				if mount, err := manifest.ParseMount(v); err == nil && mount.Source == b.Volume {
					return mount.Source, nil
				}
			}
		}
	case "compose":
		names, err := d.ComposeVolumeNames(ctx, m.ComposeProject(b.App))
		if err != nil {
			return "", err
		}
		if name, ok := names[b.Volume]; ok {
			return name, nil
		}
	}
	return "", backup.ErrNotMounted
}

// Start makes ctx the life of the runs that API requests start: they end when it ends. It first records the runs a previous
// process left running as failed, and removes the helper containers it left, so it must return before the agent takes requests. It does nothing on a nil Backups.
func (b *Backups) Start(ctx context.Context) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.base = ctx
	b.mu.Unlock()
	if n, err := b.State.RemoveLeftovers(); err != nil {
		b.logf("state backup: cannot remove leftover snapshots: %v", err)
	} else if n > 0 {
		b.logf("state backup: %d leftover snapshot(s) removed", n)
	}
	if n, err := b.store.AbandonRunningStateBackups(ctx); err != nil {
		b.logf("state backup: cannot fail interrupted snapshots: %v", err)
	} else if n > 0 {
		b.logf("state backup: %d snapshot(s) were interrupted by an agent restart and are recorded as failed", n)
	}
	if n, err := b.store.AbandonRunningBackups(ctx); err != nil {
		b.logf("backups: cannot fail interrupted runs: %v", err)
	} else if n > 0 {
		b.logf("backups: %d run(s) were interrupted by an agent restart and are recorded as failed", n)
	}
	// A killed agent leaves its archive or restore container running. No run of this process has started one yet.
	if _, err := b.Runner.Archiver.RemoveLeftovers(ctx); err != nil {
		b.logf("backups: cannot remove leftover helper containers: %v", err)
	}
	// The schedules run from now on, once: a second Start only changes the context of the runs that requests start.
	b.started.Do(func() {
		b.wg.Add(2)
		go func() {
			defer b.wg.Done()
			b.Scheduler.Run(ctx)
		}()
		go func() {
			defer b.wg.Done()
			b.StateScheduler.Run(ctx)
		}()
	})
}

// RunBackup runs the backup of a deploy hook and waits for it, on the manifest that is being deployed, as the deploy controller
// asks. It makes no scheduled run of its own: the run has the trigger "hook".
func (b *Backups) RunBackup(ctx context.Context, m manifest.Manifest, name string) (deploy.BackupResult, error) {
	res, err := b.Runner.Run(ctx, m, name, "hook")
	if err != nil {
		return deploy.BackupResult{}, err
	}
	return deploy.BackupResult{RunID: res.Run.ID, Status: res.Run.Status, Detail: res.Run.Detail}, nil
}

var _ deploy.BackupRunner = (*Backups)(nil)

// Wait returns once the runs in progress have recorded their outcome. Cancel the context given to Start first, or it waits
// for the runs to end by themselves.
func (b *Backups) Wait() {
	if b != nil {
		b.wg.Wait()
	}
}

func (b *Backups) logf(format string, args ...any) {
	if b.log != nil {
		b.log(fmt.Sprintf(format, args...))
	}
}

// start continues an admitted run in the background, on a context that ends with the agent and not with the request. The
// start and the outcome are audited as the actor that asked. The run does not wait for the request, so a slow audit write
// cannot keep a recorded run from running, or from recording that the agent stopped.
func (b *Backups) start(p *backup.Pending, actor state.Actor, name string) {
	b.mu.Lock()
	base := b.base
	b.mu.Unlock()
	if base == nil {
		base = context.Background()
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.audit(base, actor, name, "started")
		res := p.Execute(base)
		b.audit(base, actor, name, res.Run.Status)
	}()
}

// audit records one step of a manual run in the audit log, as the actor that asked for it.
func (b *Backups) audit(ctx context.Context, actor state.Actor, name, outcome string) {
	if err := b.store.RecordAudit(context.WithoutCancel(ctx), actor, "backup_run:"+name, outcome); err != nil {
		b.logf("backups: cannot audit the run of %s: %v", name, err)
	}
}

// restorer is the controller's way to restore: it reads the manifest of the deployed apps and uses the runner.
type restorer struct {
	b     *Backups
	store *state.Store
}

var _ deploy.VolumeRestorer = restorer{}

func (r restorer) manifest(ctx context.Context) (manifest.Manifest, error) {
	return recordedManifest(ctx, r.store)
}

func (r restorer) CheckRestore(ctx context.Context, key, into string) (deploy.RestorePlan, error) {
	m, err := r.manifest(ctx)
	if err != nil {
		return deploy.RestorePlan{}, err
	}
	sc, err := r.b.Runner.Check(ctx, m, key, into)
	if err != nil {
		return deploy.RestorePlan{}, err
	}
	return deploy.RestorePlan{Key: key, Backup: sc.Backup, App: sc.App, Volume: sc.Volume, Into: into, Size: sc.Size, SHA256: sc.SHA256, Finished: sc.Finished}, nil
}

func (r restorer) Restore(ctx context.Context, operationID, key, into string) error {
	m, err := r.manifest(ctx)
	if err != nil {
		return err
	}
	return r.b.Runner.Restore(ctx, m, key, into, operationID)
}

// backupError answers a request that the runner refused. A refusal's text names no secret, so it is shown as it is.
func backupError(w http.ResponseWriter, err error) {
	var refusal backup.Refusal
	switch {
	case errors.Is(err, backup.ErrUnknownBackup):
		writeError(w, 404, "not_found", "not found")
	case errors.Is(err, backup.ErrBackupRunning):
		writeError(w, 409, "conflict", string(backup.ErrBackupRunning))
	case errors.As(err, &refusal):
		writeError(w, 400, "operation_failed", string(refusal))
	default:
		writeError(w, 503, "state_unavailable", "state unavailable")
	}
}

// serveBackups answers the backup routes: the list of backups in storage, the runs of a backup, and a manual run. rest is the
// path after the environment, and the caller has checked the method and the scope.
func serveBackups(w http.ResponseWriter, r *http.Request, rest []string, d *deploy.Controller, b *Backups, actor state.Actor) {
	ctx := r.Context()
	if b == nil {
		writeError(w, 503, "backups_unavailable", "backups unavailable")
		return
	}
	if len(rest) == 1 {
		recorded, err := recordedManifest(ctx, d.Store)
		if err != nil {
			writeError(w, 503, "state_unavailable", "state unavailable")
			return
		}
		entries, err := b.Runner.List(ctx, recorded, r.URL.Query().Get("backup"))
		if err != nil {
			backupError(w, err)
			return
		}
		json.NewEncoder(w).Encode(entries)
		return
	}
	m, err := runningManifest(ctx, d.Store)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, 503, "state_unavailable", "state unavailable")
		return
	}
	switch {
	case rest[2] == "runs":
		limit := 20
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 100 {
				writeError(w, 400, "bad_request", "invalid limit")
				return
			}
			limit = n
		}
		runs, err := d.Store.BackupRuns(ctx, rest[1], limit)
		if err != nil {
			writeError(w, 503, "state_unavailable", "state unavailable")
			return
		}
		if _, known := m.Backups[rest[1]]; len(runs) == 0 && !known {
			writeError(w, 404, "not_found", "not found")
			return
		}
		out := make([]api.BackupRun, 0, len(runs))
		for _, run := range runs {
			out = append(out, backupRunStatus(run))
		}
		json.NewEncoder(w).Encode(out)
	default:
		p, err := b.Runner.Begin(ctx, m, rest[1], "manual")
		if err != nil {
			backupError(w, err)
			return
		}
		b.start(p, actor, rest[1])
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]int64{"backup_run_id": p.ID()})
	}
}

func backupRunStatus(r state.BackupRun) api.BackupRun {
	out := api.BackupRun{ID: r.ID, Backup: r.Backup, App: r.App, Volume: r.Volume, Destination: r.Destination, Key: r.ObjectKey, Trigger: r.Trigger,
		Started: r.Started, Status: r.Status, Size: r.Size, SHA256: r.SHA256, Detail: r.Detail}
	if !r.Finished.IsZero() {
		t := r.Finished
		out.Finished = &t
	}
	if !r.Scheduled.IsZero() {
		t := r.Scheduled
		out.Scheduled = &t
	}
	return out
}
