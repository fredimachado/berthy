package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
// manifest it is given, which is the manifest being deployed. Its errors are backup.Refusal values when the backup could not
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

// backupAppDeployed reports whether the app of a backup has an applied release. A backup of an app that is not deployed has
// no volume to read yet, as on the first deploy of an app, and is not part of the configuration.
func (c *Controller) backupAppDeployed(ctx context.Context, app string) (bool, error) {
	_, err := c.Store.Applied(ctx, app)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
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
	deployed, err := c.backupAppDeployed(ctx, b.App)
	if err != nil {
		return fail("state unavailable")
	}
	if !deployed {
		step("backup " + name + " skipped: app " + b.App + " is not deployed")
		return nil
	}
	step("backup " + name + " started")
	res, err := c.Backups.RunBackup(ctx, p.Request.Manifest, name)
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
