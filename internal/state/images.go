package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type AcquiredImage struct{ Ref, ID string }

func (s *Store) RecordImageAcquired(ctx context.Context, ref, id string) error {
	if ref == "" || id == "" {
		return errors.New("image ref and ID required")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active string
	if err = tx.QueryRowContext(ctx, `SELECT operation_id FROM locks WHERE environment=? AND operation_id IS NOT NULL`, s.env).Scan(&active); err != nil {
		return ErrConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO acquired_images VALUES(?,?,?) ON CONFLICT(environment,ref) DO NOTHING`, s.env, ref, id)
	if err != nil {
		return err
	}
	var stored string
	if err = tx.QueryRowContext(ctx, `SELECT image_id FROM acquired_images WHERE environment=? AND ref=?`, s.env, ref).Scan(&stored); err != nil {
		return err
	}
	if stored != id {
		return ErrConflict
	}
	return tx.Commit()
}
func (s *Store) AcquiredImages(ctx context.Context) ([]AcquiredImage, error) {
	rows, err := s.query(ctx, `SELECT ref,image_id FROM acquired_images WHERE environment=? ORDER BY ref`, s.env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AcquiredImage
	for rows.Next() {
		var i AcquiredImage
		if err = rows.Scan(&i.Ref, &i.ID); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
func (s *Store) ForgetImage(ctx context.Context, ref, id string) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active string
	if err = tx.QueryRowContext(ctx, `SELECT operation_id FROM locks WHERE environment=? AND operation_id IS NOT NULL`, s.env).Scan(&active); err != nil {
		return ErrConflict
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM acquired_images WHERE environment=? AND ref=? AND image_id=?`, s.env, ref, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

type CompletedOneOff struct{ OperationID, ID, Name, Role string }

// CompletedOneOffs lists the one-off containers of finished operations that Berthy may prune: the hooks and migration
// commands that finished, and any one-off that created a container but never finished, such as one that timed out. Their
// output lives on in the operation's diagnostics. A step that never created a container has none to list.
func (s *Store) CompletedOneOffs(ctx context.Context) ([]CompletedOneOff, error) {
	rows, err := s.query(ctx, `SELECT i.operation_id,i.ordinal,i.name,i.role,i.phase,i.receipt FROM intents i JOIN operations o ON o.id=i.operation_id WHERE o.environment=? AND o.state IN ('succeeded','failed','rolled_back','interrupted') AND i.kind='oneoff' AND (i.phase<>'finished' OR i.role IN ('pre_hook','post_hook','schema_before','migration_apply','schema_after','run')) ORDER BY i.operation_id,i.ordinal`, s.env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CompletedOneOff
	for rows.Next() {
		var x CompletedOneOff
		var ordinal int
		var phase string
		var sealed []byte
		if err = rows.Scan(&x.OperationID, &ordinal, &x.Name, &x.Role, &phase, &sealed); err != nil {
			return nil, err
		}
		plain, e := s.open(intentRecord(x.OperationID, ordinal, "receipt"), sealed)
		if e != nil {
			return nil, e
		}
		var receipt struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(plain, &receipt) != nil || receipt.ID == "" && phase != "intent" && phase != "created" {
			return nil, errors.New("invalid one-off receipt")
		}
		if receipt.ID == "" {
			continue
		}
		x.ID = receipt.ID
		out = append(out, x)
	}
	return out, rows.Err()
}

type HistoryOperation struct {
	OperationID, Kind, App, Commit, State  string
	SchemaBefore, SchemaAfter, Reason, Tag string
	Modified                               bool
	Started                                time.Time
}

func (s *Store) DeployHistory(ctx context.Context, app string) ([]HistoryOperation, error) {
	rows, err := s.query(ctx, `SELECT id,kind,app,commit_sha,state,started,schema_before,schema_after,reason,modified,public_request FROM operations WHERE environment=? AND app=? AND state IN ('failed','rolled_back','interrupted') ORDER BY started DESC,id DESC`, s.env, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HistoryOperation
	for rows.Next() {
		var o HistoryOperation
		var ms int64
		var public []byte
		if err = rows.Scan(&o.OperationID, &o.Kind, &o.App, &o.Commit, &o.State, &ms, &o.SchemaBefore, &o.SchemaAfter, &o.Reason, &o.Modified, &public); err != nil {
			return nil, err
		}
		var selection struct {
			Tag string `json:"version_tag"`
		}
		_ = json.Unmarshal(public, &selection)
		o.Tag = selection.Tag
		o.Started = time.UnixMilli(ms).UTC()
		out = append(out, o)
	}
	return out, rows.Err()
}
