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
	rollback := req.Kind == "rollback"
	if rollback {
		resolved, err := c.rollbackRequest(ctx, req)
		if err != nil {
			_ = c.Store.Finish(ctx, op.ID, state.Outcome{State: "failed", Err: "rollback release unavailable"})
			return
		}
		req = resolved
		req.Kind = "deploy"
	}
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
	img, err := c.Engine.InspectImage(ctx, prepared.Image)
	if err != nil {
		var status *engine.StatusError
		if !errors.As(err, &status) || status.Code != 404 {
			outcome.Err = "image inspection failed"
			return
		}
		pullCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err = c.Engine.PullImage(pullCtx, prepared.Image, prepared.Auth)
		cancel()
		if err != nil {
			outcome.Err = "image pull failed"
			return
		}
		img, err = c.Engine.InspectImage(ctx, prepared.Image)
		if err != nil || img.ID == "" {
			outcome.Err = "image inspection failed"
			return
		}
		if err = c.Store.RecordImageAcquired(ctx, prepared.Image, img.ID); err != nil {
			outcome.Err = "image provenance recording failed"
			return
		}
	}
	bindings, err := c.Store.Bindings(ctx, req.App)
	if err != nil {
		return
	}
	byRole := map[string]state.Binding{}
	for _, b := range bindings {
		byRole[b.Role] = b
	}
	for role := range byRole {
		if _, ok := req.Manifest.Apps[req.App].Roles[role]; !ok {
			outcome.Err = "removing managed roles unavailable"
			return
		}
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
		intent := state.Intent{Ordinal: ordinal, Kind: "create", Role: role, Name: name, Generation: op.ID, Desired: spec}
		if b, ok := byRole[role]; ok {
			name = b.Name
			live, e := c.Engine.InspectService(ctx, name)
			if e != nil || live.ID != b.ServiceID {
				outcome.Err = "bound service unavailable"
				return
			}
			var ownership struct {
				Labels       map[string]string
				TaskTemplate struct {
					ContainerSpec struct{ Labels map[string]string }
				}
			}
			if json.Unmarshal(live.Spec, &ownership) != nil {
				outcome.Err = "invalid bound service"
				return
			}
			for _, labels := range []map[string]string{ownership.Labels, ownership.TaskTemplate.ContainerSpec.Labels} {
				for key, value := range map[string]string{"berthy.environment": c.Environment, "berthy.project": req.Manifest.Project, "berthy.app": req.App, "berthy.role": role} {
					if existing, ok := labels[key]; ok && existing != value {
						outcome.Err = "bound service ownership changed"
						return
					}
				}
			}
			var previous struct {
				TaskTemplate struct {
					ContainerSpec struct{ Labels map[string]string }
				}
				Mode struct{ Replicated *struct{ Replicas int } }
			}
			if json.Unmarshal(live.Spec, &previous) != nil || previous.Mode.Replicated == nil || previous.TaskTemplate.ContainerSpec.Labels["berthy.operation"] == "" {
				outcome.Err = "prior service health cannot be verified"
				return
			}
			if _, e = c.operationAuth(ctx, req.App, live.Spec); e != nil {
				outcome.Err = "prior image credential unavailable"
				return
			}
			// A bound adopted role may keep a non-deterministic name.
			var desired map[string]any
			if json.Unmarshal(spec, &desired) != nil {
				return
			}
			desired["Name"] = name
			spec, _ = json.Marshal(desired)
			spec, e = mergeServiceSpec(live.Spec, spec, prepared.NetworkID)
			if e != nil {
				outcome.Err = "unsafe or incompatible service"
				return
			}
			intent = state.Intent{Ordinal: ordinal, Kind: "update", Role: role, Name: name, ServiceID: live.ID, SeenVersion: live.Version, Generation: op.ID, Before: live.Spec, Desired: spec}
		} else {
			_, e = c.Engine.InspectService(ctx, name)
			var status *engine.StatusError
			if e == nil || !errors.As(e, &status) || status.Code != 404 {
				outcome.Err = "service name is unavailable"
				return
			}
		}
		replicas := 1
		if r.Replicas != nil {
			replicas = *r.Replicas
		}
		work = append(work, roleWork{intent: intent, replicas: replicas})
	}
	var before, after string
	if !rollback {
		before, after, err = c.runPre(ctx, op.ID, prepared)
	}
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
		id := w.intent.ServiceID
		var e error
		if w.intent.Kind == "create" {
			id, e = createRole(rollCtx, c.Store, c.Engine, op.ID, w.intent, prepared.Auth, w.replicas)
		} else {
			e = updateRole(rollCtx, c.Store, c.Engine, op.ID, w.intent, prepared.Auth, w.replicas)
		}
		if e != nil {
			recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			recoveryErr := c.compensateRollout(recoveryCtx, op.ID, req.App)
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
	if !rollback {
		err = c.runPost(ctx, op.ID, prepared)
	} else {
		err = nil
	}
	if err != nil {
		if errors.Is(err, ErrOneOffUncertain) {
			outcome.State = "recovery_required"
			outcome.Release = nil
			outcome.Err = "post-deploy command outcome uncertain"
		} else {
			outcome.Err = "post-deploy command failed"
		}
	}
}

func (c *Controller) compensateRollout(ctx context.Context, opID, app string) error {
	intents, err := c.Store.Intents(ctx, opID)
	if err != nil {
		return err
	}
	for n := len(intents) - 1; n >= 0; n-- {
		i := intents[n]
		if i.Kind == "update" {
			if err = c.restoreUpdate(ctx, app, i); err != nil {
				return err
			}
			continue
		}
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

func (c *Controller) restoreUpdate(ctx context.Context, app string, i state.Intent) error {
	live, err := c.Engine.InspectService(ctx, i.Name)
	if err != nil || live.ID != i.ServiceID {
		return errors.New("bound service recovery inspection failed")
	}
	if !sameSpec(live.Spec, i.Before) {
		if !sameSpec(live.Spec, i.Desired) {
			return errors.New("service changed outside operation")
		}
		auth, err := c.operationAuth(ctx, app, i.Before)
		if err != nil {
			return err
		}
		if err = c.Engine.UpdateService(ctx, live.ID, live.Version, i.Before, auth); err != nil {
			return errors.New("service restoration uncertain")
		}
		live, err = c.Engine.InspectService(ctx, i.Name)
		if err != nil || live.ID != i.ServiceID || !sameSpec(live.Spec, i.Before) {
			return errors.New("service restoration unconfirmed")
		}
	}
	var before struct {
		TaskTemplate struct {
			ContainerSpec struct{ Labels map[string]string }
		}
		Mode struct{ Replicated *struct{ Replicas int } }
	}
	if json.Unmarshal(i.Before, &before) != nil || before.Mode.Replicated == nil {
		return errors.New("prior service health unavailable")
	}
	generation := before.TaskTemplate.ContainerSpec.Labels["berthy.operation"]
	if generation == "" {
		return errors.New("prior service generation unavailable")
	}
	return waitHealthy(ctx, c.Engine, i.ServiceID, generation, before.Mode.Replicated.Replicas)
}
