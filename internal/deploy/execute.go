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
	// Recovery allowance if this role and every earlier role in the rollout need to be unwound.
	recoveryAllowance time.Duration
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

func deployDrainTimeout(app manifest.App) string {
	if app.Deploy.DrainTimeout == "" {
		return "30s"
	}
	return app.Deploy.DrainTimeout
}

// releaseRoleSpec is the Swarm service of a role at the release that prepared describes, as a deploy sends it. operation is
// the generation label of the spec. A deploy and the drift check both build a role's spec here, so what a deploy would send
// and what the check expects are the same.
func (c *Controller) releaseRoleSpec(prepared preparedPlan, img engine.Image, role, operation string) (json.RawMessage, error) {
	req := prepared.Request
	app := req.Manifest.Apps[req.App]
	drain := deployDrainTimeout(app)
	order, _ := app.UpdateOrder(role)
	if err := checkUptimeImage(app, role, img); err != nil {
		return nil, err
	}
	return serviceSpec(c.Environment, req.Manifest.Project, req.App, role, req.Manifest.ServiceName(c.Environment, req.App, role), operation, prepared.NetworkID, prepared.Image, func(v string) string { return req.Manifest.VolumeName(c.Environment, v) }, prepared.containerEnv(role), app.Roles[role], order, drain, img.Healthcheck, prepared.LogDriver, prepared.ProbeSource)
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

func (c *Controller) monitorHold() time.Duration {
	if c.rolloutHold != nil {
		return *c.rolloutHold
	}
	return rolloutMonitor
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
		if errors.As(err, &refusal) && (secretMessage.MatchString(string(refusal)) || networkNameMessage.MatchString(string(refusal)) || isUptimeRefusal(refusal)) {
			outcome.Err = string(refusal)
		}
		return
	}
	if c.Reserve != nil {
		claims, err := c.reserveNames(ctx, req)
		if err != nil {
			var refusal Refusal
			if errors.As(err, &refusal) && PublicMessage(string(refusal)) {
				outcome.Err = string(refusal)
			}
			return
		}
		defer func() {
			settleCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c.dropReservation(settleCtx, claims)
		}()
	}
	planned = target{Image: p.Image, Tag: p.Tag}
	if req.Adopt {
		return c.runAdoption(ctx, op, req, rollback), planned
	}

	d := deployRun{opID: op.ID, rollback: rollback, outcome: outcome}
	if !c.prepareDeployRoles(ctx, &d, req, p) {
		return d.outcome, planned
	}
	if !c.runDeployPre(ctx, &d) {
		return d.outcome, planned
	}
	if !c.rollDeployRoles(ctx, &d) {
		return d.outcome, planned
	}
	verifyCtx, stopVerify := context.WithDeadline(ctx, d.deadline)
	defer stopVerify()
	if !c.verifyDeployRoles(ctx, verifyCtx, &d) {
		return d.outcome, planned
	}
	if !c.crossDeployBarrier(ctx, verifyCtx, &d, p.Tag) {
		return d.outcome, planned
	}
	c.finishDeployRelease(ctx, &d)
	return d.outcome, planned
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

const (
	compensationIntents = "compensation: could not read the operation's intents"
	compensationChanged = "compensation: service changed outside Berthy"
	compensationUpdate  = "compensation: restore update failed"
	compensationHealth  = "compensation: previous release did not become healthy before the deadline"
	compensationRemove  = "compensation: created service could not be removed"
	compensationFailed  = "compensation: failed"
)

func compensationMessages(err error) []string {
	var messages []string
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			messages = append(messages, compensationMessages(e)...)
		}
		return messages
	}
	switch err.Error() {
	case compensationIntents, compensationChanged, compensationUpdate, compensationHealth, compensationRemove:
		return []string{err.Error()}
	default:
		return []string{compensationFailed}
	}
}

func (c *Controller) compensateRollout(ctx context.Context, opID, app string) error {
	intents, err := c.Store.Intents(ctx, opID)
	if err != nil {
		return errors.New(compensationIntents)
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
			if e != nil {
				e = errors.New(compensationRemove)
			}
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
	if err != nil {
		return errors.New("bound service recovery inspection failed")
	}
	if live.ID != i.ServiceID {
		return errors.New(compensationChanged)
	}
	if !sameSpec(live.Spec, i.Before) {
		if !matchesStoredSpec(i.Desired, live.Spec) {
			return errors.New(compensationChanged)
		}
		auth, err := c.operationAuth(ctx, app, i.Before)
		if err != nil {
			return err
		}
		if err = c.Engine.UpdateService(ctx, live.ID, live.Version, i.Before, auth); err != nil {
			return errors.New(compensationUpdate)
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
		err = waitImageHealthy(ctx, c.Engine, i.ServiceID, before.TaskTemplate.ContainerSpec.Image, before.Mode.Replicated.Replicas)
		if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errors.New(compensationHealth)
		}
		return err
	}
	// Restoring is not a rollout of a new version: it has no attempt limit.
	_, err = waitHealthy(ctx, c.Engine, i.ServiceID, generation, before.Mode.Replicated.Replicas, startBudget{}, rolloutTiming{})
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New(compensationHealth)
	}
	return err
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
