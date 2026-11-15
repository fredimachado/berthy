package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"berthy/internal/engine"
	"berthy/internal/state"
)

type roleWork struct {
	intent   state.Intent
	replicas int
}

func (c *Controller) execute(ctx context.Context, op state.Operation, actor Actor, req Request) {
	outcome := state.Outcome{State: "failed", Err: "deployment preflight failed"}
	defer func() { _ = c.Store.Finish(context.Background(), op.ID, outcome) }()
	p, err := c.Plan(ctx, actor, req)
	if err != nil {
		return
	}
	if req.Adopt {
		outcome.Err = "adoption unavailable"
		return
	}
	prepared, err := c.prepareExecution(ctx, req, p)
	if err != nil {
		return
	}
	pullCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err = c.Engine.PullImage(pullCtx, prepared.Image, prepared.Auth)
	cancel()
	if err != nil {
		outcome.Err = "image pull failed"
		return
	}
	img, err := c.Engine.InspectImage(ctx, prepared.Image)
	if err != nil {
		outcome.Err = "image inspection failed"
		return
	}
	bindings, err := c.Store.Bindings(ctx, req.App)
	if err != nil || len(bindings) > 0 {
		outcome.Err = "updates of existing services unavailable"
		return
	}
	app := req.Manifest.Apps[req.App]
	drain := app.Deploy.DrainTimeout
	if drain == "" {
		drain = "30s"
	}
	timeout := app.Deploy.DeployTimeout
	if timeout == "" {
		timeout = "120s"
	}
	budget, err := time.ParseDuration(timeout)
	if err != nil || budget <= 0 {
		return
	}
	work := make([]roleWork, 0, len(p.Roles))
	for ordinal, role := range p.Roles {
		r := app.Roles[role]
		spec, e := serviceSpec(c.Environment, req.Manifest.Project, req.App, role, op.ID, prepared.NetworkID, prepared.Image, prepared.Env, r, drain, img.Healthcheck)
		if e != nil {
			outcome.Err = "role preflight failed"
			return
		}
		name := req.Manifest.Project + "-" + req.App + "-" + role
		_, e = c.Engine.InspectService(ctx, name)
		var status *engine.StatusError
		if e == nil || !errors.As(e, &status) || status.Code != 404 {
			outcome.Err = "service name is unavailable"
			return
		}
		replicas := 1
		if r.Replicas != nil {
			replicas = *r.Replicas
		}
		work = append(work, roleWork{intent: state.Intent{Ordinal: ordinal, Kind: "create", Role: role, Name: name, Generation: op.ID, Desired: spec}, replicas: replicas})
	}
	before, after, err := c.runPre(ctx, op.ID, prepared)
	outcome.SchemaBefore = before
	outcome.SchemaAfter = after
	if err != nil {
		if errors.Is(err, ErrOneOffUncertain) {
			outcome.State = "recovery_required"
			outcome.Err = "one-off outcome uncertain"
		} else {
			outcome.Err = "pre-deploy command failed"
		}
		return
	}
	deadline := time.Now().Add(budget)
	rollCtx, stop := context.WithDeadline(ctx, deadline)
	defer stop()
	var releaseBindings []state.Binding
	for _, w := range work {
		w.intent.Deadline = deadline
		id, e := createRole(rollCtx, c.Store, c.Engine, op.ID, w.intent, prepared.Auth, w.replicas)
		if e != nil {
			recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			recoveryErr := c.compensateCreates(recoveryCtx, op.ID)
			recoveryCancel()
			if recoveryErr != nil {
				outcome.State = "recovery_required"
				outcome.Err = "rollout recovery required"
			} else {
				outcome.State = "failed"
				outcome.Err = "rollout failed and was cleaned up"
			}
			return
		}
		releaseBindings = append(releaseBindings, state.Binding{App: req.App, Role: w.intent.Role, Name: w.intent.Name, ServiceID: id})
	}
	manifestJSON, err := json.Marshal(req.Manifest)
	if err != nil {
		outcome.State = "recovery_required"
		outcome.Err = "release recording failed"
		return
	}
	outcome = state.Outcome{State: "succeeded", SchemaBefore: before, SchemaAfter: after, Protected: req.Manifest.Protected, Release: &state.Release{App: req.App, Digest: prepared.Image, Commit: req.Commit, Manifest: manifestJSON, Bindings: releaseBindings}}
	if err = c.runPost(ctx, op.ID, prepared); err != nil {
		if errors.Is(err, ErrOneOffUncertain) {
			outcome.State = "recovery_required"
			outcome.Release = nil
			outcome.Err = "post-deploy command outcome uncertain"
		} else {
			outcome.Err = "post-deploy command failed"
		}
	}
}

func (c *Controller) compensateCreates(ctx context.Context, opID string) error {
	intents, err := c.Store.Intents(ctx, opID)
	if err != nil {
		return err
	}
	for n := len(intents) - 1; n >= 0; n-- {
		i := intents[n]
		if i.Kind != "create" {
			continue
		}
		live, e := c.Engine.InspectService(ctx, i.Name)
		if e != nil {
			var status *engine.StatusError
			if errors.As(e, &status) && status.Code == 404 {
				continue
			}
			return errors.New("recovery inspection failed")
		}
		var receipt struct {
			ID   string          `json:"id"`
			Spec json.RawMessage `json:"spec"`
		}
		if json.Unmarshal(i.Receipt, &receipt) != nil || receipt.ID == "" || live.ID != receipt.ID || !sameSpec(live.Spec, receipt.Spec) {
			return errors.New("service ownership uncertain")
		}
		if e = c.Engine.DeleteService(ctx, live.ID); e != nil {
			return errors.New("service cleanup failed")
		}
		_, e = c.Engine.InspectService(ctx, i.Name)
		var status *engine.StatusError
		if !errors.As(e, &status) || status.Code != 404 {
			return errors.New("service cleanup unconfirmed")
		}
	}
	return nil
}
