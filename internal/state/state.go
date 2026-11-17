package state

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrConflict = errors.New("state conflict")

type Store struct {
	db    *sql.DB
	key   cipher.AEAD
	env   string
	owner *os.File
}
type Admission struct {
	RequestID, ActorID, ActorName, Remote, Kind, App, Commit string
	// Modified marks a deploy from a checkout with local changes, so Commit does not hold what was deployed.
	Modified      bool
	RequestHash   [32]byte
	PublicRequest json.RawMessage
}
type Operation struct {
	ID string
	Admission
	State                          string
	Started                        time.Time
	Err, SchemaBefore, SchemaAfter string
	// Reason is the public one-line cause of a failed operation. Err stays the fixed category.
	Reason string
}
type Intent struct {
	Ordinal                                 int
	Kind, Role, Name, ServiceID, Generation string
	SeenVersion                             uint64
	Deadline                                time.Time
	Before, Desired                         json.RawMessage
	Phase                                   string
	Receipt                                 json.RawMessage
}
type Event struct {
	ID                uint64
	OperationID, Kind string
	Data              json.RawMessage
	Time              time.Time
}
type Binding struct{ App, Role, Name, ServiceID string }
type Release struct {
	ID                  int64
	App, Digest, Commit string
	// Modified is set when the release was deployed from a checkout with local changes: Commit does not hold what ran.
	Modified bool
	// Tag is the tag the deploy asked for (v1.0.2), empty for a digest deploy, a Compose app or an adoption. A label only:
	// the digest is what runs.
	Tag                                    string
	OperationID, SchemaBefore, SchemaAfter string
	Manifest                               json.RawMessage
	Created                                time.Time
	Bindings                               []Binding
}
type Actor struct{ ID, Name, Remote string }
type SecretInfo struct{ Name, Registry string }
type Outcome struct {
	Protected                      bool
	Err, SchemaBefore, SchemaAfter string
	// Reason is the public one-line cause of a failure. It joins the final event when the outcome sets no event.
	Reason  string
	State   string
	Event   json.RawMessage
	Release *Release
}
type Lock struct {
	OperationID, Holder, Reason string
	Acquired                    time.Time
}
type Audit struct {
	OperationID, ActorID, ActorName, Remote, Action, Outcome string
	Time                                                     time.Time
}

// IngressState is the managed ingress configuration of one environment.
type IngressState struct {
	AccountID, APITokenSecret, TunnelID, TunnelName string
	DNS                                             bool
	Updated                                         time.Time
}

// IngressRoute is one hostname that routes through the environment's tunnel.
type IngressRoute struct {
	Hostname, App, Role, Service, ZoneID, DNSRecordID string // DNSRecordID is empty when Berthy manages no DNS record for the hostname
	// RouteCreated and DNSCreated are false when Berthy adopted an existing rule or record instead of creating it. Removing
	// the app then releases it: an adopted rule stays in the tunnel, and an adopted record keeps living with its old comment.
	RouteCreated, DNSCreated bool
	// DNSPriorComment is the comment an adopted record had before Berthy marked it. Releasing the record puts it back.
	DNSPriorComment string
}

