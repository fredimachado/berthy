package state

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"time"
)

// ErrWrongKey means the key file does not open this state: the key check row does not decrypt with it. The text carries no
// key material, so it may be shown.
var ErrWrongKey = errors.New("the key does not open this state")

// StateBackupRun is one run of the agent's state backup: a snapshot of the database, uploaded with its record. Trigger is
// schedule or manual. Scheduled is the UTC minute of a scheduled run, and zero for a manual one.
type StateBackupRun struct {
	ID                int64
	Trigger           string
	Scheduled         time.Time
	Started, Finished time.Time // Finished is zero while the run is running
	Status            string    // running, succeeded or failed
	Key               string    // object key of the snapshot
	Size              int64
	SHA256, Detail    string
}

const (
	// AbandonedStateBackupDetail is the detail of a run that a previous agent process left running.
	AbandonedStateBackupDetail = "agent restarted during the state backup"
	// StoppedStateBackupDetail is the detail of a run that an orderly agent stop cut short.
	StoppedStateBackupDetail = "agent stopped during the state backup"
)

// StateBackupHistory is what the state backup's check and listing need to know about its runs.
type StateBackupHistory struct {
	// Last is the newest finished run, or nil. A run that an agent stop or restart cut short is not counted, as for volume backups.
	Last *StateBackupRun
	// Success is the newest run that succeeded, or nil.
	Success *StateBackupRun
}

const stateBackupColumns = `id,trigger,scheduled,started,finished,status,object_key,size,sha256,detail`

func scanStateBackupRun(row interface{ Scan(...any) error }) (StateBackupRun, error) {
	var r StateBackupRun
	var scheduled, started, finished int64
	if err := row.Scan(&r.ID, &r.Trigger, &scheduled, &started, &finished, &r.Status, &r.Key, &r.Size, &r.SHA256, &r.Detail); err != nil {
		return StateBackupRun{}, err
	}
	r.Scheduled, r.Started, r.Finished = fromMilli(scheduled), fromMilli(started), fromMilli(finished)
	return r, nil
}

// StartStateBackup records a run that has begun. A scheduled run's occurrence is recorded once per environment, so a second
// record of the same occurrence, from this process or another, returns ErrConflict. A manual run has no occurrence.
func (s *Store) StartStateBackup(ctx context.Context, r StateBackupRun) (int64, error) {
	if r.Key == "" || r.Started.IsZero() || (r.Trigger != "schedule" && r.Trigger != "manual") || (r.Trigger == "schedule") == r.Scheduled.IsZero() {
		return 0, errors.New("invalid state backup run")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO state_backups(environment,trigger,scheduled,started,finished,status,object_key,size,sha256,detail) VALUES(?,?,?,?,0,'running',?,0,'','') ON CONFLICT DO NOTHING`,
		s.env, r.Trigger, toMilli(r.Scheduled), toMilli(r.Started), r.Key)
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
	if err = pruneRuns(ctx, tx, "state_backups", "environment=?", "status='succeeded'", 1, s.env); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// FinishStateBackup ends a running run with status succeeded or failed. A succeeded run needs the snapshot's SHA-256. It returns
// ErrConflict when the run is not running, so a run is finished at most once.
func (s *Store) FinishStateBackup(ctx context.Context, id int64, status string, size int64, sha256, detail string) error {
	if status != "succeeded" && status != "failed" {
		return errors.New("invalid state backup status")
	}
	if size < 0 || (status == "succeeded" && len(sha256) != 64) {
		return errors.New("invalid state backup result")
	}
	if len(detail) > maxBackupDetail {
		detail = detail[:maxBackupDetail]
	}
	res, err := s.db.ExecContext(ctx, `UPDATE state_backups SET status=?,size=?,sha256=?,detail=?,finished=? WHERE environment=? AND id=? AND status='running'`,
		status, size, sha256, detail, now(), s.env, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	return nil
}

// AbandonRunningStateBackups fails every state backup run that a previous process left running, and returns how many.
func (s *Store) AbandonRunningStateBackups(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE state_backups SET status='failed',detail=?,finished=? WHERE environment=? AND status='running'`, AbandonedStateBackupDetail, now(), s.env)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// StateBackupRuns returns the newest runs of the state backup, newest first.
func (s *Store) StateBackupRuns(ctx context.Context, limit int) ([]StateBackupRun, error) {
	if limit < 1 {
		return nil, errors.New("invalid limit")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+stateBackupColumns+` FROM state_backups WHERE environment=? ORDER BY id DESC LIMIT ?`, s.env, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StateBackupRun
	for rows.Next() {
		r, err := scanStateBackupRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// StateBackupHistory returns the last finished and the last successful run of the state backup.
func (s *Store) StateBackupHistory(ctx context.Context) (StateBackupHistory, error) {
	var h StateBackupHistory
	success, err := scanStateBackupRun(s.db.QueryRowContext(ctx, `SELECT `+stateBackupColumns+` FROM state_backups WHERE environment=? AND status='succeeded' ORDER BY id DESC LIMIT 1`, s.env))
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return h, err
	default:
		h.Success = &success
	}
	last, err := scanStateBackupRun(s.db.QueryRowContext(ctx, `SELECT `+stateBackupColumns+` FROM state_backups WHERE environment=? AND (status='succeeded' OR status='failed' AND detail NOT IN (?,?)) ORDER BY id DESC LIMIT 1`, s.env, StoppedStateBackupDetail, AbandonedStateBackupDetail))
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return h, err
	default:
		h.Last = &last
	}
	return h, nil
}

// Snapshot writes a consistent copy of the database to path, which must not exist yet. The copy holds every secret still sealed
// and never the key, which is not in the database. It is taken while the agent keeps running. It returns the ID of the key
// the copy is sealed with, which is the key a restore needs. A rotation cannot run between the copy and that answer: both
// happen while this call holds the store's only connection.
func (s *Store) Snapshot(ctx context.Context, path string) (keyID string, err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	keyID = s.KeyID()
	// The file is created here, private, because VACUUM INTO accepts only a path that is missing or an empty file.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	if _, err = conn.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		os.Remove(path)
		return "", err
	}
	return keyID, nil
}

// SchemaVersion returns the database's schema version, which the snapshot records.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v)
	return v, err
}
