package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
	"github.com/distribution/reference"
)

type roleWork struct {
	intent   state.Intent
	replicas int
}

func (c *Controller) execute(ctx context.Context, op state.Operation, actor Actor, req Request) {
	var outcome state.Outcome
	var t target
	switch {
	case req.Kind == "rollback" && req.RevertSchema:
		// A schema revert is a migration from start to end, so a cancel is refused for all of it.
		if _, err := c.enter(ctx, reasonRevert); err != nil {
			outcome = state.Outcome{State: "failed", Err: "deployment preflight failed"}
			break
		}
		outcome = c.runSchemaRevert(ctx, op, actor, req)
	case req.Kind == "rollback":
		resolved, err := c.rollbackRequest(ctx, req)
		if err != nil {
			outcome = state.Outcome{State: "failed", Err: "rollback release unavailable"}
			break
		}
		resolved.Kind = "deploy"
		// The alert settings are the environment's, which a rollback never changes, so its manifest does not decide them.
		outcome, t = c.runDeploy(ctx, op, actor, resolved, true)
		if t.Tag == "" {
			t.Tag = resolved.recordTag
		}
	default:
		t.Manifest = &req.Manifest
		resolved, err := c.resolveKeepVersion(ctx, req)
		if err != nil {
			// A public refusal says what to do, as a version that changed since the client read it. Anything else may carry
			// internals.
			outcome = state.Outcome{State: "failed", Err: "deployment preflight failed"}
			var refusal Refusal
			if errors.As(err, &refusal) && PublicMessage(string(refusal)) {
				outcome.Err = string(refusal)
			}
			break
		}
		outcome, t = c.runDeploy(ctx, op, actor, resolved, false)
		t.Manifest = &resolved.Manifest
	}
	_ = c.finish(ctx, op, outcome, t)
}

// releaseRoleSpec is the Swarm service of a role at the release that prepared describes, as a deploy sends it. operation is
// the generation label of the spec. A deploy and the drift check both build a role's spec here, so what a deploy would send
// and what the check expects are the same.
func (c *Controller) releaseRoleSpec(prepared preparedPlan, img engine.Image, role, operation string) (json.RawMessage, error) {
	req := prepared.Request
	app := req.Manifest.Apps[req.App]
	drain := app.Deploy.DrainTimeout
	if drain == "" {
		drain = "30s"
	}
	order, _ := app.UpdateOrder(role)
	return serviceSpec(c.Environment, req.Manifest.Project, req.App, role, req.Manifest.ServiceName(c.Environment, req.App, role), operation, prepared.NetworkID, prepared.Image, func(v string) string { return req.Manifest.VolumeName(c.Environment, v) }, prepared.containerEnv(role), app.Roles[role], order, drain, img.Healthcheck, prepared.LogDriver)
}

// releaseSettings is the environment settings that the release of req applies: the environment scope of its manifest. A rollback
// applies none, since its manifest is an old one, and neither does the ingress connector's deploy, whose manifest Berthy
// builds and holds nothing of the environment.
func releaseSettings(req Request, rollback bool) (*state.Settings, error) {
	if rollback || req.App == manifest.ConnectorAppName {
		return nil, nil
	}
	return state.NewSettings(req.Manifest, req.Commit, req.Modified)
}

