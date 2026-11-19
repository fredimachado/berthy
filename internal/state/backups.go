package state

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"time"
)

// BackupRun is one run of a backup: archive the volume, or take the output of the command, upload it and record the outcome.
// A run of a command backup has no Volume.
type BackupRun struct {
	ID                               int64
	Backup, App, Volume, Destination string
	ObjectKey, Trigger               string // Trigger is schedule, manual or hook
	// Scheduled is the UTC minute of the cron occurrence a scheduled run belongs to, and zero for any other run.
	Scheduled time.Time
	// Started is when the run began; Finished is zero while it runs.
	Started, Finished time.Time
	Status            string // running, succeeded or failed
	Size              int64
	SHA256, Detail    string
}

const (
	// AbandonedBackupDetail is the detail of a run that a previous agent process left running.
	AbandonedBackupDetail = "agent restarted during the backup"
	// StoppedBackupDetail is the detail of a run that an orderly agent stop cut short.
	StoppedBackupDetail = "agent stopped during the backup"
	// maxBackupDetail bounds the detail stored for one run.
	maxBackupDetail = 512
)

var (
	backupTriggers = map[string]bool{"schedule": true, "manual": true, "hook": true}
	backupSHA256   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// StartBackup records a backup run that has begun. A run starts as running; any other status is refused. Each object key
// is recorded once per environment, whatever the run's status, and so is each scheduled occurrence of a backup, so a key or an
// occurrence that is already recorded returns ErrConflict. A scheduled run has an occurrence and no other run does.
func (s *Store) StartBackup(ctx context.Context, b BackupRun) (int64, error) {
	if b.Status != "" && b.Status != "running" {
		return 0, errors.New("invalid backup run")
	}
	if b.Backup == "" || b.App == "" || b.Destination == "" || b.ObjectKey == "" || !backupTriggers[b.Trigger] || b.Started.IsZero() || (b.Trigger == "schedule") == b.Scheduled.IsZero() {
		return 0, errors.New("invalid backup run")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO backups(environment,backup,app,volume,destination,object_key,trigger,started,finished,status,size,sha256,detail,scheduled) VALUES(?,?,?,?,?,?,?,?,0,'running',0,'','',?) ON CONFLICT DO NOTHING`,
		s.env, b.Backup, b.App, b.Volume, b.Destination, b.ObjectKey, b.Trigger, toMilli(b.Started), toMilli(b.Scheduled))
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrConflict
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	// The newest two successes are kept however old they are: the size check compares the newest with the one before it.
	if err = pruneRuns(ctx, tx, "backups", "environment=? AND backup=?", "status='succeeded'", 2, s.env, b.Backup); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// FinishBackup ends a running run with status succeeded or failed. A succeeded run needs the archive's SHA-256. It returns
// ErrConflict when the run is not running, so a run is finished at most once.
func (s *Store) FinishBackup(ctx context.Context, id int64, status string, size int64, sha256, detail string) error {
	if status != "succeeded" && status != "failed" {
		return errors.New("invalid backup status")
	}
	if size < 0 {
		return errors.New("invalid backup size")
	}
	if (status == "succeeded" || sha256 != "") && !backupSHA256.MatchString(sha256) {
		return errors.New("invalid backup checksum")
	}
	if len(detail) > maxBackupDetail {
		detail = detail[:maxBackupDetail]
	}
	res, err := s.db.ExecContext(ctx, `UPDATE backups SET status=?,size=?,sha256=?,detail=?,finished=? WHERE environment=? AND id=? AND status='running'`,
		status, size, sha256, detail, now(), s.env, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	return nil
}

// AbandonRunningBackups fails every backup run that a previous process left running, and returns how many it failed. The
// run is never listed as successful. If its upload completed just before the crash, an object may exist under its key;
// backup list shows what is in storage, so that object is visible even though no run records it as a success.
func (s *Store) AbandonRunningBackups(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE backups SET status='failed',detail=?,finished=? WHERE environment=? AND status='running'`, AbandonedBackupDetail, now(), s.env)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

const backupColumns = `id,backup,app,volume,destination,object_key,trigger,started,finished,status,size,sha256,detail,scheduled`

func scanBackupRun(row interface{ Scan(...any) error }) (BackupRun, error) {
	var r BackupRun
	var started, finished, scheduled int64
	if err := row.Scan(&r.ID, &r.Backup, &r.App, &r.Volume, &r.Destination, &r.ObjectKey, &r.Trigger, &started, &finished, &r.Status, &r.Size, &r.SHA256, &r.Detail, &scheduled); err != nil {
		return BackupRun{}, err
	}
	r.Started, r.Finished, r.Scheduled = fromMilli(started), fromMilli(finished), fromMilli(scheduled)
	return r, nil
}

// SetBackupObjectKey replaces the object key of a run that is still running. A run records a provisional key when it starts,
// because its final key names the run's own ID, which only exists once the run is recorded. It returns ErrConflict when the
// run is not running or the key belongs to another run.
func (s *Store) SetBackupObjectKey(ctx context.Context, id int64, key string) error {
	if key == "" {
		return errors.New("invalid backup object key")
	}
	// OR IGNORE turns a key that another run holds into an update of no rows.
	res, err := s.db.ExecContext(ctx, `UPDATE OR IGNORE backups SET object_key=? WHERE environment=? AND id=? AND status='running'`, key, s.env, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	return nil
}

// BackupRuns returns the newest runs of a backup, newest first. An empty backup means every backup.
func (s *Store) BackupRuns(ctx context.Context, backup string, limit int) ([]BackupRun, error) {
	if limit < 1 {
		return nil, errors.New("invalid limit")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+backupColumns+` FROM backups WHERE environment=? AND (?='' OR backup=?) ORDER BY id DESC LIMIT ?`, s.env, backup, backup, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BackupRun
	for rows.Next() {
		r, err := scanBackupRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastSuccessfulBackups returns, for each backup, its newest run that succeeded. Failed and running runs are ignored. A
// backup that never succeeded is absent.
func (s *Store) LastSuccessfulBackups(ctx context.Context) (map[string]BackupRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+backupColumns+` FROM backups WHERE environment=? AND id IN (SELECT MAX(id) FROM backups WHERE environment=? AND status='succeeded' GROUP BY backup)`, s.env, s.env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]BackupRun{}
	for rows.Next() {
		r, err := scanBackupRun(rows)
		if err != nil {
			return nil, err
		}
		out[r.Backup] = r
	}
	return out, rows.Err()
}

// BackupHistory is what a backup's checks and ages need to know about its runs.
type BackupHistory struct {
	// Last is the newest finished run, or nil. A run that an agent stop or restart cut short is not counted: it says nothing about
	// the backup, as an interrupted job run says nothing about its job.
	Last *BackupRun
	// Successes holds the newest two successful runs, newest first.
	Successes []BackupRun
}

// BackupHistory returns the history of one backup.
func (s *Store) BackupHistory(ctx context.Context, backup string) (BackupHistory, error) {
	var h BackupHistory
	rows, err := s.db.QueryContext(ctx, `SELECT `+backupColumns+` FROM backups WHERE environment=? AND backup=? AND status='succeeded' ORDER BY id DESC LIMIT 2`, s.env, backup)
	if err != nil {
		return h, err
	}
	for rows.Next() {
		r, err := scanBackupRun(rows)
		if err != nil {
			rows.Close()
			return h, err
		}
		h.Successes = append(h.Successes, r)
	}
	if err = rows.Close(); err != nil {
		return h, err
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+backupColumns+` FROM backups WHERE environment=? AND backup=? AND (status='succeeded' OR status='failed' AND detail NOT IN (?,?)) ORDER BY id DESC LIMIT 1`, s.env, backup, StoppedBackupDetail, AbandonedBackupDetail)
	last, err := scanBackupRun(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return h, err
	default:
		h.Last = &last
	}
	return h, nil
}
