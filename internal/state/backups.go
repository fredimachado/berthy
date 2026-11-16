package state

import (
	"context"
	"errors"
	"regexp"
	"time"
)

// BackupRun is one run of a volume backup: archive the volume, upload the archive and record the outcome.
type BackupRun struct {
	ID                               int64
	Backup, App, Volume, Destination string
	ObjectKey, Trigger               string // Trigger is schedule, manual or hook
	// Started is when the run began; Finished is zero while it runs.
	Started, Finished time.Time
	Status            string // running, succeeded or failed
	Size              int64
	SHA256, Detail    string
}

const (
	// AbandonedBackupDetail is the detail of a run that a previous agent process left running.
	AbandonedBackupDetail = "agent restarted during the backup"
	// maxBackupDetail bounds the detail stored for one run.
	maxBackupDetail = 512
)

var (
	backupTriggers = map[string]bool{"schedule": true, "manual": true, "hook": true}
	backupSHA256   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// StartBackup records a backup run that has begun. A run starts as running; any other status is refused. Each object key
// is recorded once per environment, whatever the run's status, so a key that is already recorded returns ErrConflict.
func (s *Store) StartBackup(ctx context.Context, b BackupRun) (int64, error) {
	if b.Status != "" && b.Status != "running" {
		return 0, errors.New("invalid backup run")
	}
	if b.Backup == "" || b.App == "" || b.Volume == "" || b.Destination == "" || b.ObjectKey == "" || !backupTriggers[b.Trigger] || b.Started.IsZero() {
		return 0, errors.New("invalid backup run")
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO backups(environment,backup,app,volume,destination,object_key,trigger,started,finished,status,size,sha256,detail) VALUES(?,?,?,?,?,?,?,?,0,'running',0,'','') ON CONFLICT DO NOTHING`,
		s.env, b.Backup, b.App, b.Volume, b.Destination, b.ObjectKey, b.Trigger, toMilli(b.Started))
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrConflict
	}
	return res.LastInsertId()
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

const backupColumns = `id,backup,app,volume,destination,object_key,trigger,started,finished,status,size,sha256,detail`

func scanBackupRun(row interface{ Scan(...any) error }) (BackupRun, error) {
	var r BackupRun
	var started, finished int64
	if err := row.Scan(&r.ID, &r.Backup, &r.App, &r.Volume, &r.Destination, &r.ObjectKey, &r.Trigger, &started, &finished, &r.Status, &r.Size, &r.SHA256, &r.Detail); err != nil {
		return BackupRun{}, err
	}
	r.Started, r.Finished = fromMilli(started), fromMilli(finished)
	return r, nil
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
