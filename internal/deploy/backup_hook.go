package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"berthy/internal/backup"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// BackupResult is the outcome of a backup that ran for a deploy hook.
type BackupResult struct {
	RunID  int64
	Status string // succeeded or failed
	// Detail says why a run failed: the step that failed and a short cause, such as "upload failed: access denied".
	Detail string
}

// BackupRunner runs a volume backup for a `backup:` hook and waits for it. The agent supplies it. It runs the backup of the
// manifest it is given, with the backup target taken from its applied release. Its errors are backup.Refusal values when the backup could not
// start, and then no run was made.
type BackupRunner interface {
	RunBackup(ctx context.Context, m manifest.Manifest, name string) (BackupResult, error)
}

// backupStages are the steps a failed backup names first in its detail. The reason of a deploy shows only that step.
var backupStages = []string{"preparation failed", "archive failed", "upload failed", "sidecar failed"}

// backupCategory is the part of a backup's failure detail that a deploy's public reason shows: the step that failed, or for a
// backup that could not start, the refusal.
func backupCategory(detail string) string {
	for _, stage := range backupStages {
		if strings.HasPrefix(detail, stage) {
			return stage
		}
	}
	return printable(detail, 100)
}

// backupCause says what went wrong with a backup hook, for the reason of a failed deploy. It never includes more than the
// category: the run's own detail is in the sealed diagnostics.
func backupCause(d Diagnostic) string {
	when := "pre-deploy"
	if d.Role == "post_hook" {
		when = "post-deploy"
	}
	return fmt.Sprintf("%s backup %s failed: %s", when, printable(d.Name, 64), backupCategory(d.Error))
}

// hookFailureText is the public error of an operation whose hook failed: when is "pre" or "post".
func hookFailureText(err error, when string) string {
	var f *oneOffFailure
	if errors.As(err, &f) && f.diagnostic.Kind == "backup" {
		return when + "-deploy backup failed"
	}
	return when + "-deploy command failed"
}

// runBackupHook runs the backup of a `backup:` hook and waits for it. kind is "pre_hook" or "post_hook" and index is the
// hook's 1-based position in its list. A backup that does not succeed is a failure that carries a diagnostic of kind
// "backup" with the run's detail. A backup whose app is not deployed is skipped, and the step says so.
func (c *Controller) runBackupHook(ctx context.Context, opID string, p preparedPlan, kind string, index int, name string) error {
	step := func(text string) {
		if data, err := json.Marshal(map[string]string{"step": text}); err == nil {
			_ = c.Store.AppendEvent(ctx, opID, state.Event{Kind: "step", Data: data})
		}
	}
	fail := func(detail string) error {
		return &oneOffFailure{err: fmt.Errorf("%s failed: backup %s", kind, name), diagnostic: Diagnostic{Kind: "backup", Name: name, Role: kind, Index: index, Error: detail, Captured: time.Now().UTC()}}
	}
	b, known := p.Request.Manifest.Backups[name]
	if !known {
		return fail("unknown backup")
	}
	if c.Backups == nil {
		return fail("backups unavailable")
	}
	release, err := c.Store.Applied(ctx, b.App)
	if errors.Is(err, sql.ErrNoRows) {
		step("backup " + name + " skipped: app " + b.App + " is not deployed")
		return nil
	}
	if err != nil {
		return fail("state unavailable")
	}
	var applied manifest.Manifest
	if json.Unmarshal(release.Manifest, &applied) != nil {
		return fail("applied manifest unavailable")
	}
	target, ok := applied.Apps[b.App]
	if !ok {
		return fail("applied app unavailable")
	}
	claims, err := c.Store.Claims(ctx)
	if err != nil {
		return fail("state unavailable")
	}
	owns := func(kind, name string) bool {
		return slices.Contains(claims, state.Claim{Kind: kind, Name: name})
	}
	if target.Kind == "compose" {
		project := applied.ComposeProject(c.Environment, b.App)
		if !owns(state.ClaimComposeProject, project) {
			return fail("backup target is not owned by this environment")
		}
		if len(b.Command) == 0 {
			volumes, err := c.ComposeVolumeNames(ctx, project)
			if err != nil || volumes[b.Volume] == "" || !owns(state.ClaimVolume, volumes[b.Volume]) {
				return fail("backup target is not owned by this environment")
			}
		}
	} else if target.Kind == "image" && len(b.Command) == 0 {
		mounted := false
		for _, role := range target.Roles {
			for _, raw := range role.Volumes {
				mount, err := manifest.ParseMount(raw)
				if err == nil && mount.Source == b.Volume && owns(state.ClaimVolume, applied.VolumeName(c.Environment, mount.Source)) {
					mounted = true
				}
			}
		}
		if !mounted {
			return fail("backup target is not owned by this environment")
		}
	} else if target.Kind != "image" {
		return fail("applied app unavailable")
	}
	backupManifest := p.Request.Manifest
	backupManifest.Project = applied.Project
	backupManifest.Apps = maps.Clone(p.Request.Manifest.Apps)
	backupManifest.Apps[b.App] = target
	if err = cancelledBy(ctx); err != nil {
		return err
	}
	step("backup " + name + " started")
	runCtx, stop := interruptible(ctx)
	res, err := c.Backups.RunBackup(runCtx, backupManifest, name)
	stop()
	if cancel := cancelledBy(ctx); cancel != nil {
		return cancel
	}
	var refusal backup.Refusal
	switch {
	case errors.As(err, &refusal):
		return fail(refusal.Error())
	case err != nil:
		return fail("backup could not run")
	case res.Status != "succeeded":
		if res.Detail == "" {
			res.Detail = "backup failed"
		}
		return fail(res.Detail)
	}
	step(fmt.Sprintf("backup %s succeeded (run %d)", name, res.RunID))
	return nil
}
