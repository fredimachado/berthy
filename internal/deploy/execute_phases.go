package deploy

import (
	"context"
	"errors"
	"time"

	"berthy/internal/state"
)

type deployRun struct {
	opID     string
	prepared preparedPlan
	work     []roleWork
	outcome  state.Outcome
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