func Open(stateDir, keyFile, environment string) (*Store, error) {
	if environment == "" || stateDir == "" || keyFile == "" {
		return nil, errors.New("state directory, key file and environment required")
	}
	if strings.ContainsAny(stateDir, "?#") {
		return nil, errors.New("ambiguous state directory")
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, err
	}
	if err := checkDir(stateDir); err != nil {
		return nil, err
	}
	key, err := readKey(keyFile)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	owner, err := lockOwner(filepath.Join(stateDir, "owner.lock"))
	if err != nil {
		return nil, err
	}
	fresh, err := prepareDatabase(stateDir)
	if err != nil {
		owner.Close()
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "state.db"))
	if err != nil {
		owner.Close()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, key: aead, env: environment, owner: owner}
	fail := func(err error) (*Store, error) { s.Close(); return nil, err }
	if _, err = db.Exec(`PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;`); err != nil {
		return fail(err)
	}
	var version int
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fail(err)
	}
	if !fresh && (version < 1 || version > 14) {
		return fail(errors.New("unsupported or uninitialized state schema"))
	}
	tx, err := db.Begin()
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`
 CREATE TABLE IF NOT EXISTS acquired_images (environment TEXT NOT NULL, ref TEXT NOT NULL, image_id TEXT NOT NULL, PRIMARY KEY(environment,ref));
 CREATE TABLE IF NOT EXISTS protection (environment TEXT PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS metadata (name TEXT PRIMARY KEY, value BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS operations (id TEXT PRIMARY KEY, environment TEXT NOT NULL, request_id TEXT NOT NULL, actor_id TEXT NOT NULL, actor_name TEXT NOT NULL, remote TEXT NOT NULL, kind TEXT NOT NULL, app TEXT NOT NULL, commit_sha TEXT NOT NULL, request_hash BLOB NOT NULL, public_request BLOB NOT NULL, state TEXT NOT NULL, started INTEGER NOT NULL, error TEXT NOT NULL, schema_before TEXT NOT NULL, schema_after TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '', modified INTEGER NOT NULL DEFAULT 0, UNIQUE(environment,request_id));
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
 CREATE TABLE IF NOT EXISTS job_runs (id INTEGER PRIMARY KEY AUTOINCREMENT, environment TEXT NOT NULL, job TEXT NOT NULL, app TEXT NOT NULL, trigger TEXT NOT NULL, scheduled INTEGER NOT NULL, started INTEGER NOT NULL, finished INTEGER NOT NULL, status TEXT NOT NULL, exit_code INTEGER, detail TEXT NOT NULL, container TEXT NOT NULL);
 CREATE UNIQUE INDEX IF NOT EXISTS job_runs_occurrence ON job_runs(environment, job, scheduled) WHERE scheduled <> 0;
 CREATE TABLE IF NOT EXISTS backups (id INTEGER PRIMARY KEY AUTOINCREMENT, environment TEXT NOT NULL, backup TEXT NOT NULL, app TEXT NOT NULL, volume TEXT NOT NULL, destination TEXT NOT NULL, object_key TEXT NOT NULL, trigger TEXT NOT NULL, started INTEGER NOT NULL, finished INTEGER NOT NULL, status TEXT NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL, detail TEXT NOT NULL, scheduled INTEGER NOT NULL DEFAULT 0);
 CREATE UNIQUE INDEX IF NOT EXISTS backups_object ON backups(environment, object_key);
 CREATE TABLE IF NOT EXISTS diagnostics (operation_id TEXT NOT NULL REFERENCES operations(id), seq INTEGER NOT NULL, sealed BLOB NOT NULL, created INTEGER NOT NULL, PRIMARY KEY(operation_id, seq));
 CREATE TABLE IF NOT EXISTS state_backups (id INTEGER PRIMARY KEY AUTOINCREMENT, environment TEXT NOT NULL, trigger TEXT NOT NULL, scheduled INTEGER NOT NULL DEFAULT 0, started INTEGER NOT NULL, finished INTEGER NOT NULL, status TEXT NOT NULL, object_key TEXT NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL, detail TEXT NOT NULL);
 CREATE UNIQUE INDEX IF NOT EXISTS state_backups_occurrence ON state_backups(environment, scheduled) WHERE scheduled <> 0;
 CREATE TABLE IF NOT EXISTS approvals (id TEXT PRIMARY KEY, environment TEXT NOT NULL, actor_id TEXT NOT NULL, actor_name TEXT NOT NULL, remote TEXT NOT NULL, method TEXT NOT NULL, path TEXT NOT NULL, request_sealed BLOB NOT NULL, summary TEXT NOT NULL, plan BLOB NOT NULL, created INTEGER NOT NULL, expires INTEGER NOT NULL, status TEXT NOT NULL, decided_by TEXT NOT NULL, decided_at INTEGER NOT NULL, result TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS approvals_environment ON approvals(environment, status, created);`); err != nil {
		tx.Rollback()
		return fail(err)
	}
	var check []byte
	err = tx.QueryRow(`SELECT value FROM metadata WHERE name='key_check'`).Scan(&check)
	if errors.Is(err, sql.ErrNoRows) && fresh {
		check, err = s.seal("key_check", []byte("berthy-state"))
		if err == nil {
			_, err = tx.Exec(`INSERT INTO metadata VALUES('key_check',?)`, check)
		}
	} else if err == nil {
		var plain []byte
		plain, err = s.open("key_check", check)
		if err != nil || string(plain) != "berthy-state" {
			err = ErrWrongKey
		}
	}
	if err != nil {
		tx.Rollback()
		return fail(fmt.Errorf("state key: %w", err))
	}
	if version < 3 {
		if _, err = tx.Exec(`ALTER TABLE releases ADD COLUMN operation_id TEXT NOT NULL DEFAULT ''; ALTER TABLE releases ADD COLUMN schema_before TEXT NOT NULL DEFAULT ''; ALTER TABLE releases ADD COLUMN schema_after TEXT NOT NULL DEFAULT ''`); err != nil {
			tx.Rollback()
			return fail(err)
		}
	}
	if version != 0 && version < 8 {
		// A database from before schema 8 lacks the reason column. The column check keeps a repeated upgrade harmless.
		var has int
		if err = tx.QueryRow(`SELECT count(*) FROM pragma_table_info('operations') WHERE name='reason'`).Scan(&has); err == nil && has == 0 {
			_, err = tx.Exec(`ALTER TABLE operations ADD COLUMN reason TEXT NOT NULL DEFAULT ''`)
		}
		if err != nil {
			tx.Rollback()
			return fail(err)
		}
	}
	if version != 0 && version < 9 {
		// A database from before schema 9 lacks the ownership columns. Every route it holds counts as created by Berthy,
		// which is how it was treated until now. The column check keeps a repeated upgrade harmless.
		var has int
		if err = tx.QueryRow(`SELECT count(*) FROM pragma_table_info('ingress_routes') WHERE name='route_created'`).Scan(&has); err == nil && has == 0 {
			_, err = tx.Exec(`ALTER TABLE ingress_routes ADD COLUMN route_created INTEGER NOT NULL DEFAULT 1; ALTER TABLE ingress_routes ADD COLUMN dns_created INTEGER NOT NULL DEFAULT 1; ALTER TABLE ingress_routes ADD COLUMN dns_prior_comment TEXT NOT NULL DEFAULT ''`)
		}
		if err != nil {
			tx.Rollback()
			return fail(err)
		}
	}
	if version != 0 && version < 10 {
		// A database from before schema 10 lacks the occurrence of a scheduled backup. The column check keeps a repeated upgrade
		// harmless.
		var has int
		if err = tx.QueryRow(`SELECT count(*) FROM pragma_table_info('backups') WHERE name='scheduled'`).Scan(&has); err == nil && has == 0 {
			_, err = tx.Exec(`ALTER TABLE backups ADD COLUMN scheduled INTEGER NOT NULL DEFAULT 0`)
		}
		if err != nil {
			tx.Rollback()
			return fail(err)
		}
	}
	if version < 13 {
		// A database from before schema 13 lacks the tag of a release. The column check keeps a repeated upgrade harmless.
		var has int
		if err = tx.QueryRow(`SELECT count(*) FROM pragma_table_info('releases') WHERE name='tag'`).Scan(&has); err == nil && has == 0 {
			_, err = tx.Exec(`ALTER TABLE releases ADD COLUMN tag TEXT NOT NULL DEFAULT ''`)
		}
		if err != nil {
			tx.Rollback()
			return fail(err)
		}
	}
	if version < 14 {
		// A database from before schema 14 lacks the local-changes flag of its releases and operations. The column checks keep a
		// repeated upgrade harmless.
		for _, table := range []string{"releases", "operations"} {
			var has int
			if err = tx.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name='modified'`, table).Scan(&has); err == nil && has == 0 {
				_, err = tx.Exec(`ALTER TABLE ` + table + ` ADD COLUMN modified INTEGER NOT NULL DEFAULT 0`)
			}
			if err != nil {
				tx.Rollback()
				return fail(err)
			}
		}
	}
	// Each scheduled occurrence of a backup is recorded once.
	if _, err = tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS backups_occurrence ON backups(environment, backup, scheduled) WHERE scheduled <> 0`); err != nil {
		tx.Rollback()
		return fail(err)
	}
	if _, err = tx.Exec(`PRAGMA user_version=14`); err != nil {
		tx.Rollback()
		return fail(err)
	}
	if err = tx.Commit(); err != nil {
		return fail(err)
	}
	return s, nil
}
func (s *Store) Close() error {
	var err error
	if s.db != nil {
		err = s.db.Close()
		s.db = nil
	}
	if s.owner != nil {
		e := s.owner.Close()
		s.owner = nil
		if err == nil {
			err = e
		}
	}
	return err
}
func (s *Store) associated(record string) string {
	if record == "key_check" {
		return "berthy-state-key-check"
	}
	return s.env + "\x00" + record
}
func (s *Store) seal(record string, p []byte) ([]byte, error) {
	nonce := make([]byte, s.key.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return s.key.Seal(nonce, nonce, p, []byte(s.associated(record))), nil
}
func (s *Store) open(record string, b []byte) ([]byte, error) {
	n := s.key.NonceSize()
	if len(b) < n {
		return nil, errors.New("invalid ciphertext")
	}
	return s.key.Open(nil, b[:n], b[n:], []byte(s.associated(record)))
}
func randomID() (string, error) {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	return hex.EncodeToString(b), err
}
func now() int64              { return time.Now().UTC().UnixMilli() }
func validJSON(b []byte) bool { return len(b) > 0 && json.Valid(b) }
func publicJSON(b []byte) bool {
	if len(b) == 0 || len(b) > 16384 || !json.Valid(b) {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil || fields == nil {
		return false
	}
	for k, v := range fields {
		switch k {
		case "app", "version", "commit", "state", "role", "step", "status", "digest", "code", "reason", "restore_key", "restore_into", "keep_digest", "App", "Version", "Commit":
			var s string
			if json.Unmarshal(v, &s) != nil || len(s) > 1024 {
				return false
			}
		case "adopt", "Adopt", "revert_schema", "replace_existing", "allow_recreate", "volumes", "tunnel", "keep_version", "modified":
			var x bool
			if json.Unmarshal(v, &x) != nil {
				return false
			}
		case "exit_code", "exit_status", "target_release":
			var x int
			if json.Unmarshal(v, &x) != nil {
				return false
			}
		default:
			return false
		}
	}
	return true
}
func (s *Store) Admit(ctx context.Context, a Admission) (Operation, bool, error) {
	if a.RequestID == "" || a.ActorID == "" || a.Kind == "" || !publicJSON(a.PublicRequest) {
		return Operation{}, false, errors.New("invalid admission")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Operation{}, false, err
	}
	defer tx.Rollback()
	op, err := operationQuery(tx.QueryRowContext(ctx, `SELECT id,request_id,actor_id,actor_name,remote,kind,app,commit_sha,request_hash,public_request,state,started,error,schema_before,schema_after,reason,modified FROM operations WHERE environment=? AND request_id=?`, s.env, a.RequestID))
	if err == nil {
		if op.ActorID != a.ActorID || op.RequestHash != a.RequestHash {
			return Operation{}, false, ErrConflict
		}
		return op, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Operation{}, false, err
	}
	id, err := randomID()
	if err != nil {
		return Operation{}, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO operations(id,environment,request_id,actor_id,actor_name,remote,kind,app,commit_sha,request_hash,public_request,state,started,error,schema_before,schema_after,reason,modified) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, s.env, a.RequestID, a.ActorID, a.ActorName, a.Remote, a.Kind, a.App, a.Commit, a.RequestHash[:], []byte(a.PublicRequest), "running", now(), "", "", "", "", boolInt(a.Modified))
	if err != nil {
		return Operation{}, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO locks VALUES(?,?,?,?,?)`, s.env, id, a.ActorName, a.Kind, now())
	if err != nil {
		return Operation{}, false, ErrConflict
	}
	if err = eventTx(ctx, tx, id, "accepted", json.RawMessage(`{}`)); err != nil {
		return Operation{}, false, err
	}
	if err = auditTx(ctx, tx, s.env, id, a, "accepted"); err != nil {
		return Operation{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return Operation{}, false, err
	}
	return Operation{ID: id, Admission: a, State: "running"}, true, nil
}
func operationQuery(row *sql.Row) (Operation, error) {
	var o Operation
	var hash []byte
	var started int64
	err := row.Scan(&o.ID, &o.RequestID, &o.ActorID, &o.ActorName, &o.Remote, &o.Kind, &o.App, &o.Commit, &hash, &o.PublicRequest, &o.State, &started, &o.Err, &o.SchemaBefore, &o.SchemaAfter, &o.Reason, &o.Modified)
	o.Started = time.UnixMilli(started).UTC()
	copy(o.RequestHash[:], hash)
	return o, err
}
func (s *Store) Operation(ctx context.Context, id string) (Operation, error) {
	return operationQuery(s.db.QueryRowContext(ctx, `SELECT id,request_id,actor_id,actor_name,remote,kind,app,commit_sha,request_hash,public_request,state,started,error,schema_before,schema_after,reason,modified FROM operations WHERE environment=? AND id=?`, s.env, id))
}
func (s *Store) Active(ctx context.Context) (Operation, error) {
	return operationQuery(s.db.QueryRowContext(ctx, `SELECT o.id,o.request_id,o.actor_id,o.actor_name,o.remote,o.kind,o.app,o.commit_sha,o.request_hash,o.public_request,o.state,o.started,o.error,o.schema_before,o.schema_after,o.reason,o.modified FROM operations o JOIN locks l ON l.operation_id=o.id WHERE l.environment=?`, s.env))
}
func eventTx(ctx context.Context, tx *sql.Tx, id, kind string, data json.RawMessage) error {
	if !publicJSON(data) {
		return errors.New("invalid event")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO events(operation_id,kind,data,created) VALUES(?,?,?,?)`, id, kind, []byte(data), now())
	return err
}
func auditTx(ctx context.Context, tx *sql.Tx, env, id string, a Admission, outcome string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO audit(environment,operation_id,actor_id,actor_name,remote,action,outcome,created) VALUES(?,?,?,?,?,?,?,?)`, env, id, a.ActorID, a.ActorName, a.Remote, a.Kind, outcome, now())
	return err
}
func owned(ctx context.Context, tx *sql.Tx, env, id string) error {
	var v string
	err := tx.QueryRowContext(ctx, `SELECT operation_id FROM locks WHERE environment=? AND operation_id=?`, env, id).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
	}
	return err
}
func (s *Store) RecordIntent(ctx context.Context, id string, i Intent) error {
	if i.Ordinal < 0 || !validJSON(i.Desired) || (len(i.Before) > 0 && !validJSON(i.Before)) {
		return errors.New("invalid intent")
	}
	record := fmt.Sprintf("intent:%s:%d:", id, i.Ordinal)
	before, err := s.seal(record+"before", i.Before)
	if err != nil {
		return err
	}
	desired, err := s.seal(record+"desired", i.Desired)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = owned(ctx, tx, s.env, id); err != nil {
		return err
	}
	receipt, err := s.seal(record+"receipt", []byte(`{}`))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO intents VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, i.Ordinal, i.Kind, i.Role, i.Name, i.ServiceID, i.Generation, i.SeenVersion, i.Deadline.UnixMilli(), before, desired, "intent", receipt)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) MarkStep(ctx context.Context, id string, ordinal int, phase string, receipt json.RawMessage) error {
	if phase == "" || !validJSON(receipt) {
		return errors.New("invalid step")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = owned(ctx, tx, s.env, id); err != nil {
		return err
	}
	sealed, err := s.seal(fmt.Sprintf("intent:%s:%d:receipt", id, ordinal), receipt)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE intents SET phase=?,receipt=? WHERE operation_id=? AND ordinal=?`, phase, sealed, id, ordinal)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}
func (s *Store) Intents(ctx context.Context, id string) ([]Intent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.ordinal,i.kind,i.role,i.name,i.service_id,i.generation,i.seen_version,i.deadline,i.before_sealed,i.desired_sealed,i.phase,i.receipt FROM intents i JOIN operations o ON o.id=i.operation_id WHERE o.environment=? AND o.id=? ORDER BY i.ordinal`, s.env, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Intent
	for rows.Next() {
		var i Intent
		var before, desired []byte
		var deadline int64
		if err = rows.Scan(&i.Ordinal, &i.Kind, &i.Role, &i.Name, &i.ServiceID, &i.Generation, &i.SeenVersion, &deadline, &before, &desired, &i.Phase, &i.Receipt); err != nil {
			return nil, err
		}
		i.Deadline = time.UnixMilli(deadline).UTC()
		record := fmt.Sprintf("intent:%s:%d:", id, i.Ordinal)
		i.Before, err = s.open(record+"before", before)
		if err != nil {
			return nil, err
		}
		i.Desired, err = s.open(record+"desired", desired)
		if err != nil {
			return nil, err
		}
		i.Receipt, err = s.open(record+"receipt", i.Receipt)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
func (s *Store) AppendEvent(ctx context.Context, id string, e Event) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = owned(ctx, tx, s.env, id); err != nil {
		return err
	}
	if err = eventTx(ctx, tx, id, e.Kind, e.Data); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) EventsAfter(ctx context.Context, id string, after uint64, limit int) ([]Event, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("invalid limit")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.id,e.kind,e.data,e.created FROM events e JOIN operations o ON o.id=e.operation_id WHERE o.environment=? AND o.id=? AND e.id>? ORDER BY e.id LIMIT ?`, s.env, id, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var ms int64
		e.OperationID = id
		if err = rows.Scan(&e.ID, &e.Kind, &e.Data, &ms); err != nil {
			return nil, err
		}
		e.Time = time.UnixMilli(ms).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *Store) Finish(ctx context.Context, id string, o Outcome) error {
	if o.State != "succeeded" && o.State != "failed" && o.State != "rolled_back" && o.State != "interrupted" && o.State != "recovery_required" {
		return errors.New("invalid outcome")
	}
	if len(o.Event) == 0 {
		o.Event = json.RawMessage(`{}`)
		if o.Reason != "" {
			event, e := json.Marshal(map[string]string{"state": o.State, "reason": o.Reason})
			if e != nil {
				return e
			}
			o.Event = event
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = owned(ctx, tx, s.env, id); err != nil {
		return err
	}
	op, err := operationQuery(tx.QueryRowContext(ctx, `SELECT id,request_id,actor_id,actor_name,remote,kind,app,commit_sha,request_hash,public_request,state,started,error,schema_before,schema_after,reason,modified FROM operations WHERE environment=? AND id=?`, s.env, id))
	if err != nil {
		return err
	}
	if o.Protected && o.Release == nil {
		return errors.New("protection requires a successful release")
	}
	if o.Protected {
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO protection VALUES(?)`, s.env); err != nil {
			return err
		}
	}
	if o.Release != nil {
		if (o.State != "succeeded" && o.State != "failed") || o.Release.App == "" || o.Release.App != op.App || !validJSON(o.Release.Manifest) {
			return errors.New("invalid release")
		}
		r := o.Release
		res, e := tx.ExecContext(ctx, `INSERT INTO releases(environment,app,digest,commit_sha,manifest,created,operation_id,schema_before,schema_after,tag,modified) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, s.env, r.App, r.Digest, r.Commit, []byte(r.Manifest), now(), id, o.SchemaBefore, o.SchemaAfter, r.Tag, boolInt(r.Modified))
		if e != nil {
			return e
		}
		rid, e := res.LastInsertId()
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO applied VALUES(?,?,?) ON CONFLICT(environment,app) DO UPDATE SET release_id=excluded.release_id`, s.env, r.App, rid); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `DELETE FROM bindings WHERE environment=? AND app=?`, s.env, r.App); e != nil {
			return e
		}
		for _, b := range r.Bindings {
			if b.App != r.App || b.Role == "" || b.Name == "" || b.ServiceID == "" {
				return errors.New("invalid binding")
			}
			if _, e = tx.ExecContext(ctx, `INSERT INTO bindings VALUES(?,?,?,?,?)`, s.env, b.App, b.Role, b.Name, b.ServiceID); e != nil {
				return e
			}
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE operations SET state=?,error=?,schema_before=?,schema_after=?,reason=COALESCE(NULLIF(?,''),reason) WHERE id=?`, o.State, o.Err, o.SchemaBefore, o.SchemaAfter, o.Reason, id); err != nil {
		return err
	}
	if err = eventTx(ctx, tx, id, o.State, o.Event); err != nil {
		return err
	}
	if err = auditTx(ctx, tx, s.env, id, op.Admission, o.State); err != nil {
		return err
	}
	if o.State != "recovery_required" {
		if _, err = tx.ExecContext(ctx, `DELETE FROM locks WHERE environment=? AND operation_id=?`, s.env, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ResolveRecovery finishes an operation stuck in recovery_required as failed and releases its lock.
func (s *Store) ResolveRecovery(ctx context.Context, actor Actor, id, reason string) error {
	if actor.ID == "" {
		return errors.New("actor required")
	}
	if reason == "" {
		return errors.New("reason required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = owned(ctx, tx, s.env, id); err != nil {
		return err
	}
	var st string
	if err = tx.QueryRowContext(ctx, `SELECT state FROM operations WHERE environment=? AND id=?`, s.env, id).Scan(&st); err != nil {
		return err
	}
	if st != "recovery_required" {
		return ErrConflict
	}
	// schema_before and schema_after keep the states the operation recorded.
	if _, err = tx.ExecContext(ctx, `UPDATE operations SET state=?,error=? WHERE environment=? AND id=?`, "failed", "recovery resolved by admin", s.env, id); err != nil {
		return err
	}
	data, err := json.Marshal(map[string]string{"state": "failed", "reason": reason})
	if err != nil {
		return err
	}
	if err = eventTx(ctx, tx, id, "failed", data); err != nil {
		return err
	}
	if err = auditTx(ctx, tx, s.env, id, Admission{ActorID: actor.ID, ActorName: actor.Name, Remote: actor.Remote, Kind: "recovery_resolve"}, "succeeded"); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM locks WHERE environment=? AND operation_id=?`, s.env, id); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) History(ctx context.Context, app string) ([]Release, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,app,digest,commit_sha,manifest,created,operation_id,schema_before,schema_after,tag,modified FROM releases WHERE environment=? AND (?='' OR app=?) ORDER BY id DESC`, s.env, app, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		var r Release
		var ms int64
		if err = rows.Scan(&r.ID, &r.App, &r.Digest, &r.Commit, &r.Manifest, &ms, &r.OperationID, &r.SchemaBefore, &r.SchemaAfter, &r.Tag, &r.Modified); err != nil {
			return nil, err
		}
		r.Created = time.UnixMilli(ms).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestMigration returns the newest finished deploy of app that recorded a schema state.
func (s *Store) LatestMigration(ctx context.Context, app string) (Operation, error) {
	return operationQuery(s.db.QueryRowContext(ctx, `SELECT o.id,o.request_id,o.actor_id,o.actor_name,o.remote,o.kind,o.app,o.commit_sha,o.request_hash,o.public_request,o.state,o.started,o.error,o.schema_before,o.schema_after,o.reason,o.modified FROM operations o WHERE o.environment=? AND o.app=? AND o.kind='deploy' AND o.schema_before<>'' AND o.state IN ('succeeded','failed','rolled_back','interrupted') ORDER BY o.started DESC, o.rowid DESC LIMIT 1`, s.env, app))
}
func (s *Store) PutSecret(ctx context.Context, actor Actor, info SecretInfo, value []byte) error {
	if actor.ID == "" {
		return errors.New("actor required")
	}
	if info.Name == "" {
		return errors.New("empty secret name")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.checkNoLock(ctx, tx); err != nil {
		return err
	}
	if err = s.putSecretTx(ctx, tx, actor, info, value); err != nil {
		return err
	}
	return tx.Commit()
}

// SecretWrite is one secret of a batch write: the name and the value it takes. A batch write stores plain secrets.
type SecretWrite struct {
	Name  string
	Value []byte
}

// RegistrySecretError says that a name is stored as a registry password. Only a write that names the registry changes one, so
// a plain write of that name is refused.
type RegistrySecretError struct{ Name string }

func (e RegistrySecretError) Error() string { return e.Name + " is a registry password" }

// rowQuerier reads one row, inside a transaction or outside one.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// checkNoLock refuses with ErrConflict while an operation holds the environment's lock.
func (s *Store) checkNoLock(ctx context.Context, tx *sql.Tx) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM locks WHERE environment=?`, s.env).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return ErrConflict
	}
	return nil
}

// putSecretTx stores one secret inside tx and audits it. The caller holds the transaction and has checked the lock.
func (s *Store) putSecretTx(ctx context.Context, tx *sql.Tx, actor Actor, info SecretInfo, value []byte) error {
	sealed, err := s.seal("secret:"+info.Name+"\x00"+info.Registry, value)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO secrets VALUES(?,?,?,?) ON CONFLICT(environment,name) DO UPDATE SET registry=excluded.registry,sealed=excluded.sealed`, s.env, info.Name, info.Registry, sealed); err != nil {
		return err
	}
	return auditTx(ctx, tx, s.env, "", Admission{ActorID: actor.ID, ActorName: actor.Name, Remote: actor.Remote, Kind: "secret_set:" + info.Name}, "succeeded")
}

// classifySecrets sorts names into those that are new and those that replace a stored plain secret. A name stored as a
// registry password is refused with a RegistrySecretError, since a plain write would make it a plain secret.
func (s *Store) classifySecrets(ctx context.Context, q rowQuerier, names []string) (create, replace []string, err error) {
	create, replace = []string{}, []string{}
	for _, name := range names {
		var registry string
		err := q.QueryRowContext(ctx, `SELECT registry FROM secrets WHERE environment=? AND name=?`, s.env, name).Scan(&registry)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			create = append(create, name)
		case err != nil:
			return nil, nil, err
		case registry != "":
			return nil, nil, RegistrySecretError{Name: name}
		default:
			replace = append(replace, name)
		}
	}
	return create, replace, nil
}

// ClassifySecrets reports which of names are new and which replace a stored plain secret, as PutSecrets would find them. It
// writes nothing.
func (s *Store) ClassifySecrets(ctx context.Context, names []string) (create, replace []string, err error) {
	return s.classifySecrets(ctx, s.db, names)
}

// PutSecrets stores every write in one transaction: all of them, or none when any one fails. It refuses with ErrConflict while
// an operation holds the lock, and with a RegistrySecretError when a name is stored as a registry password. It returns the
// names it created and the names it replaced, in the order of writes, and audits each secret as PutSecret does.
func (s *Store) PutSecrets(ctx context.Context, actor Actor, writes []SecretWrite) (created, replaced []string, err error) {
	if actor.ID == "" {
		return nil, nil, errors.New("actor required")
	}
	names := make([]string, 0, len(writes))
	seen := make(map[string]bool, len(writes))
	for _, w := range writes {
		if w.Name == "" {
			return nil, nil, errors.New("empty secret name")
		}
		if seen[w.Name] {
			return nil, nil, fmt.Errorf("secret %s is written twice", w.Name)
		}
		seen[w.Name] = true
		names = append(names, w.Name)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	if err = s.checkNoLock(ctx, tx); err != nil {
		return nil, nil, err
	}
	created, replaced, err = s.classifySecrets(ctx, tx, names)
	if err != nil {
		return nil, nil, err
	}
	for _, w := range writes {
		if err = s.putSecretTx(ctx, tx, actor, SecretInfo{Name: w.Name}, w.Value); err != nil {
			return nil, nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, nil, err
	}
	return created, replaced, nil
}

// PutOperationSecret stores a secret for the operation that holds the environment lock, such as the tunnel
// token Berthy fetches during an ingress apply. PutSecret refuses while any lock is held; this does not.
func (s *Store) PutOperationSecret(ctx context.Context, opID, name string, value []byte) error {
	if name == "" {
		return errors.New("empty secret name")
	}
	sealed, err := s.seal("secret:"+name+"\x00", value)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = owned(ctx, tx, s.env, opID); err != nil {
		return err
	}
	var a Admission
	if err = tx.QueryRowContext(ctx, `SELECT actor_id,actor_name,remote FROM operations WHERE environment=? AND id=?`, s.env, opID).Scan(&a.ActorID, &a.ActorName, &a.Remote); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO secrets VALUES(?,?,?,?) ON CONFLICT(environment,name) DO UPDATE SET registry=excluded.registry,sealed=excluded.sealed`, s.env, name, "", sealed); err != nil {
		return err
	}
	a.Kind = "secret_set:" + name
	if err = auditTx(ctx, tx, s.env, opID, a, "succeeded"); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteOperationSecret removes a secret for the operation that holds the environment lock, such as the tunnel token
// once the connector is gone. It returns sql.ErrNoRows when there is no such secret. DeleteSecret refuses while any lock
// is held; this does not.
func (s *Store) DeleteOperationSecret(ctx context.Context, opID, name string) error {
	if name == "" {
		return errors.New("empty secret name")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = owned(ctx, tx, s.env, opID); err != nil {
		return err
	}
	var a Admission
	if err = tx.QueryRowContext(ctx, `SELECT actor_id,actor_name,remote FROM operations WHERE environment=? AND id=?`, s.env, opID).Scan(&a.ActorID, &a.ActorName, &a.Remote); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM secrets WHERE environment=? AND name=?`, s.env, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	a.Kind = "secret_delete:" + name
	if err = auditTx(ctx, tx, s.env, opID, a, "succeeded"); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Secret(ctx context.Context, name string) (SecretInfo, []byte, error) {
	var b []byte
	info := SecretInfo{Name: name}
	if err := s.db.QueryRowContext(ctx, `SELECT registry,sealed FROM secrets WHERE environment=? AND name=?`, s.env, name).Scan(&info.Registry, &b); err != nil {
		return info, nil, err
	}
	v, err := s.open("secret:"+name+"\x00"+info.Registry, b)
	return info, v, err
}
func (s *Store) Secrets(ctx context.Context) ([]SecretInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name,registry FROM secrets WHERE environment=? ORDER BY name`, s.env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SecretInfo
	for rows.Next() {
		var i SecretInfo
		if err = rows.Scan(&i.Name, &i.Registry); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
func (s *Store) DeleteSecret(ctx context.Context, actor Actor, name string) error {
	if actor.ID == "" {
		return errors.New("actor required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM locks WHERE environment=?`, s.env).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return ErrConflict
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM secrets WHERE environment=? AND name=?`, s.env, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	if actor.ID != "" {
		if err = auditTx(ctx, tx, s.env, "", Admission{ActorID: actor.ID, ActorName: actor.Name, Remote: actor.Remote, Kind: "secret_delete:" + name}, "succeeded"); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) AcquireManual(ctx context.Context, actor Actor, holder, reason string) error {
	if actor.ID == "" {
		return errors.New("actor required")
	}
	if holder == "" || reason == "" {
		return errors.New("holder and reason required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO locks VALUES(?,NULL,?,?,?)`, s.env, holder, reason, now())
	if err != nil {
		return ErrConflict
	}
	if actor.ID != "" {
		if err = auditTx(ctx, tx, s.env, "", Admission{ActorID: actor.ID, ActorName: actor.Name, Remote: actor.Remote, Kind: "lock_acquire"}, "succeeded"); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) ReleaseManual(ctx context.Context, actor Actor) error {
	if actor.ID == "" {
		return errors.New("actor required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM locks WHERE environment=? AND operation_id IS NULL`, s.env)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	if actor.ID != "" {
		if err = auditTx(ctx, tx, s.env, "", Admission{ActorID: actor.ID, ActorName: actor.Name, Remote: actor.Remote, Kind: "lock_release"}, "succeeded"); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) Lock(ctx context.Context) (Lock, error) {
	var l Lock
	var ms int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(operation_id,''),holder,reason,acquired FROM locks WHERE environment=?`, s.env).Scan(&l.OperationID, &l.Holder, &l.Reason, &ms)
	l.Acquired = time.UnixMilli(ms).UTC()
	return l, err
}

func (s *Store) Applied(ctx context.Context, app string) (Release, error) {
	var r Release
	var ms int64
	err := s.db.QueryRowContext(ctx, `SELECT r.id,r.app,r.digest,r.commit_sha,r.manifest,r.created,r.operation_id,r.schema_before,r.schema_after,r.tag,r.modified FROM releases r JOIN applied a ON a.release_id=r.id WHERE a.environment=? AND a.app=?`, s.env, app).Scan(&r.ID, &r.App, &r.Digest, &r.Commit, &r.Manifest, &ms, &r.OperationID, &r.SchemaBefore, &r.SchemaAfter, &r.Tag, &r.Modified)
	r.Created = time.UnixMilli(ms).UTC()
	if err != nil {
		return r, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT app,role,name,service_id FROM bindings WHERE environment=? AND app=? ORDER BY role`, s.env, app)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	for rows.Next() {
		var b Binding
		if err = rows.Scan(&b.App, &b.Role, &b.Name, &b.ServiceID); err != nil {
			return r, err
		}
		r.Bindings = append(r.Bindings, b)
	}
	return r, rows.Err()
}
func (s *Store) AuditLog(ctx context.Context, limit int) ([]Audit, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("invalid limit")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.operation_id,a.actor_id,a.actor_name,a.remote,a.action,a.outcome,a.created FROM audit a WHERE a.environment=? ORDER BY a.id DESC LIMIT ?`, s.env, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Audit
	for rows.Next() {
		var a Audit
		var ms int64
		if err = rows.Scan(&a.OperationID, &a.ActorID, &a.ActorName, &a.Remote, &a.Action, &a.Outcome, &ms); err != nil {
			return nil, err
		}
		a.Time = time.UnixMilli(ms).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

// Ingress returns the environment's managed ingress configuration. It returns sql.ErrNoRows when there is none.
func (s *Store) Ingress(ctx context.Context) (IngressState, error) {
	var st IngressState
	var dns int
	var ms int64
	err := s.db.QueryRowContext(ctx, `SELECT account_id,api_token_secret,tunnel_id,tunnel_name,dns,updated FROM ingress WHERE environment=?`, s.env).Scan(&st.AccountID, &st.APITokenSecret, &st.TunnelID, &st.TunnelName, &dns, &ms)
	if err != nil {
		return IngressState{}, err
	}
	st.DNS = dns != 0
	st.Updated = time.UnixMilli(ms).UTC()
	return st, nil
}

// SetIngress records the environment's managed ingress configuration. A zero Updated is stamped now.
// It requires the environment lock to be held by opID.
func (s *Store) SetIngress(ctx context.Context, opID string, st IngressState) error {
	if st.AccountID == "" || st.APITokenSecret == "" || st.TunnelID == "" || st.TunnelName == "" {
		return errors.New("invalid ingress state")
	}
	updated := now()
	if !st.Updated.IsZero() {
		updated = st.Updated.UnixMilli()
	}
	dns := 0
	if st.DNS {
		dns = 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = owned(ctx, tx, s.env, opID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO ingress VALUES(?,?,?,?,?,?,?) ON CONFLICT(environment) DO UPDATE SET account_id=excluded.account_id,api_token_secret=excluded.api_token_secret,tunnel_id=excluded.tunnel_id,tunnel_name=excluded.tunnel_name,dns=excluded.dns,updated=excluded.updated`, s.env, st.AccountID, st.APITokenSecret, st.TunnelID, st.TunnelName, dns, updated)
	if err != nil {
		return err
	}
	// A tunnel in use again is no longer one that was left behind.
	if _, err = tx.ExecContext(ctx, `DELETE FROM metadata WHERE name=?`, s.keptTunnelName()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) keptTunnelName() string { return "kept_tunnel:" + s.env }

// KeptTunnel returns the tunnel that managed ingress left behind when it was removed, or sql.ErrNoRows when there is none.
// Only names and IDs are kept, never a token.
func (s *Store) KeptTunnel(ctx context.Context) (IngressState, error) {
	var raw []byte
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE name=?`, s.keptTunnelName()).Scan(&raw); err != nil {
		return IngressState{}, err
	}
	var st IngressState
	if err := json.Unmarshal(raw, &st); err != nil || st.TunnelID == "" || st.AccountID == "" {
		return IngressState{}, errors.New("invalid kept tunnel")
	}
	return st, nil
}

// SetKeptTunnel remembers the tunnel of st, so a later run can still find it after the ingress state is cleared. It
// requires the environment lock to be held by opID.
func (s *Store) SetKeptTunnel(ctx context.Context, opID string, st IngressState) error {
	if st.AccountID == "" || st.APITokenSecret == "" || st.TunnelID == "" || st.TunnelName == "" {
		return errors.New("invalid tunnel")
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = owned(ctx, tx, s.env, opID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO metadata VALUES(?,?) ON CONFLICT(name) DO UPDATE SET value=excluded.value`, s.keptTunnelName(), raw); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearKeptTunnel forgets the remembered tunnel. It is not an error when there is none. It requires the environment lock to
// be held by opID.
func (s *Store) ClearKeptTunnel(ctx context.Context, opID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = owned(ctx, tx, s.env, opID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM metadata WHERE name=?`, s.keptTunnelName()); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearIngress removes the environment's managed ingress configuration and every route in it.
// It requires the environment lock to be held by opID.
func (s *Store) ClearIngress(ctx context.Context, opID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = owned(ctx, tx, s.env, opID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM ingress_routes WHERE environment=?`, s.env); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM ingress WHERE environment=?`, s.env); err != nil {
		return err
	}
	return tx.Commit()
}

// ForgetApp removes an app's bindings and applied release, and keeps its release history. Removing managed ingress
// uses it for the connector app. It requires the environment lock to be held by opID.
func (s *Store) ForgetApp(ctx context.Context, opID, app string) error {
	if app == "" {
		return errors.New("app required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = owned(ctx, tx, s.env, opID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM applied WHERE environment=? AND app=?`, s.env, app); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM bindings WHERE environment=? AND app=?`, s.env, app); err != nil {
		return err
	}
	return tx.Commit()
}

// IngressRoutes returns the routes of app, or of every app when app is empty, ordered by hostname.
func (s *Store) IngressRoutes(ctx context.Context, app string) ([]IngressRoute, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT hostname,app,role,service,zone_id,dns_record_id,route_created,dns_created,dns_prior_comment FROM ingress_routes WHERE environment=? AND (?='' OR app=?) ORDER BY hostname`, s.env, app, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IngressRoute
	for rows.Next() {
		var r IngressRoute
		var routeCreated, dnsCreated int
		if err = rows.Scan(&r.Hostname, &r.App, &r.Role, &r.Service, &r.ZoneID, &r.DNSRecordID, &routeCreated, &dnsCreated, &r.DNSPriorComment); err != nil {
			return nil, err
		}
		r.RouteCreated, r.DNSCreated = routeCreated != 0, dnsCreated != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetIngressRoute records a route, replacing the route for the same hostname.
// It requires the environment lock to be held by opID.
func (s *Store) SetIngressRoute(ctx context.Context, opID string, r IngressRoute) error {
	if r.Hostname == "" || r.App == "" || r.Role == "" || r.Service == "" {
		return errors.New("invalid ingress route")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = owned(ctx, tx, s.env, opID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO ingress_routes(environment,hostname,app,role,service,zone_id,dns_record_id,route_created,dns_created,dns_prior_comment) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(environment,hostname) DO UPDATE SET app=excluded.app,role=excluded.role,service=excluded.service,zone_id=excluded.zone_id,dns_record_id=excluded.dns_record_id,route_created=excluded.route_created,dns_created=excluded.dns_created,dns_prior_comment=excluded.dns_prior_comment`, s.env, r.Hostname, r.App, r.Role, r.Service, r.ZoneID, r.DNSRecordID, boolInt(r.RouteCreated), boolInt(r.DNSCreated), r.DNSPriorComment)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ForgetIngressRoutes removes every route row of app. It is not an error when app has none. Removing an app uses it to
// leave no row behind once Cloudflare is done. It requires the environment lock to be held by opID.
func (s *Store) ForgetIngressRoutes(ctx context.Context, opID, app string) error {
	if app == "" {
		return errors.New("app required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = owned(ctx, tx, s.env, opID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM ingress_routes WHERE environment=? AND app=?`, s.env, app); err != nil {
		return err
	}
	return tx.Commit()
}

// AppliedApps lists the apps that have an applied release, in name order.
func (s *Store) AppliedApps(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT app FROM applied WHERE environment=? ORDER BY app`, s.env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var app string
		if err = rows.Scan(&app); err != nil {
			return nil, err
		}
		out = append(out, app)
	}
	return out, rows.Err()
}

// DeleteIngressRoute removes the route for hostname. It returns sql.ErrNoRows when there is none.
// It requires the environment lock to be held by opID.
func (s *Store) DeleteIngressRoute(ctx context.Context, opID, hostname string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = owned(ctx, tx, s.env, opID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM ingress_routes WHERE environment=? AND hostname=?`, s.env, hostname)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func (s *Store) Protected(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM protection WHERE environment=?`, s.env).Scan(&n)
	return n != 0, err
}
func (s *Store) Bindings(ctx context.Context, app string) ([]Binding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT app,role,name,service_id FROM bindings WHERE environment=? AND (?='' OR app=?) ORDER BY app,role`, s.env, app, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Binding
	for rows.Next() {
		var b Binding
		if err = rows.Scan(&b.App, &b.Role, &b.Name, &b.ServiceID); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
