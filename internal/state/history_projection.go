package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"berthy/internal/manifest"
)

const (
	factSecret   = "secret"
	factVariable = "variable"
	factTemplate = "template"
)

// historyTableAvailable lets the readers run before the schema 20 step is registered.
func (s *Store) historyTableAvailable(ctx context.Context, table string) (bool, error) {
	if schemaVersion >= 20 {
		return true, nil
	}
	var exists bool
	err := s.queryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name=?)`, table).Scan(&exists)
	return exists, err
}

func (s *Store) rememberRelease(ctx context.Context, tx *sql.Tx, r Release) error {
	m, err := manifest.DecodeRelease(r.Manifest, r.App == manifest.ConnectorAppName)
	if err != nil {
		return err
	}
	app, ok := m.Apps[r.App]
	if !ok {
		return errors.New("released app is missing from its manifest")
	}
	insert := func(app, kind, value string) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO release_facts(environment,app,kind,value) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, s.env, app, kind, value)
		return err
	}
	for _, name := range app.Env.SecretNames() {
		if err := insert(r.App, factSecret, name); err != nil {
			return err
		}
	}
	for _, name := range app.Env.SensitiveVars() {
		if err := insert(r.App, factVariable, name); err != nil {
			return err
		}
	}
	for _, a := range m.Apps {
		for _, source := range a.Env.Template {
			if err := insert("", factTemplate, source); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) historicalFacts(ctx context.Context, app, kind string) ([]string, error) {
	if exists, err := s.historyTableAvailable(ctx, "release_facts"); err != nil || !exists {
		return nil, err
	}
	rows, err := s.query(ctx, `SELECT value FROM release_facts WHERE environment=? AND app=? AND kind=? ORDER BY value`, s.env, app, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var facts []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		facts = append(facts, value)
	}
	return facts, rows.Err()
}

func (s *Store) HistoricalSecretNames(ctx context.Context, app string) ([]string, error) {
	return s.historicalFacts(ctx, app, factSecret)
}

func (s *Store) HistoricalSensitiveVars(ctx context.Context, app string) ([]string, error) {
	return s.historicalFacts(ctx, app, factVariable)
}

func (s *Store) HistoricalTemplates(ctx context.Context) ([]string, error) {
	return s.historicalFacts(ctx, "", factTemplate)
}

// HistoricalSecretReader returns the first app in lexical order, or an empty string if none read name.
func (s *Store) HistoricalSecretReader(ctx context.Context, name string) (string, error) {
	if exists, err := s.historyTableAvailable(ctx, "release_facts"); err != nil || !exists {
		return "", err
	}
	var app string
	err := s.queryRow(ctx, `SELECT app FROM release_facts WHERE environment=? AND kind='secret' AND value=? ORDER BY app LIMIT 1`, s.env, name).Scan(&app)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return app, err
}

func (s *Store) HistoricalComposeTargets(ctx context.Context, project string) ([]Claim, error) {
	if exists, err := s.historyTableAvailable(ctx, "compose_down_targets"); err != nil || !exists {
		return nil, err
	}
	rows, err := s.query(ctx, `SELECT kind,name FROM compose_down_targets WHERE environment=? AND project=? ORDER BY kind,name`, s.env, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var claims []Claim
	for rows.Next() {
		var claim Claim
		if err := rows.Scan(&claim.Kind, &claim.Name); err != nil {
			return nil, err
		}
		claims = append(claims, claim)
	}
	return claims, rows.Err()
}

func (s *Store) rememberOperationReceipts(ctx context.Context, tx *sql.Tx, id string) error {
	rows, err := tx.QueryContext(ctx, completedOneOffsQuery+` AND o.id=? ORDER BY i.ordinal`, s.env, id)
	if err != nil {
		return err
	}
	hooks, err := s.readCompletedOneOffs(rows)
	if err != nil {
		return err
	}
	for _, hook := range hooks {
		if _, err := tx.ExecContext(ctx, `INSERT INTO prune_oneoffs(environment,operation_id,id,name,role) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, s.env, hook.OperationID, hook.ID, hook.Name, hook.Role); err != nil {
			return err
		}
	}
	targets, err := s.readComposeTargets(ctx, tx, id)
	if err != nil {
		return err
	}
	for project, claims := range targets {
		for _, claim := range claims {
			if _, err := tx.ExecContext(ctx, `INSERT INTO compose_down_targets(environment,project,kind,name) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, s.env, project, claim.Kind, claim.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) readComposeTargets(ctx context.Context, tx *sql.Tx, id string) (map[string][]Claim, error) {
	rows, err := tx.QueryContext(ctx, `SELECT ordinal,name,receipt FROM intents i JOIN operations o ON o.id=i.operation_id
		WHERE o.environment=? AND i.operation_id=? AND i.kind='compose_down'`, s.env, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byProject := map[string][]Claim{}
	for rows.Next() {
		var ordinal int
		var project string
		var sealed []byte
		if err := rows.Scan(&ordinal, &project, &sealed); err != nil {
			return nil, err
		}
		plain, err := s.openFor(s.env, intentRecord(id, ordinal, "receipt"), sealed)
		if err != nil {
			return nil, err
		}
		var receipt struct {
			Targets []Claim `json:"targets"`
		}
		if err := json.Unmarshal(plain, &receipt); err != nil {
			return nil, err
		}
		byProject[project] = append(byProject[project], receipt.Targets...)
	}
	return byProject, rows.Err()
}