// runDeploy rolls out req and returns the outcome for the caller to record, and the image and tag it planned, when it got as
// far as a plan. A rollback runs no migration steps.
func (c *Controller) runDeploy(ctx context.Context, op state.Operation, actor Actor, req Request, rollback bool) (outcome state.Outcome, planned target) {
	outcome = state.Outcome{State: "failed", Err: "deployment preflight failed"}
	if composeRequest(req) {
		outcome = c.runCompose(ctx, op, actor, req, rollback)
		return
	}
	checkCtx, stopCheck := interruptible(ctx)
	p, err := c.plan(checkCtx, actor, req)
	stopCheck()
	if err != nil {
		// A secret the app reads but the agent does not hold is named, so the operator knows what to set.
		var refusal Refusal
		if errors.As(err, &refusal) && secretMessage.MatchString(string(refusal)) {
			outcome.Err = string(refusal)
		}
		return
	}
	planned = target{Image: p.Image, Tag: p.Tag}
	if req.Adopt {
		plan, bindings, e := c.planAdoption(ctx, req)
		if e != nil {
			outcome.Err = "adoption preflight failed"
			return
		}
		raw, e := json.Marshal(req.Manifest)
		if e != nil {
			return
		}
		ing, err := c.publishCheck(ctx, op.ID, req.Manifest, req.App, appRouteWants(req.Manifest, c.Environment, req.App, bindings), req.ReplaceExisting)
		if err != nil {
			outcome.Err = ingressFailure(err, errIngressPublish)
			return
		}
		settings, e := releaseSettings(req, rollback)
		if e != nil {
			return
		}
		outcome = state.Outcome{State: "succeeded", Protected: req.Manifest.Protected, Release: &state.Release{App: req.App, Digest: plan.Image, Commit: req.Commit, Modified: req.Modified, Manifest: raw, Bindings: bindings}, Settings: settings}
		if ing == nil {
			return
		}
		// Adoption starts no service, so its routes follow its bindings at once. The barrier keeps the release if the
		// process stops during the publish, as it does for a deploy.
		barrier, e := json.Marshal(outcome)
		if e != nil || c.cross(ctx, op.ID, state.Intent{Ordinal: 9999, Kind: "barrier", Desired: barrier, Deadline: time.Now()}, reasonSwitched) != nil {
			outcome = state.Outcome{State: "failed", Err: "verification barrier unavailable"}
			return
		}
		if err = c.publishIngress(ctx, op.ID, ing, req.ReplaceExisting); err != nil {
			outcome.State = "failed"
			outcome.Err = ingressFailure(err, errIngressPublish)
		}
		return
	}
	prepared, err := c.prepareExecution(ctx, req, p)
	if err != nil {
		return
	}
	img, msg, err := c.ensureImage(ctx, prepared)
	if err != nil {
		outcome.Err = msg
		return
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
		spec, e := c.releaseRoleSpec(prepared, img, role, op.ID)
		if e != nil {
			outcome.Err = "role preflight failed"
			return
		}
		name := req.Manifest.ServiceName(c.Environment, req.App, role)
		intent := state.Intent{Ordinal: ordinal, Kind: "create", Role: role, Name: name, Generation: op.ID, Desired: spec}
		if b, ok := byRole[role]; ok {
			name = b.Name
			// The release's spec goes to the bound service under its bound name. An adopted role keeps the name of the
			// service it adopted, which is not the generated one.
			if spec, e = renamed(spec, name); e != nil {
				return
			}
			live, e := c.Engine.InspectService(ctx, name)
			if e != nil {
				gone, ge := c.boundServiceGone(ctx, b, e)
				if ge != nil || !gone {
					outcome.Err = "bound service unavailable"
					return
				}
				// Recreate the role under its bound name; the release re-binds it to the new service.
				intent = state.Intent{Ordinal: ordinal, Kind: "create", Role: role, Name: name, Generation: op.ID, Desired: spec}
			} else {
				if live.ID != b.ServiceID {
					outcome.Err = "bound service unavailable"
					return
				}
				var ownership struct {
					Labels       map[string]string
					TaskTemplate struct {
						ContainerSpec struct {
							Labels map[string]string
							Image  string
						}
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
						ContainerSpec struct {
							Labels map[string]string
							Image  string
						}
					}
					Mode struct{ Replicated *struct{ Replicas int } }
				}
				if json.Unmarshal(live.Spec, &previous) != nil || previous.Mode.Replicated == nil {
					outcome.Err = "prior service health cannot be verified"
					return
				}
				if previous.TaskTemplate.ContainerSpec.Labels["berthy.operation"] == "" {
					ref, e := reference.ParseNormalizedNamed(previous.TaskTemplate.ContainerSpec.Image)
					if e != nil {
						outcome.Err = "prior image identity unavailable"
						return
					}
					if _, ok := ref.(reference.Canonical); !ok {
						outcome.Err = "prior image is not pinned"
						return
					}
					if healthyImageOnce(ctx, c.Engine, live.ID, previous.TaskTemplate.ContainerSpec.Image, previous.Mode.Replicated.Replicas) != nil {
						outcome.Err = "prior service health cannot be verified"
						return
					}
				}
				if _, e = c.operationAuth(ctx, req.App, live.Spec); e != nil {
					outcome.Err = "prior image credential unavailable"
					return
				}
				// The update sends the release's spec, as a create does. Nothing in it comes from the live spec: whatever the
				// live one has beyond the release is replaced.
				intent = state.Intent{Ordinal: ordinal, Kind: "update", Role: role, Name: name, ServiceID: live.ID, SeenVersion: live.Version, Generation: op.ID, Before: live.Spec, Desired: spec}
			}
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
	// The check step of a publishing deploy confirms, before anything changes, that its routes and records can be published.
	wants, err := c.routeWants(ctx, req.Manifest, []string{req.App})
	if err != nil {
		outcome.Err = ingressFailure(err, errIngressPublish)
		return
	}
	ing, err := c.publishCheck(ctx, op.ID, req.Manifest, req.App, wants, req.ReplaceExisting)
	if err != nil {
		outcome.Err = ingressFailure(err, errIngressPublish)
		return
	}
	// A cancel that came during the checks stops the deploy here, before it changes anything.
	if err = cancelledBy(ctx); err != nil {
		return
	}
	if !rollback && app.Migrations != nil {
		source, e := json.Marshal(struct {
			Manifest manifest.Manifest `json:"manifest"`
			Image    string            `json:"image"`
		}{req.Manifest, prepared.Image})
		if e != nil {
			return
		}
		if e = c.Store.RecordIntent(ctx, op.ID, state.Intent{Ordinal: 9000, Kind: "schema_source", Desired: source}); e != nil {
			outcome.Err = "migration source journal failed"
			return
		}
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
			outcome.Err = hookFailureText(err, "pre")
		}
		outcome.Reason = c.recordOneOffFailure(ctx, op.ID, err)
		return
	}
	attempts := app.Deploy.StartAttemptLimit()
	deadline := time.Now().Add(budget)
	// verifyCtx ends at the deploy timeout. rollCtx also ends when a cancel is accepted, so the wait for health can be cancelled,
	// and the verification that follows it is not: it is short, and the cancel is settled at the barrier.
	verifyCtx, stop := context.WithDeadline(ctx, deadline)
	defer stop()
	rollCtx, stopRoll := interruptible(verifyCtx)
	defer stopRoll()
	// unwind takes the rollout back after a failure or a cancel, with the roles in touched changed.
	unwind := func(e error, touched []roleWork) {
		// The evidence is read first: compensation deletes a created service together with its tasks and their logs.
		var gaveUp *errStartAttempts
		errors.As(e, &gaveUp)
		outcome.Reason = addStoppedFirst(c.recordRolloutFailure(ctx, op.ID, work, rolloutFallback(e), gaveUp), app, touched)
		recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		recoveryErr := c.compensateRollout(recoveryCtx, op.ID, req.App)
		recoveryCancel()
		if recoveryErr != nil {
			outcome.State = "recovery_required"
			outcome.Err = "rollout recovery required"
			return
		}
		outcome.State = "failed"
		for _, done := range work {
			if done.intent.Kind == "update" {
				outcome.State = "rolled_back"
				break
			}
		}
		outcome.Err = "rollout failed and was cleaned up"
	}
	var releaseBindings []state.Binding
	for n, w := range work {
		w.intent.Deadline = deadline
		id := w.intent.ServiceID
		var e error
		if w.intent.Kind == "create" {
			id, e = createRole(rollCtx, c.Store, c.Engine, op.ID, w.intent, prepared.Auth, w.replicas, attempts)
		} else {
			e = updateRole(rollCtx, c.Store, c.Engine, op.ID, w.intent, prepared.Auth, w.replicas, attempts)
		}
		if e != nil {
			// Only the roles up to this one were touched; the ones after it still run the version they ran before.
			unwind(e, work[:n+1])
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
	settings, err := releaseSettings(req, rollback)
	if err != nil {
		outcome.State = "recovery_required"
		outcome.Err = "release recording failed"
		return
	}
	for _, w := range work {
		live, e := c.Engine.InspectService(verifyCtx, w.intent.Name)
		if e != nil {
			outcome.State = "recovery_required"
			outcome.Err = "final verification unavailable"
			outcome.Reason = c.recordRolloutFailure(ctx, op.ID, work, "final verification unavailable", nil)
			return
		}
		if e = healthyOnce(verifyCtx, c.Engine, live.ID, op.ID, w.replicas); e != nil {
			outcome.State = "recovery_required"
			outcome.Err = "final health verification failed"
			outcome.Reason = c.recordRolloutFailure(ctx, op.ID, work, "final health verification failed", nil)
			return
		}
		if e = waitDrain(verifyCtx, c.Engine, live.ID, op.ID); e != nil {
			outcome.State = "recovery_required"
			outcome.Err = "final drain verification failed"
			outcome.Reason = c.recordRolloutFailure(ctx, op.ID, work, "final drain verification failed", nil)
			return
		}
	}
	outcome = state.Outcome{State: "succeeded", SchemaBefore: before, SchemaAfter: after, Protected: req.Manifest.Protected, Release: &state.Release{App: req.App, Digest: prepared.Image, Tag: p.Tag, Commit: req.Commit, Modified: req.Modified, Manifest: manifestJSON, Bindings: releaseBindings}, Settings: settings}
	barrier, e := json.Marshal(outcome)
	if e != nil {
		outcome.State = "recovery_required"
		outcome.Release = nil
		return
	}
	steps, e := c.Store.Intents(verifyCtx, op.ID)
	if e != nil {
		outcome.State = "recovery_required"
		outcome.Release = nil
		outcome.Err = "verification unavailable"
		return
	}
	if _, e = c.recoverBarrier(verifyCtx, steps, state.Intent{Desired: barrier}); e != nil {
		outcome.State = "recovery_required"
		outcome.Release = nil
		outcome.Err = "verification unavailable"
		return
	}
	// Recording the barrier is the point of no return of a deploy: the release is verified, and the routes and the post-deploy
	// commands follow. A cancel that was accepted before it takes the rollout back instead.
	if e = c.cross(ctx, op.ID, state.Intent{Ordinal: 9999, Kind: "barrier", Desired: barrier, Deadline: deadline}, reasonSwitched); e != nil {
		if isCancelled(e) {
			outcome = state.Outcome{SchemaBefore: before, SchemaAfter: after}
			unwind(e, work)
			return
		}
		outcome.State = "recovery_required"
		outcome.Release = nil
		outcome.Err = "verification barrier unavailable"
		return
	}
	// The publish follows the barrier. A crash during it leaves the operation to recovery, which keeps the release. The
	// next deploy or ingress apply brings any route or record it missed in line.
	publishErr := c.publishIngress(ctx, op.ID, ing, req.ReplaceExisting)
	if !rollback {
		err = c.runPost(ctx, op.ID, prepared)
	} else {
		err = nil
	}
	switch {
	case err != nil && errors.Is(err, ErrOneOffUncertain):
		outcome.State = "recovery_required"
		outcome.Release = nil
		outcome.Err = "post-deploy command outcome uncertain"
		outcome.Reason = c.recordOneOffFailure(ctx, op.ID, err)
	case err != nil:
		outcome.State = "failed"
		outcome.Err = hookFailureText(err, "post")
		outcome.Reason = c.recordOneOffFailure(ctx, op.ID, err)
	case publishErr != nil:
		// The new version runs and its release is recorded. Only its routes are missing.
		outcome.State = "failed"
		outcome.Err = ingressFailure(publishErr, errIngressPublish)
	}
	if outcome.State == "succeeded" {
		// The new version serves and its release is recorded. What the checks find is a warning, never a failure.
		c.verifyDeploy(ctx, op.ID, req.Manifest.NetworkName(c.Environment), app, work, ing != nil)
	}
	return
}

// boundServiceGone reports whether a bound service was removed: its name and its recorded ID are both unknown to Docker.
func (c *Controller) boundServiceGone(ctx context.Context, b state.Binding, nameErr error) (bool, error) {
	var status *engine.StatusError
	if !errors.As(nameErr, &status) || status.Code != 404 {
		return false, nil
	}
	_, err := c.Engine.InspectService(ctx, b.ServiceID)
	if err == nil {
		return false, nil
	}
	if errors.As(err, &status) && status.Code == 404 {
		return true, nil
	}
	return false, err
}

// ensureImage inspects a pinned image and pulls it once when the engine does not hold it.
func (c *Controller) ensureImage(ctx context.Context, p preparedPlan) (engine.Image, string, error) {
	img, err := c.Engine.InspectImage(ctx, p.Image)
	if err == nil {
		return img, "", nil
	}
	var status *engine.StatusError
	if !errors.As(err, &status) || status.Code != 404 {
		return img, "image inspection failed", err
	}
	waitCtx, stop := interruptible(ctx)
	pullCtx, cancel := context.WithTimeout(waitCtx, 2*time.Minute)
	err = c.Engine.PullImage(pullCtx, p.Image, p.Auth)
	cancel()
	stop()
	if err != nil {
		return img, "image pull failed", err
	}
	img, err = c.Engine.InspectImage(ctx, p.Image)
	if err != nil || img.ID == "" {
		return engine.Image{}, "image inspection failed", errors.New("image inspection failed")
	}
	if err = c.Store.RecordImageAcquired(ctx, p.Image, img.ID); err != nil {
		return img, "image provenance recording failed", err
	}
	return img, "", nil
}

func (c *Controller) compensateRollout(ctx context.Context, opID, app string) error {
	intents, err := c.Store.Intents(ctx, opID)
	if err != nil {
		return err
	}
	var combined error
	for n := len(intents) - 1; n >= 0; n-- {
		i := intents[n]
		var e error
		switch i.Kind {
		case "update":
			e = c.restoreUpdate(ctx, app, i)
		case "create":
			e = c.removeCreated(ctx, i)
		}
		combined = errors.Join(combined, e)
	}
	return combined
}
func (c *Controller) removeCreated(ctx context.Context, i state.Intent) error {
	live, e := c.Engine.InspectService(ctx, i.Name)
	if e != nil {
		var status *engine.StatusError
		if errors.As(e, &status) && status.Code == 404 {
			return nil
		}
		return errors.New("recovery inspection failed")
	}
	var receipt struct {
		ID   string          `json:"id"`
		Spec json.RawMessage `json:"spec"`
	}
	if json.Unmarshal(i.Receipt, &receipt) != nil {
		return errors.New("service receipt invalid")
	}
	if receipt.ID != "" {
		if live.ID != receipt.ID || !sameSpec(live.Spec, receipt.Spec) {
			return errors.New("service ownership uncertain")
		}
	} else {
		var owner struct {
			Labels       map[string]string
			TaskTemplate struct {
				ContainerSpec struct {
					Labels map[string]string
					Image  string
				}
			}
		}
		if live.ID == "" || i.Generation == "" || json.Unmarshal(live.Spec, &owner) != nil || owner.Labels["berthy.operation"] != i.Generation || owner.TaskTemplate.ContainerSpec.Labels["berthy.operation"] != i.Generation || !matchesStoredSpec(i.Desired, live.Spec) {
			return errors.New("service ownership uncertain")
		}
	}
	if e = c.Engine.DeleteService(ctx, live.ID); e != nil {
		return errors.New("service cleanup failed")
	}
	_, e = c.Engine.InspectService(ctx, i.Name)
	var status *engine.StatusError
	if !errors.As(e, &status) || status.Code != 404 {
		return errors.New("service cleanup unconfirmed")
	}
	return nil
}
func (c *Controller) restoreUpdate(ctx context.Context, app string, i state.Intent) error {
	live, err := c.Engine.InspectService(ctx, i.Name)
	if err != nil || live.ID != i.ServiceID {
		return errors.New("bound service recovery inspection failed")
	}
	if !sameSpec(live.Spec, i.Before) {
		if !matchesStoredSpec(i.Desired, live.Spec) {
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
			ContainerSpec struct {
				Labels map[string]string
				Image  string
			}
		}
		Mode struct{ Replicated *struct{ Replicas int } }
	}
	if json.Unmarshal(i.Before, &before) != nil || before.Mode.Replicated == nil {
		return errors.New("prior service health unavailable")
	}
	generation := before.TaskTemplate.ContainerSpec.Labels["berthy.operation"]
	if generation == "" {
		ref, err := reference.ParseNormalizedNamed(before.TaskTemplate.ContainerSpec.Image)
		if err != nil {
			return errors.New("prior image identity unavailable")
		}
		if _, ok := ref.(reference.Canonical); !ok {
			return errors.New("prior image is not pinned")
		}
		return waitImageHealthy(ctx, c.Engine, i.ServiceID, before.TaskTemplate.ContainerSpec.Image, before.Mode.Replicated.Replicas)
	}
	// Restoring is not a rollout of a new version: it has no attempt limit.
	return waitHealthy(ctx, c.Engine, i.ServiceID, generation, before.Mode.Replicated.Replicas, 0)
}

// renamed is spec with its Name set to name.
func renamed(spec json.RawMessage, name string) (json.RawMessage, error) {
	var v map[string]any
	if err := json.Unmarshal(spec, &v); err != nil {
		return nil, err
	}
	v["Name"] = name
	return json.Marshal(v)
}
