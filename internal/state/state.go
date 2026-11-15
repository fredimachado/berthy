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
	RequestHash                                              [32]byte
	PublicRequest                                            json.RawMessage
}
type Operation struct {
	ID string
	Admission
	State                          string
	Started                        time.Time
	Err, SchemaBefore, SchemaAfter string
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
	Manifest            json.RawMessage
	Created             time.Time
	Bindings            []Binding
}
type Actor struct{ ID, Name, Remote string }
type SecretInfo struct{ Name, Registry string }
type Outcome struct {
	Protected                      bool
	Err, SchemaBefore, SchemaAfter string
	State                          string
	Event                          json.RawMessage
	Release                        *Release
}
type Lock struct {
	OperationID, Holder, Reason string
	Acquired                    time.Time
}
type Audit struct {
	OperationID, ActorID, ActorName, Remote, Action, Outcome string
	Time                                                     time.Time
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
	if !fresh && version != 1 {
		return fail(errors.New("unsupported or uninitialized state schema"))
	}
	tx, err := db.Begin()
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`
 CREATE TABLE IF NOT EXISTS protection (environment TEXT PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS metadata (name TEXT PRIMARY KEY, value BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS operations (id TEXT PRIMARY KEY, environment TEXT NOT NULL, request_id TEXT NOT NULL, actor_id TEXT NOT NULL, actor_name TEXT NOT NULL, remote TEXT NOT NULL, kind TEXT NOT NULL, app TEXT NOT NULL, commit_sha TEXT NOT NULL, request_hash BLOB NOT NULL, public_request BLOB NOT NULL, state TEXT NOT NULL, started INTEGER NOT NULL, error TEXT NOT NULL, schema_before TEXT NOT NULL, schema_after TEXT NOT NULL, UNIQUE(environment,request_id));
 CREATE TABLE IF NOT EXISTS locks (environment TEXT PRIMARY KEY, operation_id TEXT REFERENCES operations(id), holder TEXT NOT NULL, reason TEXT NOT NULL, acquired INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS events (id INTEGER PRIMARY KEY AUTOINCREMENT, operation_id TEXT NOT NULL REFERENCES operations(id), kind TEXT NOT NULL, data BLOB NOT NULL, created INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS intents (operation_id TEXT NOT NULL REFERENCES operations(id), ordinal INTEGER NOT NULL, kind TEXT NOT NULL, role TEXT NOT NULL, name TEXT NOT NULL, service_id TEXT NOT NULL, generation TEXT NOT NULL, seen_version INTEGER NOT NULL, deadline INTEGER NOT NULL, before_sealed BLOB, desired_sealed BLOB NOT NULL, phase TEXT NOT NULL, receipt BLOB NOT NULL, PRIMARY KEY(operation_id,ordinal));
 CREATE TABLE IF NOT EXISTS releases (id INTEGER PRIMARY KEY AUTOINCREMENT, environment TEXT NOT NULL, app TEXT NOT NULL, digest TEXT NOT NULL, commit_sha TEXT NOT NULL, manifest BLOB NOT NULL, created INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS applied (environment TEXT NOT NULL, app TEXT NOT NULL, release_id INTEGER NOT NULL REFERENCES releases(id), PRIMARY KEY(environment,app));
 CREATE TABLE IF NOT EXISTS bindings (environment TEXT NOT NULL, app TEXT NOT NULL, role TEXT NOT NULL, name TEXT NOT NULL UNIQUE, service_id TEXT NOT NULL UNIQUE, PRIMARY KEY(environment,app,role));
 CREATE TABLE IF NOT EXISTS secrets (environment TEXT NOT NULL, name TEXT NOT NULL, registry TEXT NOT NULL, sealed BLOB NOT NULL, PRIMARY KEY(environment,name));
 CREATE TABLE IF NOT EXISTS audit (id INTEGER PRIMARY KEY AUTOINCREMENT, environment TEXT NOT NULL, operation_id TEXT NOT NULL, actor_id TEXT NOT NULL, actor_name TEXT NOT NULL, remote TEXT NOT NULL, action TEXT NOT NULL, outcome TEXT NOT NULL, created INTEGER NOT NULL);`); err != nil {
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
		if err == nil && string(plain) != "berthy-state" {
			err = errors.New("invalid state key")
		}
	}
	if err != nil {
		tx.Rollback()
		return fail(fmt.Errorf("state key: %w", err))
	}
	if _, err = tx.Exec(`PRAGMA user_version=1`); err != nil {
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
		case "app", "version", "commit", "state", "role", "step", "status", "digest", "code", "App", "Version", "Commit":
			var s string
			if json.Unmarshal(v, &s) != nil || len(s) > 1024 {
				return false
			}
		case "adopt", "Adopt":
			var x bool
			if json.Unmarshal(v, &x) != nil {
				return false
			}
		case "exit_code", "exit_status":
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
	op, err := operationQuery(tx.QueryRowContext(ctx, `SELECT id,request_id,actor_id,actor_name,remote,kind,app,commit_sha,request_hash,public_request,state,started,error,schema_before,schema_after FROM operations WHERE environment=? AND request_id=?`, s.env, a.RequestID))
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
	_, err = tx.ExecContext(ctx, `INSERT INTO operations VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, s.env, a.RequestID, a.ActorID, a.ActorName, a.Remote, a.Kind, a.App, a.Commit, a.RequestHash[:], []byte(a.PublicRequest), "running", now(), "", "", "")
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
	err := row.Scan(&o.ID, &o.RequestID, &o.ActorID, &o.ActorName, &o.Remote, &o.Kind, &o.App, &o.Commit, &hash, &o.PublicRequest, &o.State, &started, &o.Err, &o.SchemaBefore, &o.SchemaAfter)
	o.Started = time.UnixMilli(started).UTC()
	copy(o.RequestHash[:], hash)
	return o, err
}
func (s *Store) Operation(ctx context.Context, id string) (Operation, error) {
	return operationQuery(s.db.QueryRowContext(ctx, `SELECT id,request_id,actor_id,actor_name,remote,kind,app,commit_sha,request_hash,public_request,state,started,error,schema_before,schema_after FROM operations WHERE environment=? AND id=?`, s.env, id))
}
func (s *Store) Active(ctx context.Context) (Operation, error) {
	return operationQuery(s.db.QueryRowContext(ctx, `SELECT o.id,o.request_id,o.actor_id,o.actor_name,o.remote,o.kind,o.app,o.commit_sha,o.request_hash,o.public_request,o.state,o.started,o.error,o.schema_before,o.schema_after FROM operations o JOIN locks l ON l.operation_id=o.id WHERE l.environment=?`, s.env))
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
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = owned(ctx, tx, s.env, id); err != nil {
		return err
	}
	op, err := operationQuery(tx.QueryRowContext(ctx, `SELECT id,request_id,actor_id,actor_name,remote,kind,app,commit_sha,request_hash,public_request,state,started,error,schema_before,schema_after FROM operations WHERE environment=? AND id=?`, s.env, id))
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
		res, e := tx.ExecContext(ctx, `INSERT INTO releases(environment,app,digest,commit_sha,manifest,created) VALUES(?,?,?,?,?,?)`, s.env, r.App, r.Digest, r.Commit, []byte(r.Manifest), now())
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
	if _, err = tx.ExecContext(ctx, `UPDATE operations SET state=?,error=?,schema_before=?,schema_after=? WHERE id=?`, o.State, o.Err, o.SchemaBefore, o.SchemaAfter, id); err != nil {
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
func (s *Store) History(ctx context.Context, app string) ([]Release, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,app,digest,commit_sha,manifest,created FROM releases WHERE environment=? AND (?='' OR app=?) ORDER BY id DESC`, s.env, app, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		var r Release
		var ms int64
		if err = rows.Scan(&r.ID, &r.App, &r.Digest, &r.Commit, &r.Manifest, &ms); err != nil {
			return nil, err
		}
		r.Created = time.UnixMilli(ms).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) PutSecret(ctx context.Context, actor Actor, info SecretInfo, value []byte) error {
	if actor.ID == "" {
		return errors.New("actor required")
	}
	if info.Name == "" {
		return errors.New("empty secret name")
	}
	sealed, err := s.seal("secret:"+info.Name+"\x00"+info.Registry, value)
	if err != nil {
		return err
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
	_, err = tx.ExecContext(ctx, `INSERT INTO secrets VALUES(?,?,?,?) ON CONFLICT(environment,name) DO UPDATE SET registry=excluded.registry,sealed=excluded.sealed`, s.env, info.Name, info.Registry, sealed)
	if err != nil {
		return err
	}
	if actor.ID != "" {
		if err = auditTx(ctx, tx, s.env, "", Admission{ActorID: actor.ID, ActorName: actor.Name, Remote: actor.Remote, Kind: "secret_set:" + info.Name}, "succeeded"); err != nil {
			return err
		}
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
	err := s.db.QueryRowContext(ctx, `SELECT r.id,r.app,r.digest,r.commit_sha,r.manifest,r.created FROM releases r JOIN applied a ON a.release_id=r.id WHERE a.environment=? AND a.app=?`, s.env, app).Scan(&r.ID, &r.App, &r.Digest, &r.Commit, &r.Manifest, &ms)
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
