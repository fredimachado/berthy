package state

import (
	"database/sql"
	"errors"
	"fmt"
)

const bootstrapSchema = `
 CREATE TABLE IF NOT EXISTS claims (environment TEXT NOT NULL, kind TEXT NOT NULL, name TEXT NOT NULL, PRIMARY KEY(environment, kind, name));
 CREATE TABLE IF NOT EXISTS acquired_images (environment TEXT NOT NULL, ref TEXT NOT NULL, image_id TEXT NOT NULL, PRIMARY KEY(environment,ref));
 CREATE TABLE IF NOT EXISTS protection (environment TEXT PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS metadata (name TEXT PRIMARY KEY, value BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS operations (id TEXT PRIMARY KEY, environment TEXT NOT NULL, request_id TEXT NOT NULL, actor_id TEXT NOT NULL, actor_name TEXT NOT NULL, remote TEXT NOT NULL, kind TEXT NOT NULL, app TEXT NOT NULL, commit_sha TEXT NOT NULL, request_hash BLOB NOT NULL, public_request BLOB NOT NULL, state TEXT NOT NULL, started INTEGER NOT NULL, error TEXT NOT NULL, schema_before TEXT NOT NULL, schema_after TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '', modified INTEGER NOT NULL DEFAULT 0, no_return TEXT NOT NULL DEFAULT '', UNIQUE(environment,request_id));
 CREATE TABLE IF NOT EXISTS locks (environment TEXT PRIMARY KEY, operation_id TEXT REFERENCES operations(id), holder TEXT NOT NULL, reason TEXT NOT NULL, acquired INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS events (id INTEGER PRIMARY KEY AUTOINCREMENT, operation_id TEXT NOT NULL REFERENCES operations(id), kind TEXT NOT NULL, data BLOB NOT NULL, created INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS intents (operation_id TEXT NOT NULL REFERENCES operations(id), ordinal INTEGER NOT NULL, kind TEXT NOT NULL, role TEXT NOT NULL, name TEXT NOT NULL, service_id TEXT NOT NULL, generation TEXT NOT NULL, seen_version INTEGER NOT NULL, deadline INTEGER NOT NULL, before_sealed BLOB, desired_sealed BLOB NOT NULL, phase TEXT NOT NULL, receipt BLOB NOT NULL, PRIMARY KEY(operation_id,ordinal));
 CREATE TABLE IF NOT EXISTS releases (id INTEGER PRIMARY KEY AUTOINCREMENT, environment TEXT NOT NULL, app TEXT NOT NULL, digest TEXT NOT NULL, commit_sha TEXT NOT NULL, manifest BLOB NOT NULL, created INTEGER NOT NULL, modified INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS applied (environment TEXT NOT NULL, app TEXT NOT NULL, release_id INTEGER NOT NULL REFERENCES releases(id), PRIMARY KEY(environment,app));
 CREATE TABLE IF NOT EXISTS bindings (environment TEXT NOT NULL, app TEXT NOT NULL, role TEXT NOT NULL, name TEXT NOT NULL UNIQUE, service_id TEXT NOT NULL UNIQUE, PRIMARY KEY(environment,app,role));
 CREATE TABLE IF NOT EXISTS secrets (environment TEXT NOT NULL, name TEXT NOT NULL, registry TEXT NOT NULL, sealed BLOB NOT NULL, PRIMARY KEY(environment,name));
 CREATE TABLE IF NOT EXISTS audit (id INTEGER PRIMARY KEY AUTOINCREMENT, environment TEXT NOT NULL, operation_id TEXT NOT NULL, actor_id TEXT NOT NULL, actor_name TEXT NOT NULL, remote TEXT NOT NULL, action TEXT NOT NULL, outcome TEXT NOT NULL, created INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS ingress (environment TEXT PRIMARY KEY, account_id TEXT NOT NULL, api_token_secret TEXT NOT NULL, tunnel_id TEXT NOT NULL, tunnel_name TEXT NOT NULL, dns INTEGER NOT NULL, updated INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS ingress_routes (environment TEXT NOT NULL, hostname TEXT NOT NULL, app TEXT NOT NULL, role TEXT NOT NULL, service TEXT NOT NULL, zone_id TEXT NOT NULL, dns_record_id TEXT NOT NULL, route_created INTEGER NOT NULL DEFAULT 1, dns_created INTEGER NOT NULL DEFAULT 1, dns_prior_comment TEXT NOT NULL DEFAULT '', PRIMARY KEY(environment, hostname));
 CREATE TABLE IF NOT EXISTS checks (environment TEXT NOT NULL, name TEXT NOT NULL, state TEXT NOT NULL, detail TEXT NOT NULL, since INTEGER NOT NULL, notified_state TEXT NOT NULL, notified INTEGER NOT NULL, PRIMARY KEY(environment, name));
 CREATE TABLE IF NOT EXISTS job_runs (id INTEGER PRIMARY KEY AUTOINCREMENT, environment TEXT NOT NULL, job TEXT NOT NULL, app TEXT NOT NULL, trigger TEXT NOT NULL, scheduled INTEGER NOT NULL, started INTEGER NOT NULL, finished INTEGER NOT NULL, status TEXT NOT NULL, exit_code INTEGER, detail TEXT NOT NULL, container TEXT NOT NULL, output_sealed BLOB);
 CREATE UNIQUE INDEX IF NOT EXISTS job_runs_occurrence ON job_runs(environment, job, scheduled) WHERE scheduled <> 0;
 CREATE TABLE IF NOT EXISTS backups (id INTEGER PRIMARY KEY AUTOINCREMENT, environment TEXT NOT NULL, backup TEXT NOT NULL, app TEXT NOT NULL, volume TEXT NOT NULL, destination TEXT NOT NULL, object_key TEXT NOT NULL, trigger TEXT NOT NULL, started INTEGER NOT NULL, finished INTEGER NOT NULL, status TEXT NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL, detail TEXT NOT NULL, scheduled INTEGER NOT NULL DEFAULT 0);
 CREATE UNIQUE INDEX IF NOT EXISTS backups_object ON backups(environment, object_key);
 CREATE TABLE IF NOT EXISTS diagnostics (operation_id TEXT NOT NULL REFERENCES operations(id), seq INTEGER NOT NULL, sealed BLOB NOT NULL, created INTEGER NOT NULL, PRIMARY KEY(operation_id, seq));
 CREATE TABLE IF NOT EXISTS state_backups (id INTEGER PRIMARY KEY AUTOINCREMENT, environment TEXT NOT NULL, trigger TEXT NOT NULL, scheduled INTEGER NOT NULL DEFAULT 0, started INTEGER NOT NULL, finished INTEGER NOT NULL, status TEXT NOT NULL, object_key TEXT NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL, detail TEXT NOT NULL);
 CREATE UNIQUE INDEX IF NOT EXISTS state_backups_occurrence ON state_backups(environment, scheduled) WHERE scheduled <> 0;
 CREATE TABLE IF NOT EXISTS environment_settings (environment TEXT PRIMARY KEY, project TEXT NOT NULL, manifest BLOB NOT NULL, apps BLOB NOT NULL, commit_sha TEXT NOT NULL, modified INTEGER NOT NULL DEFAULT 0, operation_id TEXT NOT NULL DEFAULT '', updated INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS approvals (id TEXT PRIMARY KEY, environment TEXT NOT NULL, actor_id TEXT NOT NULL, actor_name TEXT NOT NULL, remote TEXT NOT NULL, method TEXT NOT NULL, path TEXT NOT NULL, request_sealed BLOB NOT NULL, summary TEXT NOT NULL, plan BLOB NOT NULL, created INTEGER NOT NULL, expires INTEGER NOT NULL, status TEXT NOT NULL, decided_by TEXT NOT NULL, decided_at INTEGER NOT NULL, result TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS approvals_environment ON approvals(environment, status, created);
 CREATE TABLE IF NOT EXISTS tokens (environment TEXT NOT NULL, name TEXT NOT NULL, digest TEXT NOT NULL UNIQUE, scope TEXT NOT NULL, agent INTEGER NOT NULL, created INTEGER NOT NULL, expires INTEGER, PRIMARY KEY(environment,name));
 CREATE TABLE IF NOT EXISTS token_use (environment TEXT NOT NULL, digest TEXT NOT NULL, last_used INTEGER NOT NULL, PRIMARY KEY(environment,digest));`

