package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
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
	// Detail is a short fixed reason, such as "exit status 3". The output of the run is kept apart, for a failed run only:
	// see FinishJobRunWithOutput.
	Detail, Container string
	// HasOutput is true when output was kept for the run. JobOutput reads it.
	HasOutput bool
}

const (
	// maxJobDetail bounds the detail stored for one run.
	maxJobDetail = 512
	// maxRunsKept is how many runs of one job, backup or state backup are kept. Runs that readers need beyond that window
	// are kept too; see pruneRuns.
	maxRunsKept = 500
	// MaxJobOutput is the most bytes of output kept for one failed run.
	MaxJobOutput = 64 << 10
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
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO job_runs(environment,job,app,trigger,scheduled,started,finished,status,exit_code,detail,container) VALUES(?,?,?,?,?,?,?,?,NULL,?,?) ON CONFLICT DO NOTHING`,
		s.env, r.Job, r.App, r.Trigger, toMilli(r.Scheduled), toMilli(r.Started), toMilli(r.Finished), r.Status, r.Detail, r.Container)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 { //nolint:errcheck // The SQLite driver returns a stored row count with no error.
		return 0, ErrConflict
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	// The newest run that succeeded or failed is the one the job's check reports.
	if err = pruneRuns(ctx, tx, "job_runs", "environment=? AND job=?", "status IN ('succeeded','failed')", 1, s.env, r.Job); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// pruneRuns keeps one run history to its window. It runs inside the transaction that records a run, so a history never
// grows past its window. A history is the rows of one table that match scope, with the arguments that scope takes. Beyond
// the newest maxRunsKept of them it deletes every row except a running one and the newest keepN rows whose status matches
// keep. Those rows are what the readers of the history need, however old they are. The table, scope and keep are fixed SQL
// in this package, never input.
func pruneRuns(ctx context.Context, tx *sql.Tx, table, scope, keep string, keepN int, args ...any) error {
	query := fmt.Sprintf(`DELETE FROM %[1]s WHERE %[2]s AND status<>'running' AND id NOT IN (SELECT id FROM %[1]s WHERE %[2]s ORDER BY id DESC LIMIT %[3]d) AND id NOT IN (SELECT id FROM %[1]s WHERE %[2]s AND %[4]s ORDER BY id DESC LIMIT %[5]d)`,
		table, scope, maxRunsKept, keep, keepN)
	// The scope appears three times in the statement, so its arguments do too.
	_, err := tx.ExecContext(ctx, query, slices.Concat(args, args, args)...)
	return err
}

// FinishJobRun ends a running run with status succeeded, failed or interrupted, keeping no output. A run is interrupted when the
// agent stopped in an orderly way while it ran; like a skipped run, it says nothing about the job. It returns ErrConflict when
// the run is not running.
func (s *Store) FinishJobRun(ctx context.Context, id int64, status string, exit *int, detail string) error {
	return s.FinishJobRunWithOutput(ctx, id, status, exit, detail, "")
}

// FinishJobRunWithOutput is FinishJobRun for a run that has output to keep, which the caller has redacted: the last
// MaxJobOutput bytes of it are sealed with the run. An empty output keeps nothing.
func (s *Store) FinishJobRunWithOutput(ctx context.Context, id int64, status string, exit *int, detail, output string) error {
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
	var sealed []byte
	if output != "" {
		if len(output) > MaxJobOutput {
			output = strings.ToValidUTF8(output[len(output)-MaxJobOutput:], "")
		}
		var err error
		if sealed, err = s.seal(jobOutputRecord(strconv.FormatInt(id, 10)), []byte(output)); err != nil {
			return err
		}
	}
	res, err := s.exec(ctx, `UPDATE job_runs SET status=?,exit_code=?,detail=?,finished=?,output_sealed=? WHERE environment=? AND id=? AND status='running'`, status, code, detail, now(), sealed, s.env, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 { //nolint:errcheck // The SQLite driver returns a stored row count with no error.
		return ErrConflict
	}
	return nil
}

// JobOutput returns a run of the job and the output kept for it: the run with that id, or with id 0 the newest run of the
// job that kept output. The output is empty for a run that kept none. It returns sql.ErrNoRows when there is no such run.
func (s *Store) JobOutput(ctx context.Context, job string, id int64) (JobRun, string, error) {
	query := `SELECT ` + jobRunColumns + `,output_sealed FROM job_runs WHERE environment=? AND job=? AND id=?`
	args := []any{s.env, job, id}
	if id == 0 {
		query = `SELECT ` + jobRunColumns + `,output_sealed FROM job_runs WHERE environment=? AND job=? AND output_sealed IS NOT NULL ORDER BY id DESC LIMIT 1`
		args = args[:2]
	}
	var sealed []byte
	r, err := scanJobRun(s.queryRow(ctx, query, args...), &sealed)
	if err != nil {
		return JobRun{}, "", err
	}
	if sealed == nil {
		return r, "", nil
	}
	plain, err := s.open(jobOutputRecord(strconv.FormatInt(r.ID, 10)), sealed)
	if err != nil {
		return JobRun{}, "", err
	}
	return r, string(plain), nil
}

// AbandonRunningJobs fails every run a previous process left running, and returns how many it failed. The command may
// still be running inside its container: only the agent's wait for it ended.
func (s *Store) AbandonRunningJobs(ctx context.Context) (int, error) {
	res, err := s.exec(ctx, `UPDATE job_runs SET status='failed',detail=?,finished=? WHERE environment=? AND status='running'`, AbandonedJobDetail, now(), s.env)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

const jobRunColumns = `id,job,app,trigger,scheduled,started,finished,status,exit_code,detail,container,output_sealed IS NOT NULL`

// scanJobRun reads the jobRunColumns of a row, then the extra values after them.
func scanJobRun(row interface{ Scan(...any) error }, extra ...any) (JobRun, error) {
	var r JobRun
	var scheduled, started, finished int64
	var exit sql.NullInt64
	if err := row.Scan(append([]any{&r.ID, &r.Job, &r.App, &r.Trigger, &scheduled, &started, &finished, &r.Status, &exit, &r.Detail, &r.Container, &r.HasOutput}, extra...)...); err != nil {
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
	rows, err := s.query(ctx, `SELECT `+jobRunColumns+` FROM job_runs WHERE environment=? AND (?='' OR job=?) ORDER BY id DESC LIMIT ?`, s.env, job, job, limit)
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

// LastJobOccurrence returns the greatest retained scheduled minute for a job, regardless of status.
func (s *Store) LastJobOccurrence(ctx context.Context, job string) (time.Time, error) {
	var scheduled int64
	err := s.queryRow(ctx, `SELECT COALESCE(MAX(scheduled), 0) FROM job_runs WHERE environment=? AND job=? AND scheduled<>0`, s.env, job).Scan(&scheduled)
	if err != nil {
		return time.Time{}, err
	}
	return fromMilli(scheduled), nil
}

// LastJobRuns returns, for each job, its newest run that succeeded or failed. Skipped and interrupted runs, and runs still running, are
// ignored. A job that has none is absent.
func (s *Store) LastJobRuns(ctx context.Context) (map[string]JobRun, error) {
	rows, err := s.query(ctx, `SELECT `+jobRunColumns+` FROM job_runs WHERE environment=? AND id IN (SELECT MAX(id) FROM job_runs WHERE environment=? AND status IN ('succeeded','failed') GROUP BY job)`, s.env, s.env)
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
	res, err := s.exec(ctx, `UPDATE job_runs SET container=? WHERE environment=? AND id=? AND status='running'`, container, s.env, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 { //nolint:errcheck // The SQLite driver returns a stored row count with no error.
		return ErrConflict
	}
	return nil
}
