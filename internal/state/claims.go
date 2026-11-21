package state

import (
	"context"
	"encoding/json"
	"errors"
)

// Claim is a Docker name this environment holds.
type Claim struct{ Kind, Name string }

const (
	ClaimService        = "service"
	ClaimComposeProject = "compose"
	ClaimNetwork        = "network"
	ClaimVolume         = "volume"
)

func (s *Store) Claims(ctx context.Context) ([]Claim, error) {
	if s.db == nil {
		return nil, errors.New("state is closed")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT kind,name FROM claims WHERE environment=? ORDER BY kind,name`, s.env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var claims []Claim
	for rows.Next() {
		var c Claim
		if err := rows.Scan(&c.Kind, &c.Name); err != nil {
			return nil, err
		}
		claims = append(claims, c)
	}
	return claims, rows.Err()
}

func (s *Store) AddClaims(ctx context.Context, claims []Claim) error {
	_, err := s.AddClaimsNew(ctx, claims)
	return err
}

// AddClaimsNew adds claims and returns only those inserted by this transaction.
func (s *Store) AddClaimsNew(ctx context.Context, claims []Claim) ([]Claim, error) {
	return s.AddClaimsForOperation(ctx, "", claims)
}

// AddClaimsForOperation journals inserted claims in the same transaction as their reservation.
func (s *Store) AddClaimsForOperation(ctx context.Context, opID string, claims []Claim) ([]Claim, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if opID != "" {
		if err := owned(ctx, tx, s.env, opID); err != nil {
			return nil, err
		}
	}
	var added []Claim
	for _, c := range claims {
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO claims(environment,kind,name) VALUES(?,?,?)`, s.env, c.Kind, c.Name)
		if err != nil {
			return nil, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n != 0 {
			added = append(added, c)
		}
	}
	if opID != "" && len(added) > 0 {
		var ordinal int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(ordinal),999999)+1 FROM intents WHERE operation_id=? AND kind='claims'`, opID).Scan(&ordinal); err != nil {
			return nil, err
		}
		raw, err := json.Marshal(added)
		if err != nil {
			return nil, err
		}
		before, err := s.seal(intentRecord(opID, ordinal, "before"), nil)
		if err != nil {
			return nil, err
		}
		desired, err := s.seal(intentRecord(opID, ordinal, "desired"), raw)
		if err != nil {
			return nil, err
		}
		receipt, err := s.seal(intentRecord(opID, ordinal, "receipt"), []byte(`{}`))
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO intents VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, opID, ordinal, "claims", "", "", "", "", 0, 0, before, desired, "intent", receipt)
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return added, nil
}

func (s *Store) ReleaseClaims(ctx context.Context, claims []Claim) error {
	return s.changeClaims(ctx, claims, `DELETE FROM claims WHERE environment=? AND kind=? AND name=?`)
}

func (s *Store) changeClaims(ctx context.Context, claims []Claim, query string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range claims {
		if _, err := tx.ExecContext(ctx, query, s.env, c.Kind, c.Name); err != nil {
			return err
		}
	}
	return tx.Commit()
}
