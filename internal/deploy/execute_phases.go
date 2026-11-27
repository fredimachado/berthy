package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"berthy/internal/state"
)

type deployRun struct {
	opID     string
	rollback bool
	prepared preparedPlan
	work     []roleWork
	ing      *ingressPublish
	budget   time.Duration
	deadline time.Time
	bindings []state.Binding
	outcome  state.Outcome
}

func (c *Controller) rollDeployRoles(ctx context.Context, d *deployRun) bool {
	req := d.prepared.Request
	app := req.Manifest.Apps[req.App]
	attempts := app.Deploy.StartAttemptLimit()
	hold := c.monitorHold()
	startupDeadline := time.Now().Add(d.budget)
	for n, w := range d.work {
		if !time.Now().Before(startupDeadline) {
			c.unwindDeploy(ctx, d, errors.New("service health deadline exceeded"), d.work[:n])
			return false
		}
		w.intent.Deadline = startupDeadline.Add(rolloutAllowance(hold))
		roleCtx, stopRole := context.WithDeadline(ctx, w.intent.Deadline)
		rollCtx, stopRoll := interruptible(roleCtx)
		id := w.intent.ServiceID
		health := app.Roles[w.intent.Role].Health
		starts := startBudget{limit: attempts, countComplete: health != nil && health.Uptime != nil}
		var e error
		if w.intent.Kind == "create" {
			id, e = createRole(rollCtx, c.Store, c.Engine, d.opID, w.intent, d.prepared.Auth, w.replicas, starts, hold)
		} else {
			e = updateRole(rollCtx, c.Store, c.Engine, d.opID, w.intent, d.prepared.Auth, w.replicas, starts, hold)
		}
		stopRoll()
		stopRole()
		if e != nil {
			touched := n + 1
			var before beforeServiceMutation
			if errors.As(e, &before) {
				touched = n
			}
			c.unwindDeploy(ctx, d, e, d.work[:touched])
			return false
		}
		startupDeadline = startupDeadline.Add(hold)
		d.bindings = append(d.bindings, state.Binding{App: req.App, Role: w.intent.Role, Name: w.intent.Name, ServiceID: id})
	}
	deadline := startupDeadline
	if hold > 0 {
		if now := time.Now(); now.After(deadline) {
			deadline = now
		}
		deadline = deadline.Add(rolloutVerificationGrace)
	}
	d.deadline = deadline
	return true
}

// unwindDeploy takes the rollout back after a failure or a cancel, with the roles in touched changed.
func (c *Controller) unwindDeploy(ctx context.Context, d *deployRun, cause error, touched []roleWork) {
	req := d.prepared.Request
	app := req.Manifest.Apps[req.App]
	// The evidence is read first: compensation deletes a created service together with its tasks and their logs.
	var gaveUp *errStartAttempts
	errors.As(cause, &gaveUp)
	d.outcome.Reason = addStoppedFirst(c.recordRolloutFailure(ctx, d.opID, d.work, rolloutFallback(cause), gaveUp), app, touched)
	allowance := serviceCleanupAllowance
	if len(touched) > 0 {
		allowance = touched[len(touched)-1].recoveryAllowance
	}
	recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), allowance)
	recoveryErr := c.compensateRollout(recoveryCtx, d.opID, req.App)
	recoveryCancel()
	if recoveryErr != nil {
		for _, message := range compensationMessages(recoveryErr) {
			if c.Log != nil {
				c.Log(message)
			}
			c.storeDiagnostics(context.Background(), d.opID, []Diagnostic{{Kind: "compensation", Error: message, Captured: time.Now().UTC()}}, "")
		}
		d.outcome.State = "recovery_required"
		d.outcome.Err = "rollout recovery required"
		return
	}
	d.outcome.State = "failed"
	for _, done := range touched {
		if done.intent.Kind == "update" {
			d.outcome.State = "rolled_back"
			break
		}
	}
	d.outcome.Err = "rollout failed and was cleaned up"
}

