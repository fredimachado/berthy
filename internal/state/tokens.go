package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// A Token is an access token made through the agent's API. The state holds its digest, never its value.
type Token struct {
	Name, Digest, Scope string
	// Agent marks the token of an AI agent.
	Agent   bool
	Created time.Time
	// Expires is when the token stops working, or nil for a token that never does.
	Expires *time.Time
}

// tokenColumns are the columns of the tokens table, in the order scanToken reads them.
const tokenColumns = `name,digest,scope,agent,created,expires`

func scanToken(row interface{ Scan(...any) error }) (Token, error) {
	var t Token
	var agent int
	var created int64
	var expires sql.NullInt64
	if err := row.Scan(&t.Name, &t.Digest, &t.Scope, &agent, &created, &expires); err != nil {
		return Token{}, err
	}
	t.Agent = agent != 0
	t.Created = time.UnixMilli(created).UTC()
	if expires.Valid {
		e := time.UnixMilli(expires.Int64).UTC()
		t.Expires = &e
	}
	return t, nil
}

func (s *Store) tokens(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]Token, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+tokenColumns+` FROM tokens WHERE environment=? ORDER BY created, name`, s.env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Tokens returns the tokens made through the API, oldest first.
func (s *Store) Tokens(ctx context.Context) ([]Token, error) {
	db, err := s.handle()
	if err != nil {
		return nil, err
	}
	tokens, err := s.tokens(ctx, db)
	return tokens, s.closed(err)
}

// CreateToken stores a token and audits token_create:<name> in the same transaction. It returns ErrConflict when a token has
// the name or the digest already. The operation lock does not matter: a token can be made or revoked during a deploy.
func (s *Store) CreateToken(ctx context.Context, actor Actor, t Token) error {
	if actor.ID == "" {
		return errors.New("actor required")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var taken int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM tokens WHERE environment=? AND (name=? OR digest=?)`, s.env, t.Name, t.Digest).Scan(&taken); err != nil {
		return err
	}
	if taken != 0 {
		return ErrConflict
	}
	var expires any
	if t.Expires != nil {
		expires = t.Expires.UTC().UnixMilli()
	}
	agent := 0
	if t.Agent {
		agent = 1
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tokens(environment,`+tokenColumns+`) VALUES(?,?,?,?,?,?,?)`, s.env, t.Name, t.Digest, t.Scope, agent, t.Created.UTC().UnixMilli(), expires); err != nil {
		return err
	}
	if err = auditTx(ctx, tx, s.env, "", Admission{ActorID: actor.ID, ActorName: actor.Name, Remote: actor.Remote, Kind: "token_create:" + t.Name}, "succeeded"); err != nil {
		return err
	}
	return tx.Commit()
}

// RevokeToken removes the token called name and audits token_revoke:<name> in the same transaction. It returns sql.ErrNoRows
// when no token has the name. guard, when it is not nil, is called inside the transaction with the tokens that would remain;
// the revoke stops with the error it returns, so a rule about what must remain cannot be raced by another revoke.
func (s *Store) RevokeToken(ctx context.Context, actor Actor, name string, guard func(remaining []Token) error) error {
	if actor.ID == "" {
		return errors.New("actor required")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	all, err := s.tokens(ctx, tx)
	if err != nil {
		return err
	}
	var target *Token
	var remaining []Token
	for i := range all {
		if all[i].Name == name {
			target = &all[i]
		} else {
			remaining = append(remaining, all[i])
		}
	}
	if target == nil {
		return sql.ErrNoRows
	}
	if guard != nil {
		if err = guard(remaining); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM tokens WHERE environment=? AND name=?`, s.env, name); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM token_use WHERE environment=? AND digest=?`, s.env, target.Digest); err != nil {
		return err
	}
	if err = auditTx(ctx, tx, s.env, "", Admission{ActorID: actor.ID, ActorName: actor.Name, Remote: actor.Remote, Kind: "token_revoke:" + name}, "succeeded"); err != nil {
		return err
	}
	return tx.Commit()
}

// TouchTokens records when tokens were last used, by digest. A token of the agent's configuration file has a digest too, so
// it is recorded the same way. It keeps the later of the stored time and the given one.
func (s *Store) TouchTokens(ctx context.Context, used map[string]time.Time) error {
	if len(used) == 0 {
		return nil
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for digest, at := range used {
		if _, err = tx.ExecContext(ctx, `INSERT INTO token_use(environment,digest,last_used) VALUES(?,?,?) ON CONFLICT(environment,digest) DO UPDATE SET last_used=max(last_used,excluded.last_used)`, s.env, digest, at.UTC().UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// TokenUse returns when each token was last used, by digest. A token that was never used has no entry.
func (s *Store) TokenUse(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.query(ctx, `SELECT digest,last_used FROM token_use WHERE environment=?`, s.env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var digest string
		var ms int64
		if err = rows.Scan(&digest, &ms); err != nil {
			return nil, err
		}
		out[digest] = time.UnixMilli(ms).UTC()
	}
	return out, rows.Err()
}
