package state

import "context"

// Claim is a Docker name this environment holds.
type Claim struct{ Kind, Name string }

const (
	ClaimService        = "service"
	ClaimComposeProject = "compose"
	ClaimNetwork        = "network"
	ClaimVolume         = "volume"
)

func (s *Store) Claims(ctx context.Context) ([]Claim, error) {
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
	return s.changeClaims(ctx, claims, `INSERT OR IGNORE INTO claims(environment,kind,name) VALUES(?,?,?)`)
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