func (c *Controller) verifyDeployRoles(ctx, verifyCtx context.Context, d *deployRun) bool {
	unavailable := state.Outcome{
		State: "recovery_required", Err: "verification unavailable",
		SchemaBefore: d.outcome.SchemaBefore, SchemaAfter: d.outcome.SchemaAfter,
	}
	steps, e := c.Store.Intents(verifyCtx, d.opID)
	if e != nil {
		d.outcome = unavailable
		return false
	}
	seen := make(map[int]bool)
	for _, i := range steps {
		if i.Kind != "create" && i.Kind != "update" {
			continue
		}
		if i.Ordinal < 0 || i.Ordinal >= len(d.work) || seen[i.Ordinal] {
			d.outcome = unavailable
			return false
		}
		expected := d.work[i.Ordinal].intent
		if i.Kind != expected.Kind || i.Role != expected.Role || i.Name != expected.Name || i.Generation != expected.Generation || i.ServiceID != expected.ServiceID {
			d.outcome = unavailable
			return false
		}
		seen[i.Ordinal] = true
	}
	if len(seen) != len(d.work) {
		d.outcome = unavailable
		return false
	}
	if e = c.verifyRollout(verifyCtx, steps); e != nil {
		var failed finalRolloutFailure
		if errors.As(e, &failed) {
			c.unwindDeploy(ctx, d, e, d.work)
			return false
		}
		d.outcome = unavailable
		return false
	}
	return true
}

func (c *Controller) crossDeployBarrier(ctx, verifyCtx context.Context, d *deployRun, tag string) bool {
	req := d.prepared.Request
	before, after := d.outcome.SchemaBefore, d.outcome.SchemaAfter
	recordingFailure := state.Outcome{
		State: "recovery_required", Err: "release recording failed",
		SchemaBefore: before, SchemaAfter: after,
	}
	manifestJSON, err := json.Marshal(req.Manifest)
	if err != nil {
		d.outcome = recordingFailure
		return false
	}
	settings, err := releaseSettings(req, d.rollback)
	if err != nil {
		d.outcome = recordingFailure
		return false
	}
	candidate := state.Outcome{
		State: "succeeded", SchemaBefore: before, SchemaAfter: after, Protected: req.Manifest.Protected,
		Release: &state.Release{
			App: req.App, Digest: d.prepared.Image, Tag: tag, Commit: req.Commit, Modified: req.Modified,
			Manifest: manifestJSON, Bindings: d.bindings,
		},
		Settings: settings,
	}
	barrier, err := json.Marshal(candidate)
	if err != nil {
		d.outcome = recordingFailure
		return false
	}
	// Recording the barrier is the point of no return of a deploy: the release is verified, and the routes and the post-deploy
	// commands follow. A cancel that was accepted before it takes the rollout back instead.
	if err = c.cross(verifyCtx, d.opID, state.Intent{Ordinal: 9999, Kind: "barrier", Desired: barrier, Deadline: d.deadline}, reasonSwitched); err != nil {
		if isCancelled(err) {
			d.outcome = state.Outcome{SchemaBefore: before, SchemaAfter: after}
			c.unwindDeploy(ctx, d, err, d.work)
			return false
		}
		d.outcome = state.Outcome{State: "recovery_required", Err: "verification barrier unavailable", SchemaBefore: before, SchemaAfter: after}
		return false
	}
	d.outcome = candidate
	return true
}

func (c *Controller) finishDeployRelease(ctx context.Context, d *deployRun) {
	req := d.prepared.Request
	// The publish follows the barrier. A crash during it leaves the operation to recovery, which keeps the release. The
	// next deploy or ingress apply brings any route or record it missed in line.
	publishErr := c.publishIngress(ctx, d.opID, d.ing, req.ReplaceExisting)
	var err error
	if !d.rollback {
		err = c.runPost(ctx, d.opID, d.prepared)
	}
	switch {
	case err != nil && errors.Is(err, ErrOneOffUncertain):
		d.outcome.State = "recovery_required"
		d.outcome.Release = nil
		d.outcome.Err = "post-deploy command outcome uncertain"
		d.outcome.Reason = c.recordOneOffFailure(ctx, d.opID, err)
	case err != nil:
		d.outcome.State = "failed"
		d.outcome.Err = hookFailureText(err, "post")
		d.outcome.Reason = c.recordOneOffFailure(ctx, d.opID, err)
	case publishErr != nil:
		// The new version runs and its release is recorded. Only its routes are missing.
		d.outcome.State = "failed"
		d.outcome.Err = ingressFailure(publishErr, errIngressPublish)
	}
	if d.outcome.State == "succeeded" {
		// The new version serves and its release is recorded. What the checks find is a warning, never a failure.
		app := req.Manifest.Apps[req.App]
		c.verifyDeploy(ctx, d.opID, req.Manifest.NetworkName(c.Environment), app, d.work, d.ing != nil)
	}
}
