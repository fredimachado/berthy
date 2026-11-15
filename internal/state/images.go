package state

import (
	"context"
	"database/sql"
	"errors"
)

type AcquiredImage struct{ Ref, ID string }

func (s *Store) RecordImageAcquired(ctx context.Context, ref, id string) error {
	if ref == "" || id == "" {
		return errors.New("image ref and ID required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
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
	rows, err := s.db.QueryContext(ctx, `SELECT ref,image_id FROM acquired_images WHERE environment=? ORDER BY ref`, s.env)
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
	tx, err := s.db.BeginTx(ctx, nil)
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
