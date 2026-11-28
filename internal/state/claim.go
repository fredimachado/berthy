package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// OwnerError is the refusal of a state that belongs to another project or environment than the one asked for.
type OwnerError struct{ Have, Want string }

func (e *OwnerError) Error() string {
	return fmt.Sprintf("state belongs to %s, not %s", e.Have, e.Want)
}

// ErrNoOwner means the state has no recorded project/environment owner metadata.
var ErrNoOwner = errors.New("state has no recorded owner")

// Claim records project and the store's environment as the owner of this state. The first call writes both; every later
// call checks both. The owner is the metadata rows project and environment only. A fresh state can hold check rows
// before any audit row exists, so the environment is never inferred from other tables.
func (s *Store) Claim(ctx context.Context, project string) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	haveProject, projectOK, err := metadataString(ctx, tx, "project")
	if err != nil {
		return err
	}
	haveEnv, envOK, err := metadataString(ctx, tx, "environment")
	if err != nil {
		return err
	}
	if !projectOK && !envOK {
		if _, err = tx.ExecContext(ctx, `INSERT INTO metadata VALUES(?,?)`, "project", []byte(project)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO metadata VALUES(?,?)`, "environment", []byte(s.env)); err != nil {
			return err
		}
		return tx.Commit()
	}
	if projectOK && envOK && haveProject == project && haveEnv == s.env {
		return nil
	}
	return &OwnerError{Have: haveProject + "/" + haveEnv, Want: project + "/" + s.env}
}

// CheckOwner reports whether the recorded owner is project and the store's environment. It never writes. A state with no
// owner, or with only one of the two metadata rows, is ErrNoOwner.
func (s *Store) CheckOwner(ctx context.Context, project string) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return checkOwner(ctx, tx, project, s.env)
}

func checkOwner(ctx context.Context, tx *sql.Tx, project, environment string) error {
	haveProject, projectOK, err := metadataString(ctx, tx, "project")
	if err != nil {
		return err
	}
	haveEnv, envOK, err := metadataString(ctx, tx, "environment")
	if err != nil {
		return err
	}
	if !projectOK || !envOK {
		return ErrNoOwner
	}
	if haveProject == project && haveEnv == environment {
		return nil
	}
	return &OwnerError{Have: haveProject + "/" + haveEnv, Want: project + "/" + environment}
}

func metadataString(ctx context.Context, tx *sql.Tx, name string) (string, bool, error) {
	var v []byte
	err := tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE name=?`, name).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(v), true, nil
}
