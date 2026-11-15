package state

import (
	"context"
	"errors"
	"fmt"
	"time"

	"berthy/internal/manifest"
)

// CheckState is the stored state of one monitored check. State is ok, warning, critical or unknown, and Since is when
// State began. NotifiedState is the last state an alert was delivered for, empty when none was; Notified is when that
// alert, or the latest reminder for it, was delivered.
type CheckState struct {
	Name, State, Detail string
	Since               time.Time
	NotifiedState       string
	Notified            time.Time
}

// maxCheckDetail bounds the detail stored for one check.
const maxCheckDetail = 1024

var checkStates = map[string]bool{"ok": true, "warning": true, "critical": true, "unknown": true}

func fromMilli(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func toMilli(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// Checks returns the stored state of every check, sorted by name.
func (s *Store) Checks(ctx context.Context) ([]CheckState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name,state,detail,since,notified_state,notified FROM checks WHERE environment=? ORDER BY name`, s.env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheckState
	for rows.Next() {
		var c CheckState
		var since, notified int64
		if err = rows.Scan(&c.Name, &c.State, &c.Detail, &since, &c.NotifiedState, &notified); err != nil {
			return nil, err
		}
		c.Since = fromMilli(since)
		c.Notified = fromMilli(notified)
		out = append(out, c)
	}
	return out, rows.Err()
}

// PutCheck stores the state of a check, replacing any stored state with the same name. Monitoring is not an operation,
// so it needs no environment lock.
func (s *Store) PutCheck(ctx context.Context, c CheckState) error {
	if c.Name == "" || len(c.Name) > 200 || !checkStates[c.State] || c.NotifiedState != "" && !checkStates[c.NotifiedState] || len(c.Detail) > maxCheckDetail {
		return errors.New("invalid check state")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO checks VALUES(?,?,?,?,?,?,?) ON CONFLICT(environment,name) DO UPDATE SET state=excluded.state,detail=excluded.detail,since=excluded.since,notified_state=excluded.notified_state,notified=excluded.notified`, s.env, c.Name, c.State, c.Detail, toMilli(c.Since), c.NotifiedState, toMilli(c.Notified))
	return err
}

// DeleteCheck removes the stored state of a check that is no longer configured. A missing check is not an error.
func (s *Store) DeleteCheck(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM checks WHERE environment=? AND name=?`, s.env, name)
	return err
}

// LatestManifest returns the manifest of the newest release of any app except the ingress connector, and when it was
// applied. The monitor reads its probes, peers and alerts from it. The connector's release carries only the connector,
// so it is never the source. It returns sql.ErrNoRows when nothing is deployed.
func (s *Store) LatestManifest(ctx context.Context) (manifest.Manifest, time.Time, error) {
	var raw []byte
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT manifest,created FROM releases WHERE environment=? AND app<>? ORDER BY id DESC LIMIT 1`, s.env, manifest.ConnectorAppName).Scan(&raw, &created)
	if err != nil {
		return manifest.Manifest{}, time.Time{}, err
	}
	m, err := manifest.DecodeRelease(raw, false)
	if err != nil {
		return manifest.Manifest{}, time.Time{}, fmt.Errorf("stored manifest unreadable: %w", err)
	}
	return *m, fromMilli(created), nil
}

// RecordAudit records an action that starts no operation, such as a test alert. outcome is succeeded or failed.
func (s *Store) RecordAudit(ctx context.Context, actor Actor, action, outcome string) error {
	if actor.ID == "" || action == "" || outcome == "" {
		return errors.New("actor, action and outcome required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = auditTx(ctx, tx, s.env, "", Admission{ActorID: actor.ID, ActorName: actor.Name, Remote: actor.Remote, Kind: action}, outcome); err != nil {
		return err
	}
	return tx.Commit()
}
