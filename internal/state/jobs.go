package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// JobRun is one run of a job: a scheduled occurrence, a manual run or a run that prepares a backup.
type JobRun struct {
	ID        int64
	Job, App  string
	Trigger   string    // "schedule", "manual" or "backup"
	Scheduled time.Time // the occurrence minute; zero for manual and backup runs
	// Started is when the run began; Finished is zero while it runs.
	Started, Finished time.Time
	Status            string // running, succeeded, failed, skipped or interrupted
	ExitCode          *int
	// Detail is a short fixed reason, such as "exit status 3". The command's output is never stored.
	Detail, Container string
}

const (
	// maxJobDetail bounds the detail stored for one run.
	maxJobDetail = 512
	// maxJobRuns is how many runs of one job are kept. The newest finished run that was not skipped is always kept.
	maxJobRuns = 500
	// AbandonedJobDetail is the detail of a run that a previous agent process left running.
	AbandonedJobDetail = "agent restarted during the run"
)

var (
	jobTriggers = map[string]bool{"schedule": true, "manual": true, "backup": true}
)

// StartJobRun records a run that has begun, or with Status skipped, one that was skipped and is finished already. An
// empty Status means running. A second run of the same scheduled occurrence returns ErrConflict, so each occurrence
// runs at most once even when two processes race for it. Manual and backup runs have no occurrence and never conflict.
func (s *Store) StartJobRun(ctx context.Context, r JobRun) (int64, error) {
	if r.Status == "" {
		r.Status = "running"
	}
	if r.Job == "" || r.App == "" || !jobTriggers[r.Trigger] || r.Started.IsZero() || (r.Trigger == "schedule") == r.Scheduled.IsZero() {
		return 0, errors.New("invalid job run")
	}
	switch r.Status {
	case "running":
		r.Finished = time.Time{}
	case "skipped":
		if r.Finished.IsZero() {
			r.Finished = r.Started
		}
	default:
		return 0, errors.New("invalid job run")
	}
	if len(r.Detail) > maxJobDetail {
		r.Detail = r.Detail[:maxJobDetail]
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO job_runs(environment,job,app,trigger,scheduled,started,finished,status,exit_code,detail,container) VALUES(?,?,?,?,?,?,?,?,NULL,?,?) ON CONFLICT DO NOTHING`,
		s.env, r.Job, r.App, r.Trigger, toMilli(r.Scheduled), toMilli(r.Started), toMilli(r.Finished), r.Status, r.Detail, r.Container)
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
	// Keep the newest maxJobRuns runs of the job, and the newest run that succeeded or failed, which the job's
	// check reports.
	if _, err = tx.ExecContext(ctx, `DELETE FROM job_runs WHERE environment=? AND job=? AND id <= COALESCE((SELECT id FROM job_runs WHERE environment=? AND job=? ORDER BY id DESC LIMIT 1 OFFSET ?),0) AND id <> COALESCE((SELECT MAX(id) FROM job_runs WHERE environment=? AND job=? AND status IN ('succeeded','failed')),0)`,
		s.env, r.Job, s.env, r.Job, maxJobRuns, s.env, r.Job); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// FinishJobRun ends a running run with status succeeded, failed or interrupted. A run is interrupted when the agent stopped
// in an orderly way while it ran; like a skipped run, it says nothing about the job. It returns ErrConflict when the run
// is not running.
func (s *Store) FinishJobRun(ctx context.Context, id int64, status string, exit *int, detail string) error {
	if status != "succeeded" && status != "failed" && status != "interrupted" {
		return errors.New("invalid job run status")
	}
	if len(detail) > maxJobDetail {
		detail = detail[:maxJobDetail]
	}
	var code any
	if exit != nil {
		code = *exit
	}
	res, err := s.db.ExecContext(ctx, `UPDATE job_runs SET status=?,exit_code=?,detail=?,finished=? WHERE environment=? AND id=? AND status='running'`, status, code, detail, now(), s.env, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	return nil
}

// AbandonRunningJobs fails every run a previous process left running, and returns how many it failed. The command may
// still be running inside its container: only the agent's wait for it ended.
func (s *Store) AbandonRunningJobs(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE job_runs SET status='failed',detail=?,finished=? WHERE environment=? AND status='running'`, AbandonedJobDetail, now(), s.env)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

const jobRunColumns = `id,job,app,trigger,scheduled,started,finished,status,exit_code,detail,container`

func scanJobRun(row interface{ Scan(...any) error }) (JobRun, error) {
	var r JobRun
	var scheduled, started, finished int64
	var exit sql.NullInt64
	if err := row.Scan(&r.ID, &r.Job, &r.App, &r.Trigger, &scheduled, &started, &finished, &r.Status, &exit, &r.Detail, &r.Container); err != nil {
		return JobRun{}, err
	}
	r.Scheduled, r.Started, r.Finished = fromMilli(scheduled), fromMilli(started), fromMilli(finished)
	if exit.Valid {
		code := int(exit.Int64)
		r.ExitCode = &code
	}
	return r, nil
}

// JobRuns returns the newest runs of a job, newest first. An empty job means every job.
func (s *Store) JobRuns(ctx context.Context, job string, limit int) ([]JobRun, error) {
	if limit < 1 {
		return nil, errors.New("invalid limit")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobRunColumns+` FROM job_runs WHERE environment=? AND (?='' OR job=?) ORDER BY id DESC LIMIT ?`, s.env, job, job, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobRun
	for rows.Next() {
		r, err := scanJobRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastJobRuns returns, for each job, its newest run that succeeded or failed. Skipped and interrupted runs, and runs still running, are
// ignored. A job that has none is absent.
func (s *Store) LastJobRuns(ctx context.Context) (map[string]JobRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobRunColumns+` FROM job_runs WHERE environment=? AND id IN (SELECT MAX(id) FROM job_runs WHERE environment=? AND status IN ('succeeded','failed') GROUP BY job)`, s.env, s.env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]JobRun{}
	for rows.Next() {
		r, err := scanJobRun(rows)
		if err != nil {
			return nil, err
		}
		out[r.Job] = r
	}
	return out, rows.Err()
}

// SetJobRunContainer records the container a running run executes in. It returns ErrConflict when the run is not running.
func (s *Store) SetJobRunContainer(ctx context.Context, id int64, container string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE job_runs SET container=? WHERE environment=? AND id=? AND status='running'`, container, s.env, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	return nil
}
