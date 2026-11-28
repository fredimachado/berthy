package state

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"berthy/internal/manifest"
)

const terminalStates = `('succeeded','failed','rolled_back','interrupted')`

const recordedMigration = `kind='deploy' AND schema_before<>'' AND state IN ` + terminalStates
const latestMigrationSelection = recordedMigration + ` ORDER BY started DESC,rowid DESC LIMIT 1`

const retentionRelease = `SELECT manifest FROM releases WHERE id=COALESCE(
	(SELECT release_id FROM applied WHERE environment=?1 AND app=?2),
	(SELECT id FROM releases WHERE environment=?1 AND app=?2 ORDER BY id DESC LIMIT 1))`

func releaseRetention(row interface{ Scan(...any) error }, app string) (int, error) {
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return manifest.DefaultReleaseRetention, nil
		}
		return 0, err
	}
	var stored struct {
		Apps map[string]struct{ Deploy manifest.Deploy }
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return 0, err
	}
	return stored.Apps[app].Deploy.ReleaseRetention(), nil
}

// CommittedMaintenanceError means that the outcome was committed and history cleanup is pending.
type CommittedMaintenanceError struct{ Err error }

func (e *CommittedMaintenanceError) Error() string {
	return "operation committed with history cleanup pending"
}
func (e *CommittedMaintenanceError) Unwrap() error { return e.Err }

func historyPendingPrefix(env string) string {
	return "history_pending:" + hex.EncodeToString([]byte(env)) + ":"
}

func (s *Store) commitWithHistory(ctx context.Context, tx *sql.Tx, app string, retryPending bool) error {
	apps := []string{app}
	var maintenance error
	if retryPending {
		pending, err := historyStrings(tx.QueryContext(ctx, `SELECT CAST(value AS TEXT) FROM metadata WHERE name GLOB ? ORDER BY name`, historyPendingPrefix(s.env)+"*"))
		maintenance = err
		for _, old := range pending {
			if old != app {
				apps = append(apps, old)
			}
		}
	}
	for _, bucket := range apps {
		pending, err := s.cleanHistoryBucket(ctx, tx, bucket)
		if err != nil {
			return err
		}
		maintenance = errors.Join(maintenance, pending)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if maintenance != nil {
		return &CommittedMaintenanceError{Err: maintenance}
	}
	return nil
}

// A bucket has its own savepoint so broken evidence cannot block cleanup for another app.
func (s *Store) cleanHistoryBucket(ctx context.Context, tx *sql.Tx, app string) (maintenance, err error) {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT trim_history`); err != nil {
		return nil, err
	}
	key := historyPendingPrefix(s.env) + app
	maintenance = s.trimHistory(ctx, tx, s.env, app)
	if maintenance == nil {
		_, maintenance = tx.ExecContext(ctx, `DELETE FROM metadata WHERE name=?`, key)
	}
	if maintenance != nil {
		if _, err := tx.ExecContext(ctx, `ROLLBACK TO trim_history`); err != nil {
			return nil, errors.Join(maintenance, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO metadata(name,value) VALUES(?,?) ON CONFLICT(name) DO NOTHING`, key, []byte(app)); err != nil {
			return nil, errors.Join(maintenance, err)
		}
	}
	_, err = tx.ExecContext(ctx, `RELEASE trim_history`)
	return maintenance, err
}

func historyStrings(rows *sql.Rows, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

func (s *Store) trimHistory(ctx context.Context, tx *sql.Tx, env, app string) error {
	keep, err := releaseRetention(tx.QueryRowContext(ctx, retentionRelease, env, app), app)
	if err != nil {
		return err
	}
	if err := s.trimReleases(ctx, tx, env, app, keep); err != nil {
		return err
	}
	return s.trimOperations(ctx, tx, env, app, keep)
}

func (s *Store) trimReleases(ctx context.Context, tx *sql.Tx, env, app string, keep int) error {
	for {
		victims, err := releaseVictims(ctx, tx, env, app, keep)
		if err != nil || len(victims) == 0 {
			return err
		}
		for _, r := range victims {
			if err := s.rememberRelease(ctx, tx, r); err != nil {
				return err
			}
			if r.OperationID != "" {
				if _, err := tx.ExecContext(ctx, `UPDATE audit SET retired_app=?,retired_release_id=? WHERE environment=? AND operation_id=? AND outcome='accepted'`, app, r.ID, env, r.OperationID); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM releases WHERE environment=? AND id=?`, env, r.ID); err != nil {
				return err
			}
		}
	}
}

func releaseVictims(ctx context.Context, tx *sql.Tx, env, app string, keep int) ([]Release, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,manifest,operation_id FROM releases WHERE environment=?1 AND app=?2
		AND id<(SELECT id FROM releases WHERE environment=?1 AND app=?2 ORDER BY id DESC LIMIT 1 OFFSET ?3)
		AND id<>COALESCE((SELECT release_id FROM applied WHERE environment=?1 AND app=?2),0) ORDER BY id LIMIT 128`, env, app, keep-1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var victims []Release
	for rows.Next() {
		r := Release{App: app}
		if err := rows.Scan(&r.ID, &r.Manifest, &r.OperationID); err != nil {
			return nil, err
		}
		victims = append(victims, r)
	}
	return victims, rows.Err()
}

// ReleaseWasRetired reports whether this app and environment recorded and then retired the release.
func (s *Store) ReleaseWasRetired(ctx context.Context, app string, id int64) (bool, error) {
	var retired bool
	err := s.queryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit WHERE environment=? AND retired_app=? AND retired_release_id=?)`, s.env, app, id).Scan(&retired)
	return retired, err
}

// LatestRetiredReleaseBefore returns the most recent retired release older than id, or zero if none exists.
func (s *Store) LatestRetiredReleaseBefore(ctx context.Context, app string, id int64) (int64, error) {
	var previous int64
	err := s.queryRow(ctx, `SELECT COALESCE(MAX(retired_release_id),0) FROM audit WHERE environment=? AND retired_app=? AND retired_release_id<?`, s.env, app, id).Scan(&previous)
	return previous, err
}

func publicRequestTag(public []byte) (string, error) {
	var selection struct {
		Tag string `json:"version_tag"`
	}
	err := json.Unmarshal(public, &selection)
	return selection.Tag, err
}

const retiredOperationSelection = `SELECT a.operation_id,a.retired_request_id,a.actor_id,a.actor_name,a.remote,a.action,a.retired_app,a.retired_request_hash,a.created,t.action,t.outcome
	FROM audit a LEFT JOIN audit t ON t.id=(SELECT id FROM audit WHERE environment=a.environment AND operation_id=a.operation_id
	AND ((action=a.action AND outcome IN ` + terminalStates + `) OR (action='recovery_resolve' AND outcome='succeeded')) ORDER BY id DESC LIMIT 1)
	WHERE a.environment=? AND a.retired_request_id IS NOT NULL AND `

func retiredOperation(row interface{ Scan(...any) error }) (Operation, error) {
	var op Operation
	var started int64
	var hash []byte
	var action string
	if err := row.Scan(&op.ID, &op.RequestID, &op.ActorID, &op.ActorName, &op.Remote, &op.Kind, &op.App, &hash, &started, &action, &op.State); err != nil {
		return Operation{}, err
	}
	if len(hash) != len(op.RequestHash) {
		return Operation{}, errors.New("invalid retired request hash")
	}
	copy(op.RequestHash[:], hash)
	op.Started = time.UnixMilli(started).UTC()
	if action == "recovery_resolve" {
		op.State, op.Err = "failed", "recovery resolved by admin"
	}
	return op, nil
}