type schemaStep struct {
	apply        func(*sql.Tx) error
	existingOnly bool
}

var schemaSteps = [...]schemaStep{
	3:  {apply: schema3},
	8:  {apply: schema8, existingOnly: true},
	9:  {apply: schema9, existingOnly: true},
	10: {apply: schema10, existingOnly: true},
	13: {apply: schema13},
	14: {apply: schema14},
	17: {apply: schema17},
	18: {apply: schema18},
	19: {}, // Claims are created by the unconditional bootstrap DDL.
}

const schemaVersion = len(schemaSteps) - 1

func (s *Store) initializeSchema(keys keyFiles, fresh bool, version int) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(bootstrapSchema); err != nil {
		return false, err
	}
	// The key check names the key the state is sealed with, which settles which key file is current.
	var check []byte
	rotationCommitted := false
	err = tx.QueryRow(`SELECT value FROM metadata WHERE name='key_check'`).Scan(&check)
	if errors.Is(err, sql.ErrNoRows) && fresh {
		s.keys = keyring{current: keys.main}
		check, err = s.seal(keyCheckName, []byte(keyCheckPlain))
		if err == nil {
			_, err = tx.Exec(`INSERT INTO metadata VALUES('key_check',?)`, check)
		}
	} else if err == nil {
		s.keys, rotationCommitted, err = chooseKeys(keys, check)
	}
	if err != nil {
		return false, fmt.Errorf("state key: %w", err)
	}
	if err = applySchemaSteps(tx, version); err != nil {
		return false, err
	}
	// Each scheduled occurrence of a backup is recorded once.
	if _, err = tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS backups_occurrence ON backups(environment, backup, scheduled) WHERE scheduled <> 0`); err != nil {
		return false, err
	}
	if _, err = tx.Exec(fmt.Sprintf(`PRAGMA user_version=%d`, schemaVersion)); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return rotationCommitted, nil
}

func applySchemaSteps(tx *sql.Tx, version int) error {
	for target, step := range schemaSteps {
		if target <= version || step.apply == nil || (version == 0 && step.existingOnly) {
			continue
		}
		if err := step.apply(tx); err != nil {
			return err
		}
	}
	return nil
}

func schema3(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE releases ADD COLUMN operation_id TEXT NOT NULL DEFAULT ''; ALTER TABLE releases ADD COLUMN schema_before TEXT NOT NULL DEFAULT ''; ALTER TABLE releases ADD COLUMN schema_after TEXT NOT NULL DEFAULT ''`)
	return err
}

