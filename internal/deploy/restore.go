package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"berthy/internal/backup"
	"berthy/internal/engine"
	"berthy/internal/jobs"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// RestorePlan is what restoring a backup would do: which backup it is and the new volume it fills, or the command it feeds and
// where that runs. Building it changes nothing.
type RestorePlan struct {
	Key    string `json:"key"`
	Backup string `json:"backup"`
	App    string `json:"app"`
	// Volume is the volume a volume backup holds, and Into the new volume it fills. A command backup has neither.
	Volume string `json:"volume,omitempty"`
	Into   string `json:"into,omitempty"`
	// Target says where the command of a restore into a command runs, such as "db service postgres", and Command is the command.
	Target   string    `json:"target,omitempty"`
	Command  []string  `json:"command,omitempty"`
	Size     int64     `json:"size"`
	SHA256   string    `json:"sha256"`
	Finished time.Time `json:"finished"`
}

// VolumeRestorer reads backups from object storage and fills new volumes, or the input of a command, from them. The agent
// supplies it.
type VolumeRestorer interface {
	// CheckRestore refuses a restore that cannot work: the key names no backup with a sidecar, or the volume exists or has an
	// invalid name. It changes nothing. Its refusals are backup.Refusal values.
	CheckRestore(ctx context.Context, key, into string) (RestorePlan, error)
	// Restore creates the volume into, labelled with the ID of the operation, and extracts the backup into it. It removes
	// the volume again when it fails.
	Restore(ctx context.Context, operationID, key, into string) error
	// CheckRestoreCommand refuses a restore into a command that cannot work: the key names no backup with a sidecar, or a backup
	// of a volume. It changes nothing. Its refusals are backup.Refusal values.
	CheckRestoreCommand(ctx context.Context, key string) (RestorePlan, error)
	// RestoreCommand checks the backup's SHA-256 and then streams it, decompressed, into the input of the job's command, which it
	// runs as a job is run. A command that did not do its work is a *backup.CommandError. It returns how the command ended.
	RestoreCommand(ctx context.Context, operationID, key string, job manifest.Job) (jobs.Outcome, error)
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

	errRestoreCommandFailed   Refusal = "restore command failed"
	errRestoreExecInterrupted Refusal = "restore interrupted; the command may have loaded part of the backup"
	errRestoreRoleOrService   Refusal = "name a role or a service, not both"

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
// the restorer accepts the backup and the volume, or the command and where it runs.
func (c *Controller) restoreCheck(ctx context.Context, actor Actor, req Request) (RestorePlan, error) {
	into := req.RestoreInto != ""
	switch {
	case !actor.Admin:
		return RestorePlan{}, errRestoreAdmin
	case req.RestoreKey == "" || into == (len(req.Command) > 0):
		// A restore fills a new volume or feeds a command: exactly one of them is named.
		return RestorePlan{}, errRestoreArgs
	case c.Restorer == nil:
		return RestorePlan{}, errRestoreUnavailable
	}
	if !into {
		return c.restoreCommandCheck(ctx, req)
	}
	plan, err := c.Restorer.CheckRestore(ctx, req.RestoreKey, req.RestoreInto)
	if err != nil {
		return RestorePlan{}, restoreRefusal(err, errRestoreUnavailable)
	}
	return plan, nil
}

// restoreCommandCheck is the check of a restore into a command: the command's target is one the app can run, and the restorer
// accepts the backup.
func (c *Controller) restoreCommandCheck(ctx context.Context, req Request) (RestorePlan, error) {
	_, target, err := c.restoreJob(ctx, req)
	if err != nil {
		return RestorePlan{}, err
	}
	plan, err := c.Restorer.CheckRestoreCommand(ctx, req.RestoreKey)
	if err != nil {
		return RestorePlan{}, restoreRefusal(err, errRestoreUnavailable)
	}
	plan.Target, plan.Command = target, req.Command
	return plan, nil
}

// restoreJob is the job a restore into a command runs, and where it runs, as a person reads it: "db service postgres" or "web
// role web". The app must be deployed and able to run the command where it is named, and a Compose app must have a running
// container to run it in. It reads state only.
func (c *Controller) restoreJob(ctx context.Context, req Request) (manifest.Job, string, error) {
	switch {
	case req.App == "":
		return manifest.Job{}, "", errRestoreArgs
	case req.Role != "" && req.Service != "":
		return manifest.Job{}, "", errRestoreRoleOrService
	case req.Command[0] == "":
		return manifest.Job{}, "", errRunCommand
	}
	if _, err := manifest.ParseCommandTimeout(req.Timeout); err != nil {
		return manifest.Job{}, "", errRunTimeout
	}
	if c.Store == nil {
		return manifest.Job{}, "", errors.New("state unavailable")
	}
	release, err := c.Store.Applied(ctx, req.App)
	if errors.Is(err, sql.ErrNoRows) {
		return manifest.Job{}, "", errRunNotDeployed
	}
	if err != nil {
		return manifest.Job{}, "", errors.New("applied release unavailable")
	}
	var m manifest.Manifest
	if json.Unmarshal(release.Manifest, &m) != nil {
		return manifest.Job{}, "", errors.New("applied manifest unavailable")
	}
	app, ok := m.Apps[req.App]
	if !ok {
		return manifest.Job{}, "", errRunNotDeployed
	}
	if msg := m.CheckCommandTarget(req.App, req.Role, req.Service); msg != "" {
		return manifest.Job{}, "", Refusal(msg)
	}
	if e, ok := c.Engine.(composeJobEngine); ok && app.Kind == "compose" {
		containers, err := e.ComposeContainers(ctx, m.ComposeProject(c.Environment, req.App))
		if err != nil {
			return manifest.Job{}, "", errors.New("containers unavailable")
		}
		_, err = jobs.PickComposeContainer(containers, req.Service)
		switch {
		case errors.Is(err, jobs.ErrNoRunningContainer):
			return manifest.Job{}, "", Refusal(noRunningContainer(req.App, req.Service))
		case errors.Is(err, jobs.ErrSeveralServices):
			return manifest.Job{}, "", Refusal("name the Compose service as service")
		case err != nil:
			return manifest.Job{}, "", errors.New("containers unavailable")
		}
	}
	target := req.App
	switch {
	case req.Service != "":
		target += " service " + req.Service
	case req.Role != "":
		target += " role " + req.Role
	}
	return manifest.Job{App: req.App, Role: req.Role, Service: req.Service, Command: req.Command, Timeout: req.Timeout}, target, nil
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
func (c *Controller) runRestore(ctx context.Context, op state.Operation, actor Actor, req Request) (outcome state.Outcome) {
	step := func(text string) {
		if data, err := json.Marshal(map[string]string{"step": text}); err == nil {
			_ = c.Store.AppendEvent(ctx, op.ID, state.Event{Kind: "step", Data: data})
		}
	}
	if _, err := c.restoreCheck(ctx, actor, req); err != nil {
		return state.Outcome{State: "failed", Err: string(restoreRefusal(err, errRestoreFailed))}
	}
	removed := false
	if req.RestoreInto != "" && c.Reserve != nil {
		claims, err := c.reserveNames(ctx, req)
		if err != nil {
			return failedWith(err)
		}
		defer func() {
			settleCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if removed {
				c.finishReservation(settleCtx, claims)
			} else {
				c.dropReservation(settleCtx, claims)
			}
		}()
	}
	exec := len(req.Command) > 0
	desired, err := json.Marshal(struct {
		Key  string `json:"key"`
		Into string `json:"into"`
		Exec bool   `json:"exec,omitempty"`
	}{req.RestoreKey, req.RestoreInto, exec})
	if err != nil {
		return state.Outcome{State: "failed", Err: string(errRestoreJournal)}
	}
	name := req.RestoreInto
	if exec {
		name = req.App
	}
	if err = c.Store.RecordIntent(ctx, op.ID, state.Intent{Ordinal: 1, Kind: restoreKind, Name: name, Desired: desired}); err != nil {
		return state.Outcome{State: "failed", Err: string(errRestoreJournal)}
	}
	if err = c.Store.MarkStep(ctx, op.ID, 1, "issued", json.RawMessage(`{}`)); err != nil {
		return state.Outcome{State: "failed", Err: string(errRestoreJournal)}
	}
	if exec {
		return c.runRestoreCommand(ctx, op, req, step)
	}
	step("restoring " + req.RestoreKey + " into volume " + req.RestoreInto)
	err = c.Restorer.Restore(ctx, op.ID, req.RestoreKey, req.RestoreInto)
	removed = errors.Is(err, backup.ErrRestoreVolumeRemoved)
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

// restoreStderrSteps is how many of the last lines of a failed restore command are reported as steps.
const restoreStderrSteps = 20

// runRestoreCommand feeds the backup to the command. The restorer checks the backup's digest before the command starts, so a
// backup that is not what was stored never reaches it. A command that fails is reported with its exit code and the last lines of
// its output, with the secrets in them replaced.
func (c *Controller) runRestoreCommand(ctx context.Context, op state.Operation, req Request, step func(string)) state.Outcome {
	job, target, err := c.restoreJob(ctx, req)
	if err != nil {
		return state.Outcome{State: "failed", Err: string(restoreRefusal(err, errRestoreFailed))}
	}
	step("restoring " + req.RestoreKey + " into the command on " + target)
	_, err = c.Restorer.RestoreCommand(ctx, op.ID, req.RestoreKey, job)
	if err != nil {
		_ = c.Store.MarkStep(ctx, op.ID, 1, "finished", json.RawMessage(`{"ok":false}`))
		var failure *backup.CommandError
		if !errors.As(err, &failure) {
			return state.Outcome{State: "failed", Err: string(restoreRefusal(err, errRestoreFailed))}
		}
		for _, line := range lastOutputLines(failure.Outcome.Output, restoreStderrSteps) {
			step(line)
		}
		return state.Outcome{State: "failed", Err: string(errRestoreCommandFailed), Reason: commandReason(failure)}
	}
	if err = c.Store.MarkStep(ctx, op.ID, 1, "finished", json.RawMessage(`{"ok":true}`)); err != nil {
		return state.Outcome{State: "failed", Err: string(errRestoreJournal)}
	}
	step("restored " + req.RestoreKey + " into the command on " + target)
	return state.Outcome{State: "succeeded"}
}

// lastOutputLines is the last n non-empty lines of s.
func lastOutputLines(s string, n int) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimRight(line, "\r"); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// maxCommandReason bounds the reason a failed command gives, which the API shows only as 300 bytes of printable ASCII.
const maxCommandReason = 280

// commandReason is the one-line reason a command did not do its work: how it ended and the last line of what it said, which
// holds the cause. A character that is not printable ASCII is shown as a question mark.
func commandReason(f *backup.CommandError) string {
	var why string
	switch {
	case f.Outcome.Exit != nil || f.Outcome.TimedOut:
		why = "command " + f.Why()
	default:
		why = f.Outcome.Detail
	}
	if lines := lastOutputLines(f.Outcome.Output, 1); len(lines) == 1 && (f.Outcome.Exit != nil || f.Outcome.TimedOut) {
		why += ": " + lines[0]
	}
	var b strings.Builder
	for len(why) > 0 {
		r, size := utf8.DecodeRuneInString(why)
		why = why[size:]
		if r < 0x20 || r > 0x7e {
			r = '?'
		}
		if b.Len() >= maxCommandReason {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
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
	if restoresIntoCommand(intents) {
		// No volume was made, and what the command did with the part of the backup it got cannot be told from here.
		msg = errRestoreExecInterrupted
	} else if c.cleanRestore(ctx, op, intents) {
		msg = errRestoreCleaned
	}
	return c.finish(ctx, op, state.Outcome{State: "interrupted", Err: string(msg)}, target{})
}

// restoresIntoCommand reports whether the journal says the restore feeds a command.
func restoresIntoCommand(intents []state.Intent) bool {
	for _, i := range intents {
		var d struct {
			Exec bool `json:"exec"`
		}
		if i.Kind == restoreKind && json.Unmarshal(i.Desired, &d) == nil && d.Exec {
			return true
		}
	}
	return false
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
		if c.id().Owns(ctr.Labels) && ctr.Labels["berthy.helper"] == "restore" {
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
	if recovering, _ := ctx.Value(recoveringKey{}).(bool); recovering {
		return false
	}
	claim := state.Claim{Kind: state.ClaimVolume, Name: into}
	if c.Reserve != nil {
		reserved, err := c.reserveClaims(ctx, []state.Claim{claim})
		if err != nil {
			return false
		}
		reserved.added = reserved.leased
		defer c.finishReservation(ctx, reserved)
	} else if err := c.notForeign(ctx, claim); err != nil {
		return false
	}
	if err = de.RemoveVolume(ctx, into); err != nil && !statusIs(err, 404) {
		return false
	}
	_, exists, err = de.VolumeLabels(ctx, into)
	if err != nil || exists {
		return false
	}
	return c.Reserve != nil || c.Store == nil || c.Store.ReleaseClaims(ctx, []state.Claim{claim}) == nil
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
