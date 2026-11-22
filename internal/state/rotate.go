package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
)

// sealedColumn is one column that holds sealed values, and how to find the record each value was sealed under. query returns
// one row per value: the row ID, the environment the value belongs to, two strings that name the row, and the sealed value.
// record builds the record from the two names. A rotation reseals every column listed here, so a column that holds sealed
// values and is missing from sealedColumns is a bug; TestEverySealedColumnIsRotated reads the schema to catch it.
type sealedColumn struct {
	table, column string
	query         string
	record        func(a, b string) string
}

func secretRecord(name, registry string) string { return "secret:" + name + "\x00" + registry }
func intentRecord(opID string, ordinal int, part string) string {
	return "intent:" + opID + ":" + strconv.Itoa(ordinal) + ":" + part
}
func diagnosticRecord(opID string, seq int) string {
	return "diagnostic:" + opID + ":" + strconv.Itoa(seq)
}
func approvalRecord(id string) string     { return "approval:" + id + ":request" }
func jobOutputRecord(runID string) string { return "job_output:" + runID }

func intentPart(part string) func(a, b string) string {
	return func(opID, ordinal string) string {
		n, _ := strconv.Atoi(ordinal)
		return intentRecord(opID, n, part)
	}
}

const intentRows = `SELECT i.rowid,o.environment,i.operation_id,CAST(i.ordinal AS TEXT),i.%s FROM intents i JOIN operations o ON o.id=i.operation_id WHERE i.%[1]s IS NOT NULL`

var sealedColumns = []sealedColumn{
	{"metadata", "value", `SELECT rowid,'',name,'',value FROM metadata WHERE name='` + keyCheckName + `'`, func(a, b string) string { return keyCheckName }},
	{"secrets", "sealed", `SELECT rowid,environment,name,registry,sealed FROM secrets`, secretRecord},
	{"intents", "before_sealed", fmt.Sprintf(intentRows, "before_sealed"), intentPart("before")},
	{"intents", "desired_sealed", fmt.Sprintf(intentRows, "desired_sealed"), intentPart("desired")},
	{"intents", "receipt", fmt.Sprintf(intentRows, "receipt"), intentPart("receipt")},
	{"diagnostics", "sealed", `SELECT d.rowid,o.environment,d.operation_id,CAST(d.seq AS TEXT),d.sealed FROM diagnostics d JOIN operations o ON o.id=d.operation_id`, func(opID, seq string) string {
		n, _ := strconv.Atoi(seq)
		return diagnosticRecord(opID, n)
	}},
	{"job_runs", "output_sealed", `SELECT rowid,environment,CAST(id AS TEXT),'',output_sealed FROM job_runs WHERE output_sealed IS NOT NULL`, func(id, _ string) string { return jobOutputRecord(id) }},
	{"approvals", "request_sealed", `SELECT rowid,environment,id,'',request_sealed FROM approvals`, func(id, _ string) string { return approvalRecord(id) }},
}

type sealedValue struct {
	rowid  int64
	env    string
	record string
	sealed []byte
}

// values reads every value of the column. All rows are read before any is changed.
func (c sealedColumn) values(ctx context.Context, tx *sql.Tx) ([]sealedValue, error) {
	rows, err := tx.QueryContext(ctx, c.query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sealedValue
	for rows.Next() {
		var v sealedValue
		var a, b string
		if err = rows.Scan(&v.rowid, &v.env, &a, &b, &v.sealed); err != nil {
			return nil, err
		}
		v.record = c.record(a, b)
		out = append(out, v)
	}
	return out, rows.Err()
}

// ErrKeyFiles means a rotation committed, so the state is sealed with the new key, but the key files could not be renamed. The
// agent finishes the renames the next time it starts.
var ErrKeyFiles = errors.New("the state is sealed with the new key, but its key files are not in place yet; the agent finishes them when it starts")

// RotateReport says what a rotation did, or with a dry run what it would do. The key IDs name keys and never reveal them.
type RotateReport struct {
	OldKeyID string
	// NewKeyID is empty for a dry run.
	NewKeyID string
	// Values is how many sealed values were sealed again.
	Values int
	// KeyFile is the file that holds the current key.
	KeyFile string
}

// RotateKey replaces the key the state is sealed with. It writes a new key beside the key file, seals every value again in one
// transaction and records the rotation in the audit log, then renames the key files: the old key is kept as <key>.prev until
// ForgetPreviousKey drops it. It refuses with ErrConflict while an operation runs or the lock is held. A crash leaves a state
// that Open settles: the transaction either committed or it did not, and the key check says which key is current.
// With dryRun it checks the same and counts the values, and changes nothing.
func (s *Store) RotateKey(ctx context.Context, actor Actor, dryRun bool) (RotateReport, error) {
	rep := RotateReport{KeyFile: s.keyFile}
	if actor.ID == "" {
		return rep, errors.New("actor required")
	}
	if !s.rotate.TryLock() {
		return rep, ErrConflict
	}
	defer s.rotate.Unlock()
	// The transaction holds the store's only connection, so nothing else changes the state until it ends.
	tx, err := s.begin(ctx)
	if err != nil {
		return rep, err
	}
	defer tx.Rollback()
	if err = s.checkNoLock(ctx, tx); err != nil {
		return rep, err
	}
	var running int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM operations WHERE environment=? AND state='running'`, s.env).Scan(&running); err != nil {
		return rep, err
	}
	if running != 0 {
		return rep, ErrConflict
	}
	rep.OldKeyID = s.KeyID()
	if dryRun {
		for _, c := range sealedColumns {
			values, err := c.values(ctx, tx)
			if err != nil {
				return rep, err
			}
			rep.Values += len(values)
		}
		return rep, nil
	}
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return rep, err
	}
	next, err := newStateKey(raw)
	if err != nil {
		return rep, err
	}
	rep.NewKeyID = next.id.String()
	if err = writeKeyFile(s.keyFile+KeyNextSuffix, raw); err != nil {
		return rep, fmt.Errorf("cannot write the next key: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			os.Remove(s.keyFile + KeyNextSuffix)
		}
	}()

	// No value is sealed or opened by anyone else from here to the swap: seal and open wait for the lock.
	s.mu.Lock()
	locked := true
	unlock := func() {
		if locked {
			s.mu.Unlock()
			locked = false
		}
	}
	defer unlock()
	old := s.keys
	for _, c := range sealedColumns {
		values, err := c.values(ctx, tx)
		if err != nil {
			return rep, err
		}
		for _, v := range values {
			plain, err := openWith(old, v.env, v.record, v.sealed)
			if err != nil {
				return rep, fmt.Errorf("%s.%s: %w", c.table, c.column, err)
			}
			resealed, err := sealWith(next, v.env, v.record, plain)
			if err != nil {
				return rep, err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE `+c.table+` SET `+c.column+`=? WHERE rowid=?`, resealed, v.rowid); err != nil {
				return rep, err
			}
			rep.Values++
		}
	}
	if err = auditTx(ctx, tx, s.env, "", Admission{ActorID: actor.ID, ActorName: actor.Name, Remote: actor.Remote, Kind: "key_rotate"}, "succeeded"); err != nil {
		return rep, err
	}
	if err = tx.Commit(); err != nil {
		return rep, err
	}
	committed = true
	s.keys = keyring{current: next, others: []stateKey{old.current}}
	unlock()
	if err = finishRotation(s.keyFile, old.current); err != nil {
		return rep, fmt.Errorf("%w: %v", ErrKeyFiles, err)
	}
	return rep, nil
}
