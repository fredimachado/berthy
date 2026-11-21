package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"time"

	"berthy/internal/manifest"
)

// Settings are the environment's settings: the environment scope of the manifest that a deploy or berthy apply last applied.
// The agent runs from them and from the app scope of each app's applied release, so a rollback of one app never changes them.
type Settings struct {
	// Project is the project of the manifest the settings came from. An environment runs one project.
	Project string
	// Commit and Modified are the commit the settings came from and whether that checkout had local changes.
	Commit   string
	Modified bool
	// OperationID is the deploy that applied them, and empty for berthy apply.
	OperationID string
	// Manifest is the environment scope of the manifest, as JSON.
	Manifest json.RawMessage
	// Apps are the names of the apps the manifest names, in name order. An app that is deployed but not among them is an orphan.
	Apps    []string
	Updated time.Time
}

// NewSettings builds the settings that applying m records. commit and modified say where m came from.
func NewSettings(m manifest.Manifest, commit string, modified bool) (*Settings, error) {
	raw, err := json.Marshal(m.EnvScope())
	if err != nil {
		return nil, err
	}
	return &Settings{Project: m.Project, Commit: commit, Modified: modified, Manifest: raw, Apps: slices.Sorted(maps.Keys(m.Apps))}, nil
}

// Settings returns the environment's settings, and false when none was ever applied.
func (s *Store) Settings(ctx context.Context) (Settings, bool, error) {
	var st Settings
	var ms int64
	var apps []byte
	err := s.db.QueryRowContext(ctx, `SELECT project,manifest,apps,commit_sha,modified,operation_id,updated FROM environment_settings WHERE environment=?`, s.env).Scan(&st.Project, &st.Manifest, &apps, &st.Commit, &st.Modified, &st.OperationID, &ms)
	if errors.Is(err, sql.ErrNoRows) {
		return Settings{}, false, nil
	}
	if err == nil {
		err = json.Unmarshal(apps, &st.Apps)
	}
	if err != nil {
		return Settings{}, false, err
	}
	st.Updated = fromMilli(ms)
	return st, true, nil
}

// putSettings writes st as the environment's settings. The caller holds the transaction.
func (s *Store) putSettings(ctx context.Context, tx *sql.Tx, st Settings, operationID string) error {
	if st.Project == "" || !validJSON(st.Manifest) {
		return errors.New("invalid settings")
	}
	apps, err := json.Marshal(st.Apps)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO environment_settings(environment,project,manifest,apps,commit_sha,modified,operation_id,updated) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(environment) DO UPDATE SET project=excluded.project,manifest=excluded.manifest,apps=excluded.apps,commit_sha=excluded.commit_sha,modified=excluded.modified,operation_id=excluded.operation_id,updated=excluded.updated`, s.env, st.Project, []byte(st.Manifest), apps, st.Commit, boolInt(st.Modified), operationID, now())
	return err
}

// ApplySettings makes st the environment's settings, as berthy apply does, and audits it as apply. It is refused with
// ErrConflict while anything holds the environment lock. protected makes the environment's protection permanent, as a deploy of a protected manifest does.
func (s *Store) ApplySettings(ctx context.Context, actor Actor, st Settings, protected bool) error {
	return s.applySettings(ctx, actor, st, protected, "")
}

// ApplySettingsOwned writes settings while opID holds the environment operation lock.
func (s *Store) ApplySettingsOwned(ctx context.Context, actor Actor, st Settings, protected bool, opID string) error {
	return s.applySettings(ctx, actor, st, protected, opID)
}

func (s *Store) applySettings(ctx context.Context, actor Actor, st Settings, protected bool, opID string) error {
	if actor.ID == "" {
		return errors.New("actor required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if opID == "" {
		err = s.checkNoLock(ctx, tx)
	} else {
		err = owned(ctx, tx, s.env, opID)
	}
	if err != nil {
		return err
	}
	if err = s.putSettings(ctx, tx, st, ""); err != nil {
		return err
	}
	if protected {
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO protection VALUES(?)`, s.env); err != nil {
			return err
		}
	}
	if opID == "" {
		if err = auditTx(ctx, tx, s.env, "", Admission{ActorID: actor.ID, ActorName: actor.Name, Remote: actor.Remote, Kind: "apply"}, "succeeded"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LatestReleases returns the newest release of every app that was ever deployed, whether or not the app is applied now. It
// includes the ingress connector's. Bindings are not filled in.
func (s *Store) LatestReleases(ctx context.Context) (map[string]Release, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,app,digest,commit_sha,manifest,created,operation_id,schema_before,schema_after,tag,modified FROM releases WHERE environment=? AND id IN (SELECT max(id) FROM releases WHERE environment=? GROUP BY app)`, s.env, s.env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Release{}
	for rows.Next() {
		var r Release
		var ms int64
		if err = rows.Scan(&r.ID, &r.App, &r.Digest, &r.Commit, &r.Manifest, &ms, &r.OperationID, &r.SchemaBefore, &r.SchemaAfter, &r.Tag, &r.Modified); err != nil {
			return nil, err
		}
		r.Created = time.UnixMilli(ms).UTC()
		out[r.App] = r
	}
	return out, rows.Err()
}

// BeginApply acquires the operation lock before an apply reserves Docker names.
func (s *Store) BeginApply(ctx context.Context, actor Actor) (Operation, error) {
	id, err := randomID()
	if err != nil {
		return Operation{}, err
	}
	op, _, err := s.Admit(ctx, Admission{RequestID: "apply-" + id, ActorID: actor.ID, ActorName: actor.Name, Remote: actor.Remote, Kind: "apply", PublicRequest: []byte(`{}`)})
	return op, err
}
