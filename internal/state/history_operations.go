package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Keep the selection stable while audit and child records are changed. The temp table lives on this transaction's connection.
func (s *Store) trimOperations(ctx context.Context, tx *sql.Tx, env, app string, keep int) error {
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE retiring_operations (
		id TEXT PRIMARY KEY, archived INTEGER NOT NULL, tag TEXT NOT NULL DEFAULT '',
		keep_compose INTEGER, keep_schema INTEGER, keep_diagnostics INTEGER, keep_approval INTEGER, keep_latest INTEGER,
		final_event INTEGER
	) WITHOUT ROWID`); err != nil {
		return err
	}
	// Previously trimmed headers may have lost their last pin, even in another app.
	result, err := tx.ExecContext(ctx, `WITH old AS (
		SELECT id FROM operations o WHERE environment=?1 AND app=?2 AND state IN `+terminalStates+`
		AND NOT EXISTS(SELECT 1 FROM locks WHERE operation_id=o.id)
		ORDER BY started DESC,rowid DESC LIMIT -1 OFFSET ?3
	)
	INSERT INTO retiring_operations(id,archived) SELECT id,archived FROM (
		SELECT o.id, EXISTS(SELECT 1 FROM audit WHERE environment=o.environment AND operation_id=o.id AND retired_request_id IS NOT NULL) AS archived
		FROM operations o WHERE environment=?1 AND state IN `+terminalStates+`
		AND NOT EXISTS(SELECT 1 FROM locks WHERE operation_id=o.id)
	) WHERE archived OR id IN (SELECT id FROM old)`, env, app, keep)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 0 {
		if err := s.retireOperations(ctx, tx, env); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `DROP TABLE temp.retiring_operations`)
	return err
}

func (s *Store) retireOperations(ctx context.Context, tx *sql.Tx, env string) error {
	publicTags, err := retiringTags(ctx, tx)
	if err != nil {
		return err
	}
	if len(publicTags) != 0 {
		if err := s.rememberOperationReceipts(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE retiring_operations AS r SET tag=tags.value
			FROM json_each(?) AS tags WHERE r.id=tags.key`, string(publicTags)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE audit AS a SET retired_tag=r.tag,
			retired_request_id=CASE WHEN a.outcome='accepted' THEN o.request_id ELSE a.retired_request_id END,
			retired_request_hash=CASE WHEN a.outcome='accepted' THEN o.request_hash ELSE a.retired_request_hash END,
			retired_app=CASE WHEN a.outcome='accepted' THEN o.app ELSE a.retired_app END
			FROM retiring_operations r JOIN operations o ON o.id=r.id
			WHERE NOT r.archived AND a.environment=o.environment AND a.operation_id=o.id`); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `WITH latest_migrations AS (
		SELECT id, ROW_NUMBER() OVER (PARTITION BY app ORDER BY started DESC,rowid DESC) AS rank
		FROM operations WHERE environment=?1 AND `+recordedMigration+`
	), latest_operations AS (
		SELECT id, ROW_NUMBER() OVER (PARTITION BY app,kind ORDER BY started DESC,rowid DESC) AS rank
		FROM operations WHERE environment=?1 AND state IN `+terminalStates+`
	)
	UPDATE retiring_operations AS r SET
		keep_compose=r.id IN (SELECT o.id FROM releases rel JOIN operations o
			ON o.id=rel.operation_id AND o.environment=rel.environment AND o.app=rel.app WHERE rel.environment=?1),
		keep_schema=r.id IN (SELECT id FROM latest_migrations WHERE rank=1),
		keep_diagnostics=EXISTS(SELECT 1 FROM diagnostics WHERE operation_id=r.id),
		keep_approval=r.id IN (SELECT substr(result,length('operation ')+1) FROM approvals
			WHERE environment=?1 AND status NOT IN ('pending','expired') AND result GLOB 'operation *'),
		keep_latest=r.id IN (SELECT id FROM latest_operations WHERE rank=1)`, env); err != nil {
		return err
	}
	for _, statement := range []string{
		`UPDATE retiring_operations AS r SET final_event=(SELECT MAX(id) FROM events WHERE operation_id=r.id AND kind IN ` + terminalStates + `) WHERE keep_latest`,
		`DELETE FROM intents AS i WHERE i.operation_id IN (SELECT id FROM retiring_operations) AND NOT EXISTS (
			SELECT 1 FROM retiring_operations r WHERE r.id=i.operation_id AND (
				(r.keep_compose AND i.kind='compose_source') OR (r.keep_schema AND i.kind='schema_source') OR
				(r.keep_approval AND i.kind='exec' AND (SELECT kind FROM operations WHERE id=r.id) IN ('exec','run'))))`,
		`DELETE FROM events AS e WHERE e.operation_id IN (SELECT id FROM retiring_operations)
			AND e.id IS NOT (SELECT final_event FROM retiring_operations WHERE id=e.operation_id)`,
		`DELETE FROM operations WHERE id IN (SELECT id FROM retiring_operations
			WHERE NOT (keep_compose OR keep_schema OR keep_diagnostics OR keep_approval OR keep_latest))`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func retiringTags(ctx context.Context, tx *sql.Tx) ([]byte, error) {
	rows, err := tx.QueryContext(ctx, `SELECT o.id,o.public_request,
		(SELECT count(*) FROM audit WHERE environment=o.environment AND operation_id=o.id AND outcome='accepted')
		FROM retiring_operations r JOIN operations o ON o.id=r.id WHERE NOT r.archived`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tags := map[string]string{}
	for rows.Next() {
		var id string
		var public []byte
		var acceptance int
		if err := rows.Scan(&id, &public, &acceptance); err != nil {
			return nil, err
		}
		if acceptance != 1 {
			return nil, errors.New("operation acceptance audit row unavailable")
		}
		tag, err := publicRequestTag(public)
		if err != nil {
			return nil, err
		}
		tags[id] = tag
	}
	if err := rows.Err(); err != nil || len(tags) == 0 {
		return nil, err
	}
	return json.Marshal(tags)
}
