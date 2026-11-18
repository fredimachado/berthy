package state

import (
	"context"
	"encoding/json"
	"errors"
)

const (
	// DiagnosticOperations is how many operations keep their diagnostics. Older ones are deleted when newer ones arrive.
	DiagnosticOperations = 50
	maxDiagnosticItems   = 128
	maxDiagnosticBytes   = 2 << 20
)

// PutDiagnostics appends sealed diagnostics to the operation that holds the environment lock, then drops the diagnostics of
// every operation beyond the newest DiagnosticOperations that have any.
func (s *Store) PutDiagnostics(ctx context.Context, opID string, items []json.RawMessage) error {
	if len(items) == 0 {
		return nil
	}
	total := 0
	for _, item := range items {
		if !validJSON(item) {
			return errors.New("invalid diagnostic")
		}
		total += len(item)
	}
	if len(items) > maxDiagnosticItems || total > maxDiagnosticBytes {
		return errors.New("diagnostics too large")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = owned(ctx, tx, s.env, opID); err != nil {
		return err
	}
	var next int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM diagnostics WHERE operation_id=?`, opID).Scan(&next); err != nil {
		return err
	}
	if next+len(items) > maxDiagnosticItems {
		return errors.New("diagnostics too large")
	}
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq)+1,0) FROM diagnostics WHERE operation_id=?`, opID).Scan(&next); err != nil {
		return err
	}
	for i, item := range items {
		sealed, e := s.seal(diagnosticRecord(opID, next+i), item)
		if e != nil {
			return e
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO diagnostics VALUES(?,?,?,?)`, opID, next+i, sealed, now()); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM diagnostics WHERE operation_id IN (SELECT o.id FROM operations o WHERE o.environment=? AND o.id IN (SELECT operation_id FROM diagnostics) ORDER BY o.started DESC, o.rowid DESC LIMIT -1 OFFSET ?)`, s.env, DiagnosticOperations)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Diagnostics returns the diagnostics stored for an operation of this environment, oldest first. An operation without any
// has none.
func (s *Store) Diagnostics(ctx context.Context, opID string) ([]json.RawMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.seq,d.sealed FROM diagnostics d JOIN operations o ON o.id=d.operation_id WHERE o.environment=? AND o.id=? ORDER BY d.seq`, s.env, opID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var seq int
		var sealed []byte
		if err = rows.Scan(&seq, &sealed); err != nil {
			return nil, err
		}
		plain, e := s.open(diagnosticRecord(opID, seq), sealed)
		if e != nil {
			return nil, e
		}
		out = append(out, json.RawMessage(plain))
	}
	return out, rows.Err()
}