func schema8(tx *sql.Tx) error {
	// A database from before schema 8 lacks the reason column. The column check keeps a repeated upgrade harmless.
	var has int
	err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info('operations') WHERE name='reason'`).Scan(&has)
	if err == nil && has == 0 {
		_, err = tx.Exec(`ALTER TABLE operations ADD COLUMN reason TEXT NOT NULL DEFAULT ''`)
	}
	return err
}

func schema9(tx *sql.Tx) error {
	// A database from before schema 9 lacks the ownership columns. Every route it holds counts as created by Berthy,
	// which is how it was treated until now. The column check keeps a repeated upgrade harmless.
	var has int
	err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info('ingress_routes') WHERE name='route_created'`).Scan(&has)
	if err == nil && has == 0 {
		_, err = tx.Exec(`ALTER TABLE ingress_routes ADD COLUMN route_created INTEGER NOT NULL DEFAULT 1; ALTER TABLE ingress_routes ADD COLUMN dns_created INTEGER NOT NULL DEFAULT 1; ALTER TABLE ingress_routes ADD COLUMN dns_prior_comment TEXT NOT NULL DEFAULT ''`)
	}
	return err
}

func schema10(tx *sql.Tx) error {
	// A database from before schema 10 lacks the occurrence of a scheduled backup. The column check keeps a repeated upgrade
	// harmless.
	var has int
	err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info('backups') WHERE name='scheduled'`).Scan(&has)
	if err == nil && has == 0 {
		_, err = tx.Exec(`ALTER TABLE backups ADD COLUMN scheduled INTEGER NOT NULL DEFAULT 0`)
	}
	return err
}

func schema13(tx *sql.Tx) error {
	// A database from before schema 13 lacks the tag of a release. The column check keeps a repeated upgrade harmless.
	var has int
	err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info('releases') WHERE name='tag'`).Scan(&has)
	if err == nil && has == 0 {
		_, err = tx.Exec(`ALTER TABLE releases ADD COLUMN tag TEXT NOT NULL DEFAULT ''`)
	}
	return err
}

func schema14(tx *sql.Tx) error {
	// A database from before schema 14 lacks the local-changes flag of its releases and operations. The column checks keep a
	// repeated upgrade harmless.
	for _, table := range []string{"releases", "operations"} {
		var has int
		err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name='modified'`, table).Scan(&has)
		if err == nil && has == 0 {
			_, err = tx.Exec(`ALTER TABLE ` + table + ` ADD COLUMN modified INTEGER NOT NULL DEFAULT 0`)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func schema17(tx *sql.Tx) error {
	// A database from before schema 17 lacks the output a failed job run keeps. The column check keeps a repeated upgrade
	// harmless.
	var has int
	err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info('job_runs') WHERE name='output_sealed'`).Scan(&has)
	if err == nil && has == 0 {
		_, err = tx.Exec(`ALTER TABLE job_runs ADD COLUMN output_sealed BLOB`)
	}
	return err
}

func schema18(tx *sql.Tx) error {
	// A database from before schema 18 lacks the point of no return of an operation. The column check keeps a repeated upgrade
	// harmless, and every operation it holds counts as not crossed.
	var has int
	err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info('operations') WHERE name='no_return'`).Scan(&has)
	if err == nil && has == 0 {
		_, err = tx.Exec(`ALTER TABLE operations ADD COLUMN no_return TEXT NOT NULL DEFAULT ''`)
	}
	return err
}
