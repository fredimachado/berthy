package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"berthy/internal/backup"
	"berthy/internal/engine"
	"berthy/internal/state"
)

// RestorePlan is what restoring a backup would do: which backup it is and the new volume it fills. Building it changes nothing.
type RestorePlan struct {
	Key      string    `json:"key"`
	Backup   string    `json:"backup"`
	App      string    `json:"app"`
	Volume   string    `json:"volume"`
	Into     string    `json:"into"`
	Size     int64     `json:"size"`
	SHA256   string    `json:"sha256"`
	Finished time.Time `json:"finished"`
}

// VolumeRestorer reads backups from object storage and fills new volumes from them. The agent supplies it.
type VolumeRestorer interface {
	// CheckRestore refuses a restore that cannot work: the key names no backup with a sidecar, or the volume exists or has an
	// invalid name. It changes nothing. Its refusals are backup.Refusal values.
	CheckRestore(ctx context.Context, key, into string) (RestorePlan, error)
	// Restore creates the volume into, labelled with the ID of the operation, and extracts the backup into it. It removes
	// the volume again when it fails.
	Restore(ctx context.Context, operationID, key, into string) error
}

const (
	errRestoreAdmin       Refusal = "restoring a backup requires administrator access"
	errRestoreArgs        Refusal = "a backup key and a new volume name are required"
	errRestoreUnavailable Refusal = "restore unavailable"
	errRestoreFailed      Refusal = "restore failed"
	errRestoreChecksum    Refusal = "backup checksum mismatch"
	errRestoreJournal     Refusal = "restore journal failed"
	errRestoreInterrupted Refusal = "restore interrupted; remove the new volume if it exists and run restore again"
	errRestoreCleaned     Refusal = "restore interrupted; no partial volume is left"

	restoreKind = "restore"
)

// restoreRefusal is the public reason a restore stopped. A refusal of the backup package that holds no names, and a checksum
// mismatch, keep their texts; anything else, which may name files or storage, is fallback.
func restoreRefusal(err error, fallback Refusal) Refusal {
	var r backup.Refusal
	switch {
	case errors.Is(err, backup.ErrChecksumMismatch):
		return errRestoreChecksum
	case errors.As(err, &r) && backup.PublicRefusal(string(r)):
		return Refusal(r)
	}
	return fallback
}

// restoreCheck is what a restore decides before it takes the lock: the caller is an administrator, the names are given, and
// the restorer accepts the backup and the volume.
func (c *Controller) restoreCheck(ctx context.Context, actor Actor, req Request) (RestorePlan, error) {
	switch {
	case !actor.Admin:
		return RestorePlan{}, errRestoreAdmin
	case req.RestoreKey == "" || req.RestoreInto == "":
		return RestorePlan{}, errRestoreArgs
	case c.Restorer == nil:
		return RestorePlan{}, errRestoreUnavailable
	}
	plan, err := c.Restorer.CheckRestore(ctx, req.RestoreKey, req.RestoreInto)
	if err != nil {
		return RestorePlan{}, restoreRefusal(err, errRestoreUnavailable)
	}
	return plan, nil
}

func (c *Controller) planRestore(ctx context.Context, actor Actor, req Request) (Plan, error) {
	p, err := c.restoreCheck(ctx, actor, req)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Roles: []string{}, Restore: &p}, nil
}

// runRestore fills the new volume from the backup. The run is journaled first, so a recovery after a crash knows a restore was
// under way. Failure leaves no volume: the restorer removes the one it created.
func (c *Controller) runRestore(ctx context.Context, op state.Operation, actor Actor, req Request) state.Outcome {
	step := func(text string) {
		if data, err := json.Marshal(map[string]string{"step": text}); err == nil {
			_ = c.Store.AppendEvent(ctx, op.ID, state.Event{Kind: "step", Data: data})
		}
	}
	if _, err := c.restoreCheck(ctx, actor, req); err != nil {
		return state.Outcome{State: "failed", Err: string(restoreRefusal(err, errRestoreFailed))}
	}
	desired, err := json.Marshal(struct {
		Key  string `json:"key"`
		Into string `json:"into"`
	}{req.RestoreKey, req.RestoreInto})
	if err != nil {
		return state.Outcome{State: "failed", Err: string(errRestoreJournal)}
	}
	if err = c.Store.RecordIntent(ctx, op.ID, state.Intent{Ordinal: 1, Kind: restoreKind, Name: req.RestoreInto, Desired: desired}); err != nil {
		return state.Outcome{State: "failed", Err: string(errRestoreJournal)}
	}
	if err = c.Store.MarkStep(ctx, op.ID, 1, "issued", json.RawMessage(`{}`)); err != nil {
		return state.Outcome{State: "failed", Err: string(errRestoreJournal)}
	}
	step("restoring " + req.RestoreKey + " into volume " + req.RestoreInto)
	err = c.Restorer.Restore(ctx, op.ID, req.RestoreKey, req.RestoreInto)
	if err != nil {
		_ = c.Store.MarkStep(ctx, op.ID, 1, "finished", json.RawMessage(`{"ok":false}`))
		return state.Outcome{State: "failed", Err: string(restoreRefusal(err, errRestoreFailed))}
	}
	if err = c.Store.MarkStep(ctx, op.ID, 1, "finished", json.RawMessage(`{"ok":true}`)); err != nil {
		return state.Outcome{State: "failed", Err: string(errRestoreJournal)}
	}
	step("restored volume " + req.RestoreInto)
	return state.Outcome{State: "succeeded"}
}

