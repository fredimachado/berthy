package state

import (
	"context"
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
	db, err := s.handle()
	if err != nil {
		return nil, err
	}
	if s.hold != nil {
		s.hold()
	}
	rows, err := db.QueryContext(ctx, `SELECT kind,name FROM claims WHERE environment=? ORDER BY kind,name`, s.env)
	if err != nil {
		return nil, s.closed(err)
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
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
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
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return added, nil
}

func (s *Store) ReleaseClaims(ctx context.Context, claims []Claim) error {
	return s.changeClaims(ctx, claims, `DELETE FROM claims WHERE environment=? AND kind=? AND name=?`)
}

func (s *Store) changeClaims(ctx context.Context, claims []Claim, query string) error {
	tx, err := s.begin(ctx)
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