// restoreCleanupEngine is the part of the Docker API that a restore recovery uses. The Controller's Engine may implement it.
type restoreCleanupEngine interface {
	InventoryContainers(context.Context) ([]engine.InventoryContainer, error)
	RemoveContainer(context.Context, string) error
	VolumeLabels(context.Context, string) (map[string]string, bool, error)
	RemoveVolume(context.Context, string) error
}

// recoverRestore ends a restore that an agent stop or crash interrupted. It removes the restore helper container the dead
// agent left, which holds the new volume, and then the volume itself, but only a volume that carries this operation's ID in its
// label: whatever else has that name is not this restore's, and stays. The operation says whether a partial volume may remain.
func (c *Controller) recoverRestore(ctx context.Context, op state.Operation, intents []state.Intent) error {
	for _, i := range intents {
		var r struct {
			OK bool `json:"ok"`
		}
		// A restore that journaled its success finished; only the agent's last write was lost. Its volume is complete.
		if i.Kind == restoreKind && i.Phase == "finished" && json.Unmarshal(i.Receipt, &r) == nil && r.OK {
			return c.finish(ctx, op, state.Outcome{State: "succeeded"}, target{})
		}
	}
	msg := errRestoreInterrupted
	if c.cleanRestore(ctx, op, intents) {
		msg = errRestoreCleaned
	}
	return c.finish(ctx, op, state.Outcome{State: "interrupted", Err: string(msg)}, target{})
}

// cleanRestore reports whether no partial volume of the operation is left.
func (c *Controller) cleanRestore(ctx context.Context, op state.Operation, intents []state.Intent) bool {
	de, ok := c.Engine.(restoreCleanupEngine)
	if !ok {
		return false
	}
	var into string
	for _, i := range intents {
		var d struct {
			Into string `json:"into"`
		}
		if i.Kind == restoreKind && json.Unmarshal(i.Desired, &d) == nil {
			into = d.Into
		}
	}
	if into == "" {
		return true // the restore had not begun, so it made no volume
	}
	containers, err := de.InventoryContainers(ctx)
	if err != nil {
		return false
	}
	for _, ctr := range containers {
		if ctr.Labels["berthy.environment"] == c.Environment && ctr.Labels["berthy.helper"] == "restore" {
			if err = de.RemoveContainer(ctx, ctr.ID); err != nil && !statusIs(err, 404) {
				return false
			}
		}
	}
	labels, exists, err := de.VolumeLabels(ctx, into)
	switch {
	case err != nil:
		return false
	case !exists:
		return true
	case labels[backup.RestoreOperationLabel] != op.ID:
		return false
	}
	if err = de.RemoveVolume(ctx, into); err != nil && !statusIs(err, 404) {
		return false
	}
	return true
}

// composeConfigVolumes is the part of `compose config --format json` that names a project's volumes.
type composeConfigVolumes struct {
	Volumes map[string]composeConfigVolume `json:"volumes"`
}

// ComposeVolumeNames returns the Docker name of each top-level volume of a deployed Compose project, by its key in the
// Compose file. Compose resolves the name: the project name and the key, an explicit name, or an external volume's own name.
// It reads the project's stored files and changes nothing.
func (c *Controller) ComposeVolumeNames(ctx context.Context, project string) (map[string]string, error) {
	if c.Compose == nil || c.StateDir == "" {
		return nil, errComposeUnavailable
	}
	out, _, err := c.Compose.Run(ctx, engine.ComposeRun{Project: project, Dir: c.composeDir(project), Args: []string{"config", "--format", "json", "--no-env-resolution"}})
	if err != nil {
		return nil, errComposeConfig
	}
	var cfg composeConfigVolumes
	if json.Unmarshal(out, &cfg) != nil {
		return nil, errComposeConfig
	}
	names := make(map[string]string, len(cfg.Volumes))
	for key, v := range cfg.Volumes {
		if v.Name == "" {
			return nil, errComposeConfig
		}
		names[key] = v.Name
	}
	return names, nil
}
